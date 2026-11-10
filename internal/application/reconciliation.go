package application

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

type ReconciliationItem struct {
	ClaimID            string    `json:"claim_id"`
	ProjectionRevision uint64    `json:"projection_revision"`
	SnapshotHash       string    `json:"snapshot_hash"`
	Currency           string    `json:"currency"`
	PaidNetMinor       string    `json:"paid_net_minor"`
	UpdatedAt          time.Time `json:"updated_at"`
	Consistent         bool      `json:"consistent"`
}
type ReconciliationPage struct {
	Items      []ReconciliationItem `json:"items"`
	NextCursor string               `json:"next_cursor"`
}
type ProjectReconciler interface {
	Reconcile(context.Context, string, string, int) (ReconciliationPage, error)
}

// ReconciliationTenants 分页发现实际源端租户，不依赖额外手填租户清单。
// 参数：ctx 为数据库边界，after 为上一租户，size 为每页上限。
// 返回值：当前报销库有单据的租户及错误。
func (s *Service) ReconciliationTenants(ctx context.Context, after string, size int) ([]string, error) {
	if size < 1 || size > 100 {
		size = 20
	}
	var out []string
	e := s.db.WithContext(ctx).Model(&store.Claim{}).Distinct("tenant_id").Where("tenant_id > ?", after).Order("tenant_id").Limit(size).Pluck("tenant_id", &out).Error
	return out, e
}

// ReconcileStep 执行单租户有界对账页，网络失败不推进已持久游标。
// 参数：ctx 为取消边界，tenant 为可信本地租户，remote 为隔离机器适配器，size 为页大小。
// 返回值：是否推进；每日增量、每七天全量，差异修复须独立管理命令。
func (s *Service) ReconcileStep(ctx context.Context, tenant string, remote ProjectReconciler, size int) (bool, error) {
	if tenant == "" || remote == nil {
		return false, domain.Fail("RECONCILIATION_UNAVAILABLE", "对账租户或依赖缺失", 503)
	}
	if size < 1 || size > 100 {
		size = 20
	}
	db := s.db.WithContext(ctx)
	cursor, e := s.reconciliationRun(db, tenant)
	if e != nil {
		return false, e
	}
	if cursor.Phase == "IDLE" {
		return false, nil
	}
	var page ReconciliationPage
	if cursor.Phase == "REMOTE" {
		page, e = remote.Reconcile(ctx, tenant, cursor.RemoteCursor, size)
		if e != nil {
			return false, e
		}
		if len(page.Items) > size || len(page.NextCursor) > 512 || (page.NextCursor != "" && page.NextCursor == cursor.RemoteCursor) {
			return false, domain.Fail("RECONCILIATION_RESPONSE_INVALID", "项目对账分页无效", 503)
		}
	}
	e = localTransaction(ctx, db, func(tx *gorm.DB) error {
		var current store.ReconciliationCursor
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND version = ?", tenant, cursor.Version).First(&current).Error; err != nil {
			return err
		}
		if current.Phase == "REMOTE" {
			for _, item := range page.Items {
				if item.ClaimID == "" || item.ProjectionRevision == 0 || len(item.SnapshotHash) != 64 {
					return domain.Fail("RECONCILIATION_RESPONSE_INVALID", "项目对账记录无效", 503)
				}
				if err := s.observeRemote(tx, current, item); err != nil {
					return err
				}
			}
			current.RemoteCursor = page.NextCursor
			if page.NextCursor == "" {
				current.Phase = "SOURCE"
			}
		} else {
			if err := s.scanSourcePage(tx, &current, size); err != nil {
				return err
			}
		}
		current.LastError = ""
		now := time.Now().UTC()
		current.LastAttemptAt = &now
		current.Version++
		current.UpdatedAt = time.Now().UTC()
		r := tx.Model(&store.ReconciliationCursor{}).Where("tenant_id = ? AND version = ?", tenant, cursor.Version).Select("*").Updates(&current)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return domain.Fail("VERSION_CONFLICT", "对账游标已更新", 409)
		}
		return nil
	})
	return e == nil, e
}

// reconciliationRun 初始化或恢复持久运行，保留日增量高水位。
// 参数：db 为数据库，tenant 为本地租户。
// 返回值：当前游标或初始化错误。
func (s *Service) reconciliationRun(db *gorm.DB, tenant string) (store.ReconciliationCursor, error) {
	now := time.Now().UTC()
	epoch := time.Unix(0, 0).UTC()
	seed := store.ReconciliationCursor{TenantID: tenant, Phase: "IDLE", Version: 1, SourceUpdatedAt: epoch, Watermark: epoch, RunStartedAt: epoch}
	if e := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; e != nil {
		return seed, e
	}
	var row store.ReconciliationCursor
	if e := db.Where("tenant_id = ?", tenant).First(&row).Error; e != nil {
		return row, e
	}
	if row.Phase != "IDLE" || (row.LastCompletedAt != nil && now.Sub(*row.LastCompletedAt) < 24*time.Hour) {
		return row, nil
	}
	run, e := newID()
	if e != nil {
		return row, e
	}
	mode := "INCREMENTAL"
	source := row.Watermark
	if row.LastFullAt == nil || now.Sub(*row.LastFullAt) >= 7*24*time.Hour {
		mode = "FULL"
		source = epoch
	}
	r := db.Model(&store.ReconciliationCursor{}).Where("tenant_id = ? AND version = ?", tenant, row.Version).Updates(map[string]any{"run_id": run, "mode": mode, "phase": "REMOTE", "remote_cursor": "", "source_updated_at": source, "source_claim_id": "", "run_started_at": now, "version": row.Version + 1})
	if r.Error != nil {
		return row, r.Error
	}
	if r.RowsAffected != 1 {
		return row, domain.Fail("VERSION_CONFLICT", "对账已由其他任务启动", 409)
	}
	e = db.Where("tenant_id = ?", tenant).First(&row).Error
	return row, e
}

// observeRemote 比较远端权威游标与当前源端，不向项目写入任何效果。
// 参数：tx 为事务，cursor 为本次运行，item 为已校验项目数据。
// 返回值：差异索引持久化结果。
func (s *Service) observeRemote(tx *gorm.DB, cursor store.ReconciliationCursor, item ReconciliationItem) error {
	finding := store.ReconciliationFinding{TenantID: cursor.TenantID, ClaimID: item.ClaimID, RunID: cursor.RunID, RemoteRevision: item.ProjectionRevision, RemoteHash: item.SnapshotHash, RemoteConsistent: item.Consistent}
	return s.saveFinding(tx, &finding)
}

// scanSourcePage 按更新时间和单据标识扫描完整权威版本，发现漏投。
// 参数：tx 为事务，cursor 为当前持久运行，size 为有界页大小。
// 返回值：查询与持久化错误，不自动重写任何业务历史。
func (s *Service) scanSourcePage(tx *gorm.DB, cursor *store.ReconciliationCursor, size int) error {
	var rows []store.Claim
	e := tx.Where("tenant_id = ? AND updated_at <= ? AND (updated_at > ? OR (updated_at = ? AND id > ?))", cursor.TenantID, cursor.RunStartedAt, cursor.SourceUpdatedAt, cursor.SourceUpdatedAt, cursor.SourceClaimID).Order("updated_at, id").Limit(size).Find(&rows).Error
	if e != nil {
		return e
	}
	for _, row := range rows {
		var c domain.Claim
		if e = json.Unmarshal([]byte(row.ContentJSON), &c); e != nil {
			return e
		}
		if c.ProjectionRevision > 0 {
			var finding store.ReconciliationFinding
			e = tx.Where("tenant_id = ? AND claim_id = ?", cursor.TenantID, c.ID).First(&finding).Error
			if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
				return e
			}
			if finding.RunID != cursor.RunID {
				finding = store.ReconciliationFinding{TenantID: cursor.TenantID, ClaimID: c.ID, RunID: cursor.RunID}
			}
			if e = s.saveFinding(tx, &finding); e != nil {
				return e
			}
		}
		cursor.SourceUpdatedAt = row.UpdatedAt
		cursor.SourceClaimID = row.ID
	}
	if len(rows) < size {
		now := time.Now().UTC()
		cursor.Phase = "IDLE"
		cursor.Watermark = cursor.RunStartedAt
		cursor.LastCompletedAt = &now
		if cursor.Mode == "FULL" {
			cursor.LastFullAt = &now
		}
	}
	return nil
}

// authoritativeSnapshot 查找与当前财务版本精确一致的已冻结完整快照。
// 参数：tx 为数据库，tenant/claim 为来源边界，revision 为当前版本。
// 返回值：权威快照；缺失或摘要无效时明确阻断。
func authoritativeSnapshot(tx *gorm.DB, tenant, claim string, revision uint64) (domain.SpendSnapshot, error) {
	row, e := snapshotOutbox(tx, tenant, claim, revision)
	if e != nil {
		return domain.SpendSnapshot{}, e
	}
	var snap domain.SpendSnapshot
	e = json.Unmarshal([]byte(row.PayloadJSON), &snap)
	return snap, e
}

// snapshotOutbox 定位当前财务版本对应的唯一原始权威快照队列。
// 参数：tx 为数据库，tenant/claim 为来源边界，revision 为当前财务版本。
// 返回值：原队列；重复同版冲突、内容无效或缺失时拒绝恢复。
func snapshotOutbox(tx *gorm.DB, tenant, claim string, revision uint64) (store.Outbox, error) {
	var rows []store.Outbox
	if e := tx.Where("tenant_id = ? AND resource_id = ? AND topic IN ?", tenant, claim, []string{"project", "project_spend"}).Order("created_at DESC, id DESC").Limit(2001).Find(&rows).Error; e != nil {
		return store.Outbox{}, e
	}
	if len(rows) > 2000 {
		return store.Outbox{}, domain.Fail("SYNC_HISTORY_LIMIT", "同步历史超过恢复校验上限", 422)
	}
	var selected store.Outbox
	hash := ""
	for _, row := range rows {
		var snap domain.SpendSnapshot
		if e := json.Unmarshal([]byte(row.PayloadJSON), &snap); e != nil {
			return row, e
		}
		if snap.ProjectionRevision != revision {
			continue
		}
		if snap.TenantID != tenant || snap.ClaimID != claim || domain.ValidateSpendSnapshot(snap) != nil {
			return row, domain.Fail("AUTHORITATIVE_SNAPSHOT_INVALID", "权威快照校验失败", 409)
		}
		if hash != "" && hash != snap.SnapshotHash {
			return row, domain.Fail("HISTORY_CONFLICT", "同财务版本存在不同权威摘要", 409)
		}
		hash = snap.SnapshotHash
		if selected.ID == "" {
			selected = row
		}
	}
	if selected.ID == "" {
		return selected, domain.Fail("AUTHORITATIVE_SNAPSHOT_MISSING", "当前财务版本权威快照缺失", 409)
	}
	return selected, nil
}

// saveFinding 生成当前差异状态，修复历史仍保留在恢复命令与项目审计中。
// 参数：tx 为事务，finding 为当前运行观察值。
// 返回值：当前差异索引持久化错误。
func (s *Service) saveFinding(tx *gorm.DB, finding *store.ReconciliationFinding) error {
	c, _, e := loadClaim(tx, finding.TenantID, finding.ClaimID, false)
	if e != nil {
		var p *domain.Problem
		if !errors.As(e, &p) || p.Code != "RESOURCE_NOT_FOUND" {
			return e
		}
		finding.Status = "SOURCE_MISSING"
	} else {
		finding.SourceRevision = uint64(c.ProjectionRevision)
		snapshot, err := authoritativeSnapshot(tx, c.TenantID, c.ID, uint64(c.ProjectionRevision))
		switch {
		case err != nil:
			var p *domain.Problem
			if !errors.As(err, &p) {
				return err
			}
			finding.Status = p.Code
		case finding.RemoteRevision == 0:
			finding.SourceHash = snapshot.SnapshotHash
			finding.Status = "REMOTE_MISSING"
		default:
			finding.SourceHash = snapshot.SnapshotHash
			finding.Status = "MATCHED"
			if finding.RemoteRevision < finding.SourceRevision {
				finding.Status = "REMOTE_BEHIND"
			} else if finding.RemoteRevision > finding.SourceRevision || finding.RemoteHash != finding.SourceHash {
				finding.Status = "HISTORY_CONFLICT"
			} else if !finding.RemoteConsistent {
				finding.Status = "PROJECTION_CORRUPT"
			}
		}
	}
	finding.ID, e = digest([]string{finding.TenantID, finding.ClaimID})
	if e != nil {
		return e
	}
	finding.Version = 1
	finding.UpdatedAt = time.Now().UTC()
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}, {Name: "claim_id"}}, DoUpdates: clause.Assignments(map[string]any{"run_id": finding.RunID, "source_revision": finding.SourceRevision, "source_hash": finding.SourceHash, "remote_revision": finding.RemoteRevision, "remote_hash": finding.RemoteHash, "remote_consistent": finding.RemoteConsistent, "status": finding.Status, "version": gorm.Expr("version + 1"), "updated_at": finding.UpdatedAt})}).Create(finding).Error
}

// RecordReconciliationFailure 保存可观测失败码而不移动对账游标。
// 参数：ctx 为持久化边界，tenant 为源租户，cause 为网络或协议错误。
// 返回值：诊断落库错误，调用方不得静默忽略。
func (s *Service) RecordReconciliationFailure(ctx context.Context, tenant string, cause error) error {
	code := "PROJECT_RECONCILIATION_UNAVAILABLE"
	if cause != nil && workerReasonCode.MatchString(cause.Error()) {
		code = cause.Error()
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Model(&store.ReconciliationCursor{}).Where("tenant_id = ?", tenant).Updates(map[string]any{"last_error": code, "last_attempt_at": now}).Error
}
