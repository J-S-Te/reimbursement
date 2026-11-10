package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"strings"
)

type ClaimControlInput struct {
	CommandInput
	Reason string `json:"reason"`
}

// controlledClaimChange 将待付义务交回申请人确认或作废，不改写旧批准及付款事实。
// 参数：tx 为本地事务，cc 为独立财务操作者命令，payload 为原因与当前单据版本。
// 返回值：单据新状态与版本，旧快照和已消费证据仍保留。
func (s *Service) controlledClaimChange(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input ClaimControlInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	c, _, e := loadClaim(tx, cc.actor.TenantID, cc.resourceID, true)
	if e != nil {
		return nil, "", e
	}
	permission := "expense.claim.reopen"
	if cc.operation == "claim.approved_void" {
		permission = "expense.claim.approved_void"
	}
	if e = requireFinancial(tx, cc.actor, c, permission); e != nil {
		return nil, "", e
	}
	if e = validateClaimControl(c, cc.actor, input); e != nil {
		return nil, "", e
	}
	previous := c.Version
	c.Version++
	c.Status = "RETURNED"
	action := "CLAIM_REOPEN"
	if cc.operation == "claim.approved_void" {
		c.Status = "VOID"
		action = "CLAIM_APPROVED_VOID"
		if e = releaseInvoices(tx, c); e != nil {
			return nil, "", e
		}
	}
	id, e := newID()
	if e != nil {
		return nil, "", e
	}
	if e = tx.Create(&store.Action{ID: id, TenantID: c.TenantID, ClaimID: c.ID, Actor: cc.actor.SubjectID, Action: action, Reason: input.Reason, Round: c.CurrentRound}).Error; e != nil {
		return nil, "", e
	}
	e = saveClaim(tx, &c, previous)
	if e == nil {
		e = appendFinancialHistoryArchive(tx, c)
	}
	return c, c.ID, e
}

// validateClaimControl 检查财务重开和批准后作废的共同状态、版本与职责分离约束。
// 参数：c 为当前聚合，a 为操作者，input 为原因与乐观锁版本。
// 返回值：不满足前置条件时的明确错误。
func validateClaimControl(c domain.Claim, a domain.Actor, input ClaimControlInput) error {
	if e := verifyVersion(c.Version, input.ExpectedVersion); e != nil {
		return e
	}
	if c.Applicant == a.SubjectID {
		return domain.Fail("SELF_FINANCIAL_ACTION_FORBIDDEN", "申请人不能为本人重开或作废已批准义务", 403)
	}
	if strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 2000 {
		return domain.Fail("REASON_REQUIRED", "请填写重开或作废的财务原因", 422)
	}
	if c.Status != "APPROVED_PENDING_PAYMENT" || c.ActivePaymentID != "" {
		return domain.Fail("STATE_CONFLICT", "仅无有效付款的待付款单可以重开或批准后作废", 409)
	}
	return nil
}

// consumedEvidenceReapproval 检查消费证据仍归本单且所有既有付款义务都已合法全额冲正。
// 参数：tx 为当前提交事务，c 为准备重新审批的原单。
// 返回值：不具备完整零净额历史时拒绝重新使用消费证据。
func consumedEvidenceReapproval(tx *gorm.DB, c domain.Claim) error {
	if c.ActivePaymentID != "" {
		return domain.Fail("INVOICE_CONSUMED", "尚有有效付款，不能重新审批消费证据", 409)
	}
	var payments []store.Payment
	if e := tx.Where("tenant_id = ? AND claim_id = ?", c.TenantID, c.ID).Limit(2001).Find(&payments).Error; e != nil {
		return e
	}
	if len(payments) == 0 || len(payments) > 2000 {
		return domain.Fail("INVOICE_CONSUMED", "消费证据缺少可复核的全额冲正历史", 409)
	}
	for _, payment := range payments {
		var reversal store.Reversal
		if e := tx.Where("tenant_id = ? AND claim_id = ? AND payment_id = ?", c.TenantID, c.ID, payment.ID).First(&reversal).Error; e != nil {
			return domain.Fail("INVOICE_CONSUMED", "原单存在未全额冲正的付款", 409)
		}
		var proof struct {
			Request ReversalData       `json:"request"`
			Effect  domain.SpendEffect `json:"effect"`
		}
		if e := json.Unmarshal([]byte(reversal.ContentJSON), &proof); e != nil {
			return e
		}
		original, e := exceptionLinkedEffect(tx, c, payment.ID, "PAYMENT_RECORDED")
		if e != nil {
			return e
		}
		reverse, e := exceptionLinkedEffect(tx, c, reversal.ID, "PAYMENT_FULLY_REVERSED")
		if e != nil {
			return e
		}
		if proof.Request.Status != "APPROVED" || proof.Request.PaymentID != payment.ID || reverse.PaymentRecordID != payment.ID || reverse.ReversesEffectID != original.EffectID || reverse.AmountMinor != "-"+original.AmountMinor || domain.SpendEffectHash(proof.Effect) != domain.SpendEffectHash(reverse) {
			return domain.Fail("INVOICE_CONSUMED", "消费证据的独立批准全额冲正链不完整", 409)
		}
	}
	return nil
}
