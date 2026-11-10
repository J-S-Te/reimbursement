package jobs

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"regexp"
	"strings"
	"time"
)

type Record struct {
	ID, TenantID, Kind, Topic, ResourceID, PayloadJSON, EventID, Status, LeaseOwner string
	Version                                                                         int64
	Attempt                                                                         int
	LeaseUntil                                                                      *time.Time
	NextAttemptAt                                                                   time.Time
	CreatedAt                                                                       time.Time
}
type Queue struct {
	DB    *gorm.DB
	Owner string
	Lease time.Duration
}

// tableAllowed 限制后台任务表，阻断动态标识符注入。
// 参数：table为内部任务表名。
// 返回值：是否为已审查任务表。
func tableAllowed(table string) bool { return table == "exp_job" || table == "exp_outbox" }

// Take 在短事务中领取到期任务或接管失效租约。
// 参数：ctx为数据库取消边界；table仅允许任务与outbox表。
// 返回值：有排他租约的任务；无待办返回nil，数据库失败返回错误。
func (q Queue) Take(ctx context.Context, table string) (*Record, error) {
	if !tableAllowed(table) || q.Owner == "" || q.Lease < time.Second {
		return nil, errors.New("invalid queue configuration")
	}
	var row Record
	err := q.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Table(table).Where("(status = ? AND next_attempt_at <= ?) OR (status = ? AND lease_until < ?)", "PENDING", time.Now().UTC(), "PROCESSING", time.Now().UTC()).Order("next_attempt_at ASC").Order("id ASC")
		if tx.Dialector.Name() == "mysql" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		if err := query.Take(&row).Error; err != nil {
			return err
		}
		until := time.Now().UTC().Add(q.Lease)
		r := tx.Table(table).Where("id = ? AND version = ?", row.ID, row.Version).Updates(map[string]any{"status": "PROCESSING", "lease_owner": q.Owner, "lease_until": until, "version": row.Version + 1, "attempt": row.Attempt + 1})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return errors.New("lease conflict")
		}
		row.Version++
		row.Attempt++
		row.LeaseOwner = q.Owner
		row.LeaseUntil = &until
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// Finish 以租约所有者与版本作CAS，迟到worker不得覆盖接管者结果。
// 参数：ctx为数据库边界；table和row为已领取任务；cause是处理失败原因。
// 返回值：提交或租约失效错误；失败原因仅落稳定诊断码；终态失败与业务审计同事务提交。
func (q Queue) Finish(ctx context.Context, table string, row Record, cause error) error {
	if !tableAllowed(table) {
		return errors.New("invalid queue table")
	}
	changes := map[string]any{"status": "DONE", "lease_owner": "", "lease_until": nil, "last_error": "", "version": row.Version + 1, "updated_at": time.Now().UTC()}
	if cause != nil {
		changes["status"] = "PENDING"
		changes["last_error"] = "DELIVERY_OR_PROCESSING_FAILED"
		code := cause.Error()
		if regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,99}$`).MatchString(code) {
			changes["last_error"] = code
		}
		permanent := strings.HasPrefix(code, "NO_") || strings.HasPrefix(code, "INVALID_") || strings.HasPrefix(code, "UNSAFE_") || strings.HasPrefix(code, "UNSUPPORTED_") || strings.HasPrefix(code, "DOCUMENT_") || strings.HasPrefix(code, "OFD_") || strings.HasPrefix(code, "PDF_") || strings.HasPrefix(code, "MALFORMED_") || strings.HasPrefix(code, "EXPORT_LIMIT_")
		if permanent {
			changes["status"] = "FAILED"
		}
		// 业务处理次数上限不能把尚未可靠记录失败及审计的运行标为终结。
		if row.Attempt >= 12 && code != "PARSE_FAILURE_PERSISTENCE_UNAVAILABLE" {
			changes["status"] = "FAILED"
		}
		exponent := min(row.Attempt, 10)
		var seed [2]byte
		if _, e := rand.Read(seed[:]); e != nil {
			return e
		}
		jitter := time.Duration(binary.BigEndian.Uint16(seed[:])%1000) * time.Millisecond
		changes["next_attempt_at"] = time.Now().UTC().Add(time.Duration(1<<exponent)*time.Second + jitter)
	}
	// 仅终态失败（不可恢复或重试耗尽）留痕，退避重试的每次PENDING不写审计。
	terminalFailure := cause != nil && changes["status"] == "FAILED"
	return q.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r := tx.Table(table).Where("id = ? AND version = ? AND lease_owner = ? AND status = ? AND lease_until > ?", row.ID, row.Version, q.Owner, "PROCESSING", time.Now().UTC()).Updates(changes)
		if r.Error != nil {
			return r.Error
		}
		// CAS 落空说明结果已由接管者或其他worker提交，不能再写一条重复审计。
		if r.RowsAffected != 1 {
			return errors.New("lease no longer owned")
		}
		if !terminalFailure || table != "exp_job" {
			return nil
		}
		reason, _ := changes["last_error"].(string)
		return recordArchivePackageFailure(tx, row, reason)
	})
}

// Run 顺序处理可靠队列，耗时操作在领取事务之外执行。
// 参数：ctx为进程生命周期；table为内部队列；handler有独立任务超时。
// 返回值：进程取消或不可恢复数据库错误。
func (q Queue) Run(ctx context.Context, table string, handler func(context.Context, Record) error) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		row, err := q.Take(ctx, table)
		if err != nil {
			return err
		}
		if row == nil {
			continue
		}
		taskCtx, cancel := context.WithTimeout(ctx, q.Lease/2)
		cause := handler(taskCtx, *row)
		cancel()
		if err = q.Finish(ctx, table, *row, cause); err != nil {
			return err
		}
	}
}

// withLease 将最终业务结果与当前任务租约复核放在同一短事务。
// 参数：ctx为数据库边界；row为领取凭证；apply只执行本地结果落库。
// 返回值：旧worker或过期租约不能提交任何业务结果。
func (p *Processor) withLease(ctx context.Context, row Record, apply func(*gorm.DB) error) error {
	return p.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current struct{ ID string }
		query := tx.Table("exp_job").Where("id = ? AND tenant_id = ? AND version = ? AND lease_owner = ? AND status = ? AND lease_until > ?", row.ID, row.TenantID, row.Version, row.LeaseOwner, "PROCESSING", time.Now().UTC())
		if tx.Dialector.Name() == "mysql" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.Take(&current).Error; err != nil {
			return errors.New("JOB_LEASE_LOST")
		}
		return apply(tx)
	})
}
