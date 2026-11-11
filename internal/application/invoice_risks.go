package application

import (
	"encoding/json"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// recordInvoiceRisk 记录真实票据风险和原票关系，不能把红字或退款伪装成负数报销。
// 参数：tx 为事务，cc 为票据报告人，input 为风险种类、原票、原因和证据。
// 返回值：独立财务复核案件、票据标识及错误。
func recordInvoiceRisk(tx *gorm.DB, cc *commandContext, input InvoiceControlInput) (any, string, error) {
	if e := require(cc.actor, "expense.invoice.review"); e != nil {
		return nil, "", e
	}
	if input.RiskKind != "RED_LETTER" && input.RiskKind != "VOID" && input.RiskKind != "REFUND" {
		return nil, "", domain.Fail("INVOICE_RISK_INVALID", "风险仅支持红字、作废或退款事实", 422)
	}
	if input.OriginalInvoiceID == "" || len(input.EvidenceFileIDs) == 0 || len(input.EvidenceFileIDs) > 20 {
		return nil, "", domain.Fail("INVOICE_RISK_EVIDENCE_REQUIRED", "须引用真实原票及1至20份风险证据", 422)
	}
	row, c, e := lockControlledInvoice(tx, cc.actor.TenantID, cc.resourceID)
	if e != nil {
		return nil, "", e
	}
	if row.Owner != cc.actor.SubjectID {
		return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "仅票据所有人可报告风险", 404)
	}
	if e = verifyVersion(row.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	original, originalClaim, e := lockControlledInvoice(tx, cc.actor.TenantID, input.OriginalInvoiceID)
	if e != nil {
		return nil, "", e
	}
	if original.Owner != row.Owner {
		return nil, "", domain.Fail("INVOICE_ORIGINAL_REFERENCE_INVALID", "原票必须属于相同租户及票据所有人", 422)
	}
	for _, id := range input.EvidenceFileIDs {
		if _, e = invoiceControlDocument(tx, cc.actor, id); e != nil {
			return nil, "", e
		}
	}
	risk, resource, e := createInvoiceRisk(tx, cc.actor, row, c,
		input.RiskKind, original.ID, input.Reason, "", input.EvidenceFileIDs)
	if e != nil {
		return nil, "", e
	}
	if originalClaim != nil && (c == nil || c.ID != originalClaim.ID) {
		if e = addInvoiceControlImpact(tx, original, *originalClaim, risk.ID, "RISK", "FINANCE_REVIEW_REQUIRED"); e != nil {
			return nil, "", e
		}
	}
	if e = appendInvoiceEvidenceArchives(tx, c, originalClaim); e != nil {
		return nil, "", e
	}
	return risk, resource, nil
}

// createInvoiceRisk 建立独立财务复核案件并通知原申请人，不改发票、付款或退款事实。
// 参数：tx 为事务，a 为报告人，row/c 为票据及关联单，
//
//	kind/original/reason/evidence 为风险依据，correctionID 为自动更正风险的精确案件标识。
//
// 返回值：新风险案件、票据标识和错误。
func createInvoiceRisk(
	tx *gorm.DB, a domain.Actor, row store.Invoice, c *domain.Claim,
	kind, original, reason, correctionID string, evidence []string,
) (store.InvoiceRisk, string, error) {
	id, e := newID()
	if e != nil {
		return store.InvoiceRisk{}, "", e
	}
	content := map[string]any{"reason": reason, "evidence_file_ids": evidence, "invoice_version": row.Version}
	if kind == "CORRECTION" && correctionID != "" {
		content["correction_id"] = correctionID
	}
	raw, e := encode(content)
	if e != nil {
		return store.InvoiceRisk{}, "", e
	}
	risk := store.InvoiceRisk{ID: id, TenantID: row.TenantID, InvoiceID: row.ID, OriginalInvoiceID: original, Recorder: a.SubjectID, Kind: kind, Status: "OPEN", Version: 1, ContentJSON: raw}
	if e = tx.Create(&risk).Error; e != nil {
		return risk, "", e
	}
	if c != nil {
		if e = addInvoiceControlImpact(tx, row, *c, id, "RISK", "FINANCE_REVIEW_REQUIRED"); e != nil {
			return risk, "", e
		}
		e = appendOutbox(tx, c.TenantID, "notification", c.ID, map[string]any{"kind": "INVOICE_FINANCE_REVIEW_REQUIRED", "claim_id": c.ID, "invoice_id": row.ID, "risk_id": id, "assignee_subject_id": c.Applicant})
	}
	return risk, row.ID, e
}

// requireInvoiceRiskClosureWithoutClaim 允许无归属单据的票据风险由独立财务关闭，不保留无法闭环的阻断案件。
// 参数：a 为关闭人，row 为被标票据，original 为风险原票。
// 返回值：缺少票据更正或财务复核能力、或关闭人为票据所有人时拒绝。
func requireInvoiceRiskClosureWithoutClaim(a domain.Actor, row, original store.Invoice) error {
	// 票据未被单据占用时没有可核实的组织归属，因此收口为双重财务能力，并继续禁止票据所有人自审。
	if e := require(a, "expense.invoice.correct"); e != nil {
		return e
	}
	if e := require(a, "expense.finance.review"); e != nil {
		return e
	}
	for _, invoice := range []store.Invoice{row, original} {
		if invoice.Owner != "" && a.SubjectID == invoice.Owner {
			return domain.Fail("SELF_FINANCIAL_ACTION_FORBIDDEN", "票据所有人不能自行关闭财务复核", 403)
		}
	}
	return nil
}

// closeInvoiceRisk 由独立财务人员核实关闭风险案件，确认风险仍阻断普通报销；未被单据占用的票据风险同样由独立财务闭环。
// 参数：tx 为事务，cc 为财务核查身份，input 为案件版本、CONFIRMED_RISK或DISMISSED及原因。
// 返回值：已关闭案件与票据标识；不生成或代替付款冲正。
func closeInvoiceRisk(tx *gorm.DB, cc *commandContext, input InvoiceControlInput) (any, string, error) {
	if input.Resolution != "CONFIRMED_RISK" && input.Resolution != "DISMISSED" {
		return nil, "", domain.Fail("INVOICE_RISK_RESOLUTION_INVALID", "财务复核须明确确认风险或排除风险", 422)
	}
	var lookup store.InvoiceRisk
	if e := tx.Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&lookup).Error; e != nil {
		return nil, "", invoiceControlNotFound(e)
	}
	row, c, e := lockControlledInvoice(tx, cc.actor.TenantID, lookup.InvoiceID)
	if e != nil {
		return nil, "", e
	}
	original, originalClaim, e := lockControlledInvoice(tx, cc.actor.TenantID, lookup.OriginalInvoiceID)
	if e != nil {
		return nil, "", e
	}
	if c != nil {
		if e = requireInvoiceControlReview(tx, cc.actor, row, c); e != nil {
			return nil, "", e
		}
	}
	if originalClaim != nil {
		if e = requireInvoiceControlReview(tx, cc.actor, original, originalClaim); e != nil {
			return nil, "", e
		}
	}
	if c == nil && originalClaim == nil {
		if e = requireInvoiceRiskClosureWithoutClaim(cc.actor, row, original); e != nil {
			return nil, "", e
		}
	}
	var risk store.InvoiceRisk
	if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&risk).Error; e != nil {
		return nil, "", e
	}
	if e = verifyVersion(risk.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if risk.Recorder == cc.actor.SubjectID {
		return nil, "", domain.Fail("SELF_FINANCIAL_ACTION_FORBIDDEN", "风险记录人不能自行关闭财务复核", 403)
	}
	if risk.Status != "OPEN" {
		return nil, "", domain.Fail("STATE_CONFLICT", "风险案件已经关闭", 409)
	}
	var content map[string]any
	if e = json.Unmarshal([]byte(risk.ContentJSON), &content); e != nil {
		return nil, "", e
	}
	content["resolution_reason"], content["reviewer_subject_id"] = input.Reason, cc.actor.SubjectID
	raw, e := encode(content)
	if e != nil {
		return nil, "", e
	}
	risk.Status, risk.Resolution, risk.Reviewer, risk.Version = "CLOSED", input.Resolution, cc.actor.SubjectID, risk.Version+1
	if e = tx.Model(&store.InvoiceRisk{}).Where("id = ? AND version = ?", risk.ID, input.ExpectedVersion).Updates(map[string]any{"status": risk.Status, "resolution": risk.Resolution, "reviewer": risk.Reviewer, "version": risk.Version, "content_json": raw}).Error; e != nil {
		return nil, "", e
	}
	if e = tx.Model(&store.InvoiceControlImpact{}).Where("tenant_id = ? AND control_id = ?", risk.TenantID, risk.ID).Updates(map[string]any{"status": "RESOLVED", "version": gorm.Expr("version + 1")}).Error; e != nil {
		return nil, "", e
	}
	if correctionID, _ := content["correction_id"].(string); risk.Kind == "CORRECTION" &&
		correctionID != "" && input.Resolution == "DISMISSED" {
		e = tx.Model(&store.InvoiceControlImpact{}).Where(
			"tenant_id = ? AND invoice_id = ? AND control_id = ? AND kind = ? AND status = ?",
			risk.TenantID, risk.InvoiceID, correctionID, "CORRECTION", "FINANCE_REVIEW_REQUIRED",
		).Updates(map[string]any{"status": "RESOLVED", "version": gorm.Expr("version + 1")}).Error
	}
	if e == nil {
		e = appendInvoiceEvidenceArchives(tx, c, originalClaim)
	}
	return risk, row.ID, e
}
