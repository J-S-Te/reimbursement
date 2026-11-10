package application

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// QueryInvoiceControls 在票据当前访问权下返回更正、风险和来源，隐藏无权单据影响。
// 参数：ctx 为请求边界，a 为当前主体，invoiceID 为票据。
// 返回值：有界控制历史与票面建议；拒绝无权对象或历史超限。
func (s *Service) QueryInvoiceControls(ctx context.Context, a domain.Actor, invoiceID string) (json.RawMessage, error) {
	db := s.db.WithContext(ctx)
	if _, err := s.listInvoices(db, a, invoiceID, nil); err != nil {
		return nil, err
	}
	var invoice domain.Invoice
	var row store.Invoice
	if err := db.Where("tenant_id = ? AND id = ?", a.TenantID, invoiceID).First(&row).Error; err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(row.ContentJSON), &invoice); err != nil {
		return nil, err
	}
	invoice.TenantID = a.TenantID
	invoice.ID = invoiceID
	impacts, risks, err := invoiceControlRecords(db, a.TenantID, invoiceID)
	if err != nil {
		return nil, err
	}
	visible := []store.InvoiceControlImpact{}
	for _, impact := range impacts {
		c, _, e := loadClaim(db, a.TenantID, impact.ClaimID, false)
		if e != nil {
			return nil, e
		}
		if s.canRead(db, a, c) == nil {
			visible = append(visible, impact)
		}
	}
	var corrections []store.InvoiceCorrection
	if err = db.Where("tenant_id = ? AND invoice_id = ?", a.TenantID, invoiceID).Order("created_at DESC").Limit(1001).Find(&corrections).Error; err != nil {
		return nil, err
	}
	if len(corrections) > 1000 {
		return nil, domain.Fail("INVOICE_CONTROL_LIMIT", "票据控制历史超出上限", 422)
	}
	items := []map[string]any{}
	for _, correction := range corrections {
		items = append(items, map[string]any{"id": correction.ID, "version": correction.Version, "status": correction.Status, "requester_subject_id": correction.Requester, "approver_subject_id": correction.Approver, "content": json.RawMessage(correction.ContentJSON)})
	}
	sources, err := invoiceSourceViews(db, invoice)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"invoice_id": invoiceID, "version": row.Version, "corrections": items, "risks": risks, "impacts": visible, "sources": sources})
}
