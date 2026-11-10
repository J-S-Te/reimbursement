package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ApprovalReminderOptions struct {
	OverdueAfter time.Duration
	RepeatEvery  time.Duration
	PollEvery    time.Duration
	BatchSize    int
}

// normalized 补足催办默认值并限制单次扫描资源。
// 参数：无。
// 返回值：默认48小时超期、24小时间隔、每分钟扫描50条的配置或非法值错误。
func (o ApprovalReminderOptions) normalized() (ApprovalReminderOptions, error) {
	if o.OverdueAfter == 0 {
		o.OverdueAfter = 48 * time.Hour
	}
	if o.RepeatEvery == 0 {
		o.RepeatEvery = 24 * time.Hour
	}
	if o.PollEvery == 0 {
		o.PollEvery = time.Minute
	}
	if o.BatchSize == 0 {
		o.BatchSize = 50
	}
	if o.OverdueAfter < time.Second || o.RepeatEvery < time.Second ||
		o.PollEvery < time.Second || o.BatchSize < 1 || o.BatchSize > 100 {
		return o, errors.New("INVALID_APPROVAL_REMINDER_OPTIONS")
	}
	return o, nil
}

// RunApprovalReminders 按有限批次循环扫描当前有效审批任务，重启或多实例不重复入队。
// 参数：ctx为worker生命周期，options为超期、重复间隔、扫描周期及批次配置。
// 返回值：取消、配置或持久化错误；目录和许可暂不可用时下轮重试。
func (p *Processor) RunApprovalReminders(ctx context.Context, options ApprovalReminderOptions) error {
	opts, err := options.normalized()
	if err != nil {
		return err
	}
	if p.DB == nil || p.Service == nil || p.License == nil {
		return errors.New("APPROVAL_REMINDER_UNAVAILABLE")
	}
	ticker := time.NewTicker(opts.PollEvery)
	defer ticker.Stop()
	after := ""
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			next, _, batchErr := p.ApprovalReminderBatch(ctx, now.UTC(), opts.OverdueAfter,
				opts.RepeatEvery, opts.BatchSize, after)
			if batchErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				var problem *domain.Problem
				if (errors.As(batchErr, &problem) && problem.Status >= 500) ||
					batchErr.Error() == "LICENSE_UNAVAILABLE" ||
					batchErr.Error() == "APPROVAL_REMINDER_QUALIFICATION_STALE" {
					// 目录故障不生成事件；游标留在原页，恢复后重新核验资格。
					continue
				}
				return batchErr
			}
			after = next
		}
	}
}

// approvalReminderCandidates 只读扫描到期任务，跨租户连接始终绑定同租户单据。
// 参数：ctx为取消边界，now/delay为到期边界，limit/after为有限游标分页。
// 返回值：按ID排序的候选任务及数据库错误；真正状态在写事务再次复核。
func (p *Processor) approvalReminderCandidates(ctx context.Context, now time.Time,
	delay time.Duration, limit int, after string) ([]store.Task, error) {
	var rows []store.Task
	cutoff := now.Add(-delay)
	err := p.DB.WithContext(ctx).Table("exp_approval_task AS t").Select("t.*").
		Joins("JOIN exp_claim AS c ON c.id = t.claim_id AND c.tenant_id = t.tenant_id").
		Where("t.status = ? AND c.status = ? AND t.id > ?", "PENDING", "IN_APPROVAL", after).
		Where("t.created_at <= ? AND t.updated_at <= ?", cutoff, cutoff).
		Order("t.id ASC").Limit(limit).Find(&rows).Error
	return rows, err
}

// ApprovalReminderBatch 在当前资格复核后逐项事务入队，不将历史权限作为新催办授权。
// 参数：ctx为边界，now为扫描时间，delay/repeat为超期和重复间隔，limit/after为分页。
// 返回值：下一页游标、本批新增事件数及错误；同桶已有事件不会重复计数。
func (p *Processor) ApprovalReminderBatch(ctx context.Context, now time.Time,
	delay, repeat time.Duration, limit int, after string) (string, int, error) {
	opts, err := (ApprovalReminderOptions{OverdueAfter: delay, RepeatEvery: repeat,
		BatchSize: limit}).normalized()
	if err != nil || len(after) > 64 || now.IsZero() {
		return after, 0, errors.New("INVALID_APPROVAL_REMINDER_OPTIONS")
	}
	if p.DB == nil || p.Service == nil || p.License == nil {
		return after, 0, errors.New("APPROVAL_REMINDER_UNAVAILABLE")
	}
	if err = p.License.Check(ctx, core.MUTATE_BUSINESS); err != nil {
		return after, 0, errors.New("LICENSE_UNAVAILABLE")
	}
	rows, err := p.approvalReminderCandidates(ctx, now, opts.OverdueAfter, opts.BatchSize, after)
	if err != nil {
		return after, 0, err
	}
	queued := 0
	for _, row := range rows {
		added, e := p.prepareApprovalReminder(ctx, row, now, opts)
		if e != nil {
			return after, queued, e
		}
		queued += added
	}
	if len(rows) < opts.BatchSize {
		return "", queued, nil
	}
	return rows[len(rows)-1].ID, queued, nil
}

// prepareApprovalReminder 在事务外复核人员和责任，失效任务跳过，依赖故障不丢游标。
// 参数：ctx为调用边界，row为扫描任务，now/opts为催办时点和规则。
// 返回值：新增事件数及依赖或持久化错误。
func (p *Processor) prepareApprovalReminder(ctx context.Context, row store.Task,
	now time.Time, opts ApprovalReminderOptions) (int, error) {
	var claim store.Claim
	if err := p.DB.WithContext(ctx).Where("tenant_id = ? AND id = ?", row.TenantID,
		row.ClaimID).First(&claim).Error; err != nil {
		return 0, err
	}
	started := time.Now().UTC()
	err := p.Service.VerifyApprovalReminder(ctx, row.TenantID, row.ID, row.Version, claim.Version)
	if err != nil {
		var problem *domain.Problem
		if errors.As(err, &problem) && problem.Status < 500 {
			return 0, nil
		}
		return 0, err
	}
	if time.Since(started) > time.Minute {
		return 0, errors.New("APPROVAL_REMINDER_QUALIFICATION_STALE")
	}
	return p.persistApprovalReminder(ctx, row, claim, now, opts)
}

// reminderEvent 以任务、轮次、当前激活时点及相对时间桶派生稳定事件与收件人。
// 参数：task为写事务内的当前任务，now/opts为扫描时点及催办规则。
// 返回值：冻结待投递通知或尚未到期的空值。
func reminderEvent(task store.Task, now time.Time, opts ApprovalReminderOptions) *store.Outbox {
	activated := task.CreatedAt
	if task.UpdatedAt.After(activated) {
		activated = task.UpdatedAt
	}
	if activated.IsZero() || now.Before(activated.Add(opts.OverdueAfter)) {
		return nil
	}
	bucket := int64(now.Sub(activated.Add(opts.OverdueAfter)) / opts.RepeatEvery)
	key, _ := json.Marshal([]any{"approval-reminder-v1", task.TenantID, task.ID,
		task.Round, activated.UTC().Format(time.RFC3339Nano), int64(opts.OverdueAfter),
		int64(opts.RepeatEvery), bucket})
	hash := sha256.Sum256(key)
	id := hex.EncodeToString(hash[:])
	payload, _ := json.Marshal(map[string]any{"kind": "APPROVAL_REMINDER", "task_id": task.ID,
		"claim_id": task.ClaimID, "round": task.Round, "assignee_subject_id": task.Assignee,
		"reminder_bucket": bucket, "activated_at": activated.UTC().Format(time.RFC3339Nano)})
	return &store.Outbox{ID: id, EventID: id, TenantID: task.TenantID, Topic: "notification",
		ResourceID: task.ClaimID, PayloadJSON: string(payload), Status: "PENDING", Version: 1,
		NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
}

// persistApprovalReminder 按业务锁序重核任务再以唯一事件入队，完成任务不会生成新提醒。
// 参数：ctx为事务边界，expected/claim为资格复核版本，now/opts为规则。
// 返回值：新增事件数；多实例冲突按既有事件处理，不重复投递。
func (p *Processor) persistApprovalReminder(ctx context.Context, expected store.Task,
	claim store.Claim, now time.Time, opts ApprovalReminderOptions) (int, error) {
	queued := 0
	err := p.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var lockedClaim store.Claim
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?",
			claim.TenantID, claim.ID).First(&lockedClaim).Error; e != nil {
			return e
		}
		var c domain.Claim
		if e := json.Unmarshal([]byte(lockedClaim.ContentJSON), &c); e != nil {
			return e
		}
		var task store.Task
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?",
			expected.TenantID, expected.ID).First(&task).Error; e != nil {
			return e
		}
		if lockedClaim.Status != "IN_APPROVAL" || lockedClaim.Version != claim.Version ||
			task.Status != "PENDING" || task.Version != expected.Version || task.Assignee != expected.Assignee ||
			task.ClaimID != claim.ID || task.Round != c.CurrentRound || task.Assignee == c.Applicant {
			return nil
		}
		event := reminderEvent(task, now, opts)
		if event == nil {
			return nil
		}
		if e := p.License.Check(ctx, core.MUTATE_BUSINESS); e != nil {
			return errors.New("LICENSE_UNAVAILABLE")
		}
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_id"}},
			DoNothing: true}).Create(event)
		if result.Error == nil {
			queued = int(result.RowsAffected)
		}
		return result.Error
	})
	return queued, err
}

// ApprovalReminderDeliverable 在真实通知出站前重核当前任务和同收件人资格。
// 参数：ctx为出站边界，row为已领取outbox租约，data为其冻结载荷。
// 返回值：已完成、换人或撤权返回false且无错误；目录故障返回错误以保留重试。
func (p *Processor) ApprovalReminderDeliverable(ctx context.Context, row Record,
	data map[string]any) (bool, error) {
	if data["kind"] != "APPROVAL_REMINDER" {
		return true, nil
	}
	if p.DB == nil || p.Service == nil {
		return false, errors.New("APPROVAL_REMINDER_UNAVAILABLE")
	}
	var payload struct {
		TaskID    string `json:"task_id"`
		ClaimID   string `json:"claim_id"`
		Recipient string `json:"assignee_subject_id"`
		Round     int    `json:"round"`
	}
	raw, err := json.Marshal(data)
	if err != nil || json.Unmarshal(raw, &payload) != nil || payload.TaskID == "" ||
		payload.ClaimID != row.ResourceID || payload.Recipient == "" || payload.Round < 1 {
		return false, errors.New("INVALID_APPROVAL_REMINDER_PAYLOAD")
	}
	var task store.Task
	if err = p.DB.WithContext(ctx).Where("tenant_id = ? AND id = ?", row.TenantID,
		payload.TaskID).First(&task).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if task.Status != "PENDING" || task.Assignee != payload.Recipient ||
		task.ClaimID != row.ResourceID || task.Round != payload.Round {
		return false, nil
	}
	var claim store.Claim
	if err = p.DB.WithContext(ctx).Where("tenant_id = ? AND id = ?", row.TenantID,
		task.ClaimID).First(&claim).Error; err != nil {
		return false, err
	}
	err = p.Service.VerifyApprovalReminder(ctx, row.TenantID, task.ID, task.Version, claim.Version)
	var problem *domain.Problem
	if errors.As(err, &problem) && problem.Status < 500 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return p.reminderDeliveryStateUnchanged(ctx, task, claim)
}

// reminderDeliveryStateUnchanged 在人员及本地范围复核返回后再次比对最初出站坐标。
// 参数：ctx为读取边界，expected/claim为本次投递最初读取的任务和单据。
// 返回值：任务完成、换人或版本轮次改变时为false；数据库故障返回错误保留重试。
func (p *Processor) reminderDeliveryStateUnchanged(ctx context.Context, expected store.Task,
	claim store.Claim) (bool, error) {
	var current store.Task
	if err := p.DB.WithContext(ctx).Where("tenant_id = ? AND id = ?", expected.TenantID,
		expected.ID).First(&current).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var latest store.Claim
	if err := p.DB.WithContext(ctx).Where("tenant_id = ? AND id = ?", claim.TenantID,
		claim.ID).First(&latest).Error; err != nil {
		return false, err
	}
	return current.Status == "PENDING" && current.Version == expected.Version &&
		current.Assignee == expected.Assignee && current.Round == expected.Round &&
		current.ClaimID == claim.ID && current.ContentJSON == expected.ContentJSON &&
		latest.Status == "IN_APPROVAL" && latest.Version == claim.Version &&
		latest.ContentJSON == claim.ContentJSON, nil
}
