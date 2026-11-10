package application

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type ParseLease struct {
	JobID, Owner     string
	Version          int64
	CandidateIndexes []int
}

// CompleteParseLeased 在同一事务复核任务租约并提交原始结果和人工候选。
// 参数：ctx为任务边界；tenant/documentID为固定文档；candidates/issues/raw为解析结果；lease为领取凭证。
// 返回值：租约失效时不写任何候选或来源，持久化失败整体回滚。
func (s *Service) CompleteParseLeased(ctx context.Context, tenant, documentID string, candidates []domain.Invoice, issues []string, raw json.RawMessage, lease ParseLease) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := lockParseJob(tx, tenant, documentID, lease)
		if err != nil {
			return err
		}
		var source store.Document
		if err := tx.Where("tenant_id = ? AND id = ?", tenant, documentID).Take(&source).Error; err != nil {
			return err
		}
		run, err := ensureParseRun(tx, job, source)
		if err != nil {
			return err
		}
		if run.Status == "FAILED" || run.Status == "ABANDONED" {
			return domain.Fail("PARSE_RUN_TERMINATED", "该解析尝试已经终结", 409)
		}
		if run.Status != "RUNNING" {
			return nil
		}
		nested := *s
		nested.db = tx
		if err := nested.CompleteParseResult(ctx, tenant, documentID, candidates, issues, raw); err != nil {
			return err
		}
		var doc store.Document
		if err := tx.Where("tenant_id = ? AND id = ?", tenant, documentID).Take(&doc).Error; err != nil {
			return err
		}
		if err := persistParseResult(tx, job, doc, raw, lease.CandidateIndexes); err != nil {
			return err
		}
		result := tx.Model(&store.Job{}).Where("id = ? AND tenant_id = ? AND version = ? AND lease_owner = ?", job.ID, tenant, lease.Version, lease.Owner).Update("payload_json", string(raw))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return domain.Fail("PARSE_LEASE_LOST", "解析任务已改变", 409)
		}
		return AppendWorkerAudit(tx, tenant, documentID, "invoice.parse.completed", job.ID, "SUCCESS", "")
	})
}
