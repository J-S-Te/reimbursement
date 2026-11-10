package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// freezeInvoiceArchiveEvidence 固定本单票据的来源ID与更正风险状态，不依赖时间戳猜测集合。
// 参数：tx 为持有单据锁的事务，c 为归档请求所对应的单据。
// 返回值：有界、可独立复核的票据证据；不截断超限历史。
func freezeInvoiceArchiveEvidence(tx *gorm.DB, c domain.Claim) (domain.ArchivedInvoiceEvidence, error) {
	out := domain.ArchivedInvoiceEvidence{SourceIDs: []string{}, Controls: []domain.ArchivedInvoiceControl{}}
	ids := invoiceIDs(c)
	if len(ids) == 0 {
		return out, nil
	}
	if err := tx.Model(&store.InvoiceSource{}).Where("tenant_id = ? AND invoice_id IN ?", c.TenantID, ids).Order("id").Limit(2001).Pluck("id", &out.SourceIDs).Error; err != nil {
		return out, err
	}
	if len(out.SourceIDs) > 2000 {
		return out, domain.Fail("ARCHIVE_HISTORY_LIMIT", "票据来源超出单包受控上限", 422)
	}
	q := tx.Where("tenant_id = ? AND invoice_id IN ?", c.TenantID, ids)
	corrections, err := freezeInvoiceControlRows(q, "CORRECTION", func(r store.InvoiceCorrection) string { return r.ContentJSON })
	if err != nil {
		return out, err
	}
	risks, err := freezeInvoiceControlRows(tx.Where("tenant_id = ? AND (invoice_id IN ? OR original_invoice_id IN ?)", c.TenantID, ids, ids), "RISK", func(r store.InvoiceRisk) string { return r.ContentJSON })
	if err != nil {
		return out, err
	}
	impacts, err := freezeInvoiceControlRows(tx.Where("tenant_id = ? AND claim_id = ?", c.TenantID, c.ID), "IMPACT", func(r store.InvoiceControlImpact) string { return r.ContentJSON })
	if err != nil {
		return out, err
	}
	out.Controls = append(out.Controls, corrections...)
	out.Controls = append(out.Controls, risks...)
	out.Controls = append(out.Controls, impacts...)
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if len(raw) > 2<<20 {
		return out, domain.Fail("ARCHIVE_HISTORY_LIMIT", "票据控制证据超出单包受控上限", 422)
	}
	return out, nil
}

// freezeInvoiceControlRows 在读取大JSON前约束总量，并将案件元数据与内容分别固定。
// 参数：q 为带租户与对象限制的查询，kind 为固定类别，content 为内容提取函数。
// 返回值：冻结案件记录；超过条数或字节边界时显式拒绝。
func freezeInvoiceControlRows[T any](q *gorm.DB, kind string, content func(T) string) ([]domain.ArchivedInvoiceControl, error) {
	var size int64
	if err := q.Session(&gorm.Session{}).Model(new(T)).Select("COALESCE(SUM(LENGTH(content_json)),0)").Row().Scan(&size); err != nil {
		return nil, err
	}
	if size > 2<<20 {
		return nil, domain.Fail("ARCHIVE_HISTORY_LIMIT", "票据案件内容超出归档上限", 422)
	}
	var rows []T
	if err := q.Session(&gorm.Session{}).Model(new(T)).Select("*").Order("id").Limit(1001).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > 1000 {
		return nil, domain.Fail("ARCHIVE_HISTORY_LIMIT", "票据案件数量超出归档上限", 422)
	}
	out := []domain.ArchivedInvoiceControl{}
	for _, row := range rows {
		raw := content(row)
		if !json.Valid([]byte(raw)) {
			return nil, domain.Fail("ARCHIVE_EVIDENCE_INVALID", "票据证据内容不可读", 409)
		}
		metadata, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.ArchivedInvoiceControl{Kind: kind, Record: metadata, Content: json.RawMessage(raw)})
	}
	return out, nil
}

// appendInvoiceEvidenceArchives 在同一事务中为已发生资金事实的关联单追加档案。
// 参数：tx 为持有相关单据锁的事务，claims 为本次控制命令影响的单据。
// 返回值：追加任务错误；不修改付款效果、财务版本或原封存包。
func appendInvoiceEvidenceArchives(tx *gorm.DB, claims ...*domain.Claim) error {
	seen := map[string]bool{}
	for _, claim := range claims {
		if claim == nil || seen[claim.ID] {
			continue
		}
		seen[claim.ID] = true
		if err := appendFinancialHistoryArchive(tx, *claim); err != nil {
			return err
		}
	}
	return nil
}
