package application

import (
	"encoding/json"
	"sort"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// correctionInvoiceFace 只复制可人工确认的票面，保留服务器归属、占用及原件集合。
// 参数：tx 为事务，row 为原票据，input 为完整更正票面。
// 返回值：共用确认规则后的票面；原件缺失或身份冲突时拒绝。
func correctionInvoiceFace(tx *gorm.DB, row store.Invoice, input domain.Invoice) (domain.Invoice, error) {
	var inv domain.Invoice
	if e := json.Unmarshal([]byte(row.ContentJSON), &inv); e != nil {
		return inv, e
	}
	oldFiles, newFiles := append([]string{}, inv.FileIDs...), append([]string{}, input.FileIDs...)
	sort.Strings(oldFiles)
	sort.Strings(newFiles)
	if len(oldFiles) == 0 || len(oldFiles) != len(newFiles) {
		return inv, domain.Fail("INVOICE_SOURCE_IMMUTABLE", "重大更正不得删除或替换原件，请使用受控追加来源", 422)
	}
	for i := range oldFiles {
		if oldFiles[i] != newFiles[i] {
			return inv, domain.Fail("INVOICE_SOURCE_IMMUTABLE", "重大更正不得删除或替换原件", 422)
		}
	}
	inv.InvoiceType, inv.InvoiceNumber, inv.InvoiceCode = input.InvoiceType, input.InvoiceNumber, input.InvoiceCode
	inv.InvoiceDate, inv.SellerTaxID, inv.BuyerTaxID = input.InvoiceDate, input.SellerTaxID, input.BuyerTaxID
	inv.Total, inv.VerificationStatus = input.Total, "NOT_CHECKED"
	inv.Content = input.Content
	inv.TravelFacts = input.TravelFacts
	inv.Version = row.Version + 1
	if e := confirmInvoiceFields(tx, &inv); e != nil {
		return inv, e
	}
	var count int64
	if e := tx.Model(&store.Invoice{}).Where("tenant_id = ? AND identity_key = ? AND id <> ?", row.TenantID, inv.IdentityKey, row.ID).Count(&count).Error; e != nil {
		return inv, e
	}
	if count != 0 {
		return inv, domain.Fail("INVOICE_IDENTITY_CONFLICT", "更正后的业务身份已经存在，不能绕过票据查重", 409)
	}
	return inv, nil
}

// requestInvoiceCorrection 为已占用或消费票据建立独立申请，并立即阻断受影响单据付款。
// 参数：tx 为事务，cc 为本人票据命令，input 为完整拟更正票面及原因。
// 返回值：待独立批准案件、票据标识及错误。
func requestInvoiceCorrection(tx *gorm.DB, cc *commandContext, input InvoiceControlInput) (any, string, error) {
	if e := require(cc.actor, "expense.invoice.review"); e != nil {
		return nil, "", e
	}
	row, c, e := lockControlledInvoice(tx, cc.actor.TenantID, cc.resourceID)
	if e != nil {
		return nil, "", e
	}
	if row.Owner != cc.actor.SubjectID {
		return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "票据不存在或非本人票据", 404)
	}
	if e = verifyVersion(row.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if c == nil || (row.ReservationState != "RESERVED" && row.ReservationState != "CONSUMED") {
		return nil, "", domain.Fail("INVOICE_NOT_LOCKED", "未占用票据请使用普通人工确认入口", 409)
	}
	var pending int64
	if e = tx.Model(&store.InvoiceCorrection{}).Where("tenant_id = ? AND invoice_id = ? AND status = ?", row.TenantID, row.ID, "PENDING").Count(&pending).Error; e != nil {
		return nil, "", e
	}
	if pending > 0 {
		return nil, "", domain.Fail("INVOICE_CORRECTION_PENDING", "该票据已有待批准重大更正", 409)
	}
	proposed, e := correctionInvoiceFace(tx, row, input.ProposedInvoice)
	if e != nil {
		return nil, "", e
	}
	var previous domain.Invoice
	if e = json.Unmarshal([]byte(row.ContentJSON), &previous); e != nil {
		return nil, "", e
	}
	content, e := encode(invoiceCorrectionContent{Previous: previous, Proposed: proposed, Reason: input.Reason})
	if e != nil {
		return nil, "", e
	}
	id, e := newID()
	if e != nil {
		return nil, "", e
	}
	request := store.InvoiceCorrection{ID: id, TenantID: row.TenantID, InvoiceID: row.ID, Requester: cc.actor.SubjectID, Status: "PENDING", Version: 1, InvoiceVersion: row.Version, ContentJSON: content}
	if e = tx.Create(&request).Error; e != nil {
		return nil, "", e
	}
	if e = addInvoiceControlImpact(tx, row, *c, id, "CORRECTION", "PENDING_CORRECTION"); e != nil {
		return nil, "", e
	}
	if e = appendOutbox(tx, c.TenantID, "notification", c.ID, map[string]any{"kind": "INVOICE_CORRECTION_REQUESTED", "claim_id": c.ID, "invoice_id": row.ID, "assignee_subject_id": c.Applicant}); e != nil {
		return nil, "", e
	}
	if e = appendInvoiceEvidenceArchives(tx, c); e != nil {
		return nil, "", e
	}
	return request, row.ID, nil
}

// addInvoiceControlImpact 记录控制案件对应的真实单据与审批轮次，不修改旧快照。
// 参数：tx 为事务，row 为票据，c 为关联单据，controlID/kind/status 为案件与初态。
// 返回值：有界关系写入错误。
func addInvoiceControlImpact(tx *gorm.DB, row store.Invoice, c domain.Claim, controlID, kind, status string) error {
	id, e := newID()
	if e != nil {
		return e
	}
	return tx.Create(&store.InvoiceControlImpact{ID: id, TenantID: row.TenantID, InvoiceID: row.ID, ClaimID: c.ID, ControlID: controlID, Kind: kind, Status: status, Version: 1, OriginalRound: c.CurrentRound, InvoiceVersion: row.Version, ContentJSON: "{}"}).Error
}

// decideInvoiceCorrection 独立核准重大更正，未付退回重审，已付只建立财务复核不改原事实。
// 参数：tx 为事务，cc 为独立财务身份，input 为案件版本、批准决定及原因。
// 返回值：决定后的案件与票据标识；失败全部回滚。
func decideInvoiceCorrection(tx *gorm.DB, cc *commandContext, input InvoiceControlInput) (any, string, error) {
	var lookup store.InvoiceCorrection
	if e := tx.Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&lookup).Error; e != nil {
		return nil, "", invoiceControlNotFound(e)
	}
	row, c, e := lockControlledInvoice(tx, cc.actor.TenantID, lookup.InvoiceID)
	if e != nil {
		return nil, "", e
	}
	if e = requireInvoiceControlReview(tx, cc.actor, row, c); e != nil {
		return nil, "", e
	}
	var request store.InvoiceCorrection
	if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&request).Error; e != nil {
		return nil, "", e
	}
	if e = verifyVersion(request.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if request.Requester == cc.actor.SubjectID {
		return nil, "", domain.Fail("SELF_FINANCIAL_ACTION_FORBIDDEN", "更正申请人不能自行批准", 403)
	}
	if request.Status != "PENDING" || request.InvoiceVersion != row.Version {
		return nil, "", domain.Fail("VERSION_CONFLICT", "更正案件或票据已改变", 409)
	}
	var content invoiceCorrectionContent
	if e = json.Unmarshal([]byte(request.ContentJSON), &content); e != nil {
		return nil, "", e
	}
	status, impactStatus := "REJECTED", "REJECTED"
	if input.Approve {
		status = "APPROVED"
		if c.ActivePaymentID != "" || c.Status == "PAID" {
			impactStatus = "FINANCE_REVIEW_REQUIRED"
			if _, _, e = createInvoiceRisk(tx, cc.actor, row, c, "CORRECTION", row.ID, input.Reason, nil); e != nil {
				return nil, "", e
			}
		} else {
			impactStatus = "RECONFIRM_REQUIRED"
			content.Proposed, e = correctionInvoiceFace(tx, row, content.Proposed)
			if e != nil {
				return nil, "", e
			}
			if e = applyUnpaidInvoiceCorrection(tx, &row, c, content.Proposed); e != nil {
				return nil, "", e
			}
		}
	}
	content.DecisionReason = input.Reason
	raw, e := encode(content)
	if e != nil {
		return nil, "", e
	}
	request.Status, request.Approver, request.Version, request.ContentJSON = status, cc.actor.SubjectID, request.Version+1, raw
	if e = tx.Model(&store.InvoiceCorrection{}).Where("id = ? AND version = ?", request.ID, input.ExpectedVersion).Updates(map[string]any{"status": status, "approver": request.Approver, "version": request.Version, "content_json": raw}).Error; e != nil {
		return nil, "", e
	}
	if e = tx.Model(&store.InvoiceControlImpact{}).Where("tenant_id = ? AND control_id = ?", row.TenantID, request.ID).Updates(map[string]any{"status": impactStatus, "invoice_version": content.Proposed.Version, "version": gorm.Expr("version + 1")}).Error; e != nil {
		return nil, "", e
	}
	if e = appendInvoiceEvidenceArchives(tx, c); e != nil {
		return nil, "", e
	}
	return request, row.ID, nil
}

// applyUnpaidInvoiceCorrection 仅更新当前票面并将未付单交回申请人，不释放占票或改历史。
// 参数：tx 为事务，row 为锁定票据，c 为锁定未付单，proposed 为再次校验的票面。
// 返回值：票面更新、任务取消或单据版本写入错误。
func applyUnpaidInvoiceCorrection(tx *gorm.DB, row *store.Invoice, c *domain.Claim, proposed domain.Invoice) error {
	if c.Status == "VOID" {
		return domain.Fail("STATE_CONFLICT", "已作废单据的重大更正须另行财务复核", 409)
	}
	raw, e := encode(proposed)
	if e != nil {
		return e
	}
	result := tx.Model(&store.Invoice{}).Where("tenant_id = ? AND id = ? AND version = ?", row.TenantID, row.ID, row.Version).Updates(map[string]any{"version": proposed.Version, "identity_key": proposed.IdentityKey, "content_json": raw})
	if result.Error != nil {
		return domain.Fail("INVOICE_IDENTITY_CONFLICT", "更正票据身份冲突，未修改任何数据", 409)
	}
	if result.RowsAffected != 1 {
		return domain.Fail("VERSION_CONFLICT", "票据已改变，请刷新", 409)
	}
	row.Version, row.ContentJSON = proposed.Version, raw
	if e = cancelTasks(tx, *c); e != nil {
		return e
	}
	previous := c.Version
	c.Version++
	c.Status = "RETURNED"
	if e = saveClaim(tx, c, previous); e != nil {
		return e
	}
	return appendOutbox(tx, c.TenantID, "notification", c.ID, map[string]any{"kind": "INVOICE_RECONFIRM_REQUIRED", "claim_id": c.ID, "invoice_id": row.ID, "assignee_subject_id": c.Applicant})
}

// reconfirmInvoiceCorrection 要求申请人明确接受当前更正票面，随后仍必须创建新审批轮次。
// 参数：tx 为事务，cc 为申请人单据命令，input 为案件引用、单据版本和原因。
// 返回值：确认影响状态及所属单据；不直接批准或付款。
func reconfirmInvoiceCorrection(tx *gorm.DB, cc *commandContext, input InvoiceControlInput) (any, string, error) {
	if e := require(cc.actor, "expense.claim.submit"); e != nil {
		return nil, "", e
	}
	c, _, e := loadClaim(tx, cc.actor.TenantID, cc.resourceID, true)
	if e != nil {
		return nil, "", e
	}
	if c.Applicant != cc.actor.SubjectID {
		return nil, "", domain.Fail("FORBIDDEN", "仅申请人可重新确认受影响单据", 403)
	}
	if e = verifyVersion(c.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if c.Status != "RETURNED" && c.Status != "DRAFT" {
		return nil, "", domain.Fail("STATE_CONFLICT", "仅退回或草稿单据可重新确认票据", 409)
	}
	var impact store.InvoiceControlImpact
	if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND control_id = ? AND claim_id = ? AND kind = ?", c.TenantID, input.CorrectionID, c.ID, "CORRECTION").First(&impact).Error; e != nil {
		return nil, "", invoiceControlNotFound(e)
	}
	if impact.Status != "RECONFIRM_REQUIRED" {
		return nil, "", domain.Fail("STATE_CONFLICT", "此更正当前不允许申请人重新确认", 409)
	}
	var row store.Invoice
	if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", c.TenantID, impact.InvoiceID).First(&row).Error; e != nil {
		return nil, "", e
	}
	var inv domain.Invoice
	if e = json.Unmarshal([]byte(row.ContentJSON), &inv); e != nil {
		return nil, "", e
	}
	if inv.Version != impact.InvoiceVersion {
		return nil, "", domain.Fail("VERSION_CONFLICT", "票据已再次变化，请重新核实更正", 409)
	}
	raw, e := encode(map[string]any{"invoice": inv, "reason": input.Reason, "actor_subject_id": cc.actor.SubjectID})
	if e != nil {
		return nil, "", e
	}
	if e = tx.Model(&store.InvoiceControlImpact{}).Where("id = ? AND version = ?", impact.ID, impact.Version).Updates(map[string]any{"status": "RECONFIRMED", "confirmed_by": cc.actor.SubjectID, "content_json": raw, "version": impact.Version + 1}).Error; e != nil {
		return nil, "", e
	}
	previous := c.Version
	c.Version++
	if e = saveClaim(tx, &c, previous); e != nil {
		return nil, "", e
	}
	return map[string]any{"id": impact.ID, "correction_id": input.CorrectionID, "claim_id": c.ID, "claim_version": c.Version, "status": "RECONFIRMED"}, c.ID, nil
}
