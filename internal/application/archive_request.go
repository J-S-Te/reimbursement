package application

import (
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// archiveHistoryReferences 冻结请求时已存在的不可变历史ID，避免毫秒时间边界混入未来事实。
// 参数：tx 为持有claim锁的事务，c 为当前单据。
// 返回值：各历史类别的有界ID集合或超限错误。
func archiveHistoryReferences(tx *gorm.DB, c domain.Claim) (map[string]any, error) {
	sets := []struct {
		Key   string
		Model any
	}{{"exception_fact_ids", &store.PaymentExceptionFact{}}, {"exception_review_ids", &store.PaymentExceptionReview{}}, {"claim_action_ids", &store.Action{}}, {"approval_action_detail_ids", &store.ActionDetail{}}}
	out := map[string]any{}
	for _, set := range sets {
		ids := []string{}
		if e := tx.Model(set.Model).Where("tenant_id = ? AND claim_id = ?", c.TenantID, c.ID).Order("id").Limit(2001).Pluck("id", &ids).Error; e != nil {
			return nil, e
		}
		if len(ids) > 2000 {
			return nil, domain.Fail("ARCHIVE_HISTORY_LIMIT", "档案不可变历史引用超过单包上限，请受控处理", 422)
		}
		out[set.Key] = ids
	}
	evidence, err := freezeInvoiceArchiveEvidence(tx, c)
	if err != nil {
		return nil, err
	}
	out["invoice_evidence"] = evidence
	return out, nil
}

// appendFinancialHistoryArchive 仅在已有真实财务投影的单据追加非资金变动档案。
// 参数：tx 为当前业务事务，c 为持锁单据。
// 返回值：任务写入错误；从不新增付款效果或项目revision。
func appendFinancialHistoryArchive(tx *gorm.DB, c domain.Claim) error {
	if c.ProjectionRevision == 0 {
		return nil
	}
	return createArchiveJob(tx, c)
}
