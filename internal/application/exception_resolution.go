package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"strconv"
)

// validateExceptionResolution 验证外部异常与既有正常付款或已批准冲正完全对应，不创造新资金效果。
// 参数：tx 为事务，c 为锁定单据，fact 为原异常事实，actor 为独立核实人，input 为关联决定。
// 返回值：无效关联、不同交易或自审错误。
func validateExceptionResolution(tx *gorm.DB, c domain.Claim, fact domain.PaymentExceptionFact, actor domain.Actor, input PaymentExceptionReviewInput) error {
	var row store.Payment
	if e := tx.Where("tenant_id = ? AND claim_id = ? AND id = ?", c.TenantID, c.ID, input.LinkedPaymentID).First(&row).Error; e != nil {
		return domain.Fail("EXCEPTION_LINK_MISMATCH", "关联付款不存在或不属于当前单据", 422)
	}
	if row.Actor == actor.SubjectID {
		return domain.Fail("EXCEPTION_REVIEWER_CONFLICT", "原付款人不能为自己的付款异常作独立解除核实", 403)
	}
	var payment PaymentResult
	if e := json.Unmarshal([]byte(row.ContentJSON), &payment); e != nil {
		return e
	}
	if payment.ID != row.ID || payment.ClaimID != c.ID || payment.Amount != fact.Amount || payment.Currency != fact.Currency || payment.PaidDate != fact.PaidDate || payment.Method != fact.Method || payment.SourceAccountRef != fact.SourceAccountRef || payment.TransactionNo != fact.TransactionNo || (fact.PayeeProfileID != "" && payment.PayeeProfileID != fact.PayeeProfileID) {
		return domain.Fail("EXCEPTION_LINK_MISMATCH", "关联付款的金额、币种、日期、渠道、来源、流水或已知收款资料与原事实不一致", 422)
	}
	var snapshot store.Snapshot
	if e := tx.Where("tenant_id = ? AND claim_id = ? AND content_hash = ?", c.TenantID, c.ID, payment.ContentHash).First(&snapshot).Error; e != nil {
		return domain.Fail("EXCEPTION_LINK_MISMATCH", "关联付款缺少合法批准快照", 422)
	}
	if e := VerifyApprovalSnapshot(json.RawMessage(snapshot.ContentJSON), payment.ContentHash); e != nil {
		return e
	}
	effect, e := exceptionLinkedEffect(tx, c, payment.ID, "PAYMENT_RECORDED")
	if e != nil {
		return e
	}
	if effect.PaymentRecordID != payment.ID || effect.EffectID != payment.ID || effect.AmountMinor != strconv.FormatInt(int64(fact.Amount), 10) || effect.ApprovedContentHash != payment.ContentHash {
		return domain.Fail("EXCEPTION_LINK_MISMATCH", "关联付款不可变效果与原事实不一致", 422)
	}
	if input.Decision == "RESOLVE_WITH_PAYMENT" {
		if c.Status != "PAID" || c.ActivePaymentID != payment.ID {
			return domain.Fail("EXCEPTION_LINK_MISMATCH", "只能通过当前有效正常付款解除此类冲突", 422)
		}
		return nil
	}
	var reversal store.Reversal
	if e = tx.Where("tenant_id = ? AND claim_id = ? AND payment_id = ? AND id = ?", c.TenantID, c.ID, payment.ID, input.LinkedReversalID).First(&reversal).Error; e != nil {
		return domain.Fail("EXCEPTION_LINK_MISMATCH", "关联冲正不存在或不属于原付款", 422)
	}
	var proof struct {
		Request ReversalData       `json:"request"`
		Effect  domain.SpendEffect `json:"effect"`
	}
	if e = json.Unmarshal([]byte(reversal.ContentJSON), &proof); e != nil {
		return e
	}
	reverse, e := exceptionLinkedEffect(tx, c, reversal.ID, "PAYMENT_FULLY_REVERSED")
	if e != nil {
		return e
	}
	if proof.Request.Status != "APPROVED" || proof.Request.PaymentID != payment.ID || proof.Request.Approver == "" || proof.Effect.EffectID != reverse.EffectID || domain.SpendEffectHash(proof.Effect) != domain.SpendEffectHash(reverse) || reverse.ReversesEffectID != payment.ID || reverse.PaymentRecordID != payment.ID || reverse.AmountMinor != "-"+effect.AmountMinor || reverse.ApprovedContentHash != payment.ContentHash {
		return domain.Fail("EXCEPTION_LINK_MISMATCH", "解除冲突所需全额冲正及独立批准证据不完整", 422)
	}
	return nil
}

// exceptionLinkedEffect 只读取内容摘要完整的同单不可变财务效果。
// 参数：tx 为事务，c 为单据边界，id/kind 为期望的效果类型。
// 返回值：已验证效果或非法关联错误。
func exceptionLinkedEffect(tx *gorm.DB, c domain.Claim, id, kind string) (domain.SpendEffect, error) {
	var row store.Effect
	var effect domain.SpendEffect
	if e := tx.Where("tenant_id = ? AND claim_id = ? AND id = ? AND kind = ?", c.TenantID, c.ID, id, kind).First(&row).Error; e != nil {
		return effect, domain.Fail("EXCEPTION_LINK_MISMATCH", "关联资金事实缺少不可变业务效果", 422)
	}
	if e := json.Unmarshal([]byte(row.ContentJSON), &effect); e != nil {
		return effect, e
	}
	if domain.SpendEffectHash(effect) != row.ContentHash || effect.Kind != kind {
		return effect, domain.Fail("EXCEPTION_LINK_MISMATCH", "关联资金效果摘要不一致", 409)
	}
	return effect, nil
}
