package application

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestSubmitBlockedByOpenInvoiceRiskUntilReviewClosed 验证未闭环红冲风险真实阻断提交，闭环后才可再提交。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestSubmitBlockedByOpenInvoiceRiskUntilReviewClosed(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	app, claimID := submissionFixture(t, db, true)
	app.Permissions = append(app.Permissions, "expense.invoice.review")
	seedPolicy(t, db, "scopes", domain.Scope{ID: "risk-reviewer-scope", SubjectID: "risk-reviewer", OrgIDs: []string{"org"}})
	runCommand(t, s, app, "claim.submit", claimID, map[string]any{"command_id": "risk-submit-first", "expected_version": 1})
	// 已占用票据的重大更正：独立财务批准后单据退回申请人，票据票面被更正。
	correction := runCommand(t, s, app, "invoice.correction.request", "invoice", map[string]any{
		"command_id": "risk-correction", "expected_version": 2, "reason": "核对原件后更正票面",
		"proposed_invoice": domain.Invoice{InvoiceType: "VAT", InvoiceNumber: "risk-corrected", InvoiceDate: "2026-11-10", Total: 100, FileIDs: []string{"file"}},
	})
	correctionID, _ := correction["id"].(string)
	if correctionID == "" {
		t.Fatal("未返回重大更正案件标识")
	}
	reviewer := domain.Actor{TenantID: "t", SubjectID: "risk-reviewer", Permissions: []string{"expense.claim.read", "expense.invoice.read", "expense.invoice.correct", "expense.finance.review"}}
	runCommand(t, s, reviewer, "invoice.correction.decide", correctionID, map[string]any{"command_id": "risk-correction-approve", "expected_version": 1, "approve": true, "reason": "复核原件后批准更正"})
	claim, _, e := loadClaim(db, "t", claimID, false)
	if e != nil || claim.Status != "RETURNED" {
		t.Fatalf("重大更正未退回申请人重审：%s %v", claim.Status, e)
	}
	runCommand(t, s, app, "claim.invoice.reconfirm", claimID, map[string]any{"command_id": "risk-reconfirm", "expected_version": claim.Version, "correction_id": correctionID, "reason": "确认更正后的票面"})
	claim, _, e = loadClaim(db, "t", claimID, false)
	if e != nil || claim.Status != "RETURNED" {
		t.Fatalf("申请人重新确认不应直接批准：%s %v", claim.Status, e)
	}
	// 申请人登记真实红字风险，风险与仍受影响单据同时留下财务复核影响。
	risk := runCommand(t, s, app, "invoice.risk.record", "invoice", map[string]any{
		"command_id": "risk-record", "expected_version": 3, "risk_kind": "RED_LETTER",
		"original_invoice_id": "invoice", "evidence_file_ids": []string{"file"}, "reason": "收到供应商红字通知",
	})
	riskID, _ := risk["id"].(string)
	if riskID == "" {
		t.Fatal("未返回风险案件标识")
	}
	var impact store.InvoiceControlImpact
	if e = db.First(&impact, "tenant_id = ? AND control_id = ? AND kind = ?", "t", riskID, "RISK").Error; e != nil || impact.Status != "FINANCE_REVIEW_REQUIRED" {
		t.Fatalf("风险未建立阻断影响：%+v %v", impact, e)
	}
	raw, _ := json.Marshal(map[string]any{"command_id": "risk-submit-blocked", "expected_version": claim.Version})
	_, e = s.Execute(context.Background(), app, "claim.submit", claimID, raw)
	sourceErrorCode(t, e, "INVOICE_RISK_REVIEW_REQUIRED", 409)
	unchanged, _, e := loadClaim(db, "t", claimID, false)
	if e != nil || unchanged.Version != claim.Version || unchanged.CurrentRound != claim.CurrentRound || unchanged.Status != "RETURNED" {
		t.Fatalf("被拒绝的提交仍改变单据事实：%s v%d r%d %v", unchanged.Status, unchanged.Version, unchanged.CurrentRound, e)
	}
	// 独立财务核实排除风险后，同一提交才可放行并进入新轮次。
	runCommand(t, s, reviewer, "invoice.risk.close", riskID, map[string]any{"command_id": "risk-close", "expected_version": 1, "resolution": "DISMISSED", "reason": "与供应商核实未开具红字"})
	runCommand(t, s, app, "claim.submit", claimID, map[string]any{"command_id": "risk-submit-passed", "expected_version": claim.Version})
	final, _, e := loadClaim(db, "t", claimID, false)
	if e != nil || final.Status != "IN_APPROVAL" || final.CurrentRound != claim.CurrentRound+1 {
		t.Fatalf("风险闭环后提交未进入新轮次：%s v%d r%d %v", final.Status, final.Version, final.CurrentRound, e)
	}
}

// TestPaymentBlockedByPendingInvoiceCorrection 验证未决重大更正真实阻断出款且不写入付款事实。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestPaymentBlockedByPendingInvoiceCorrection(t *testing.T) {
	s, db, claim, app, _ := approvedControlFixture(t)
	app.Permissions = append(app.Permissions, "expense.invoice.review")
	correction := runCommand(t, s, app, "invoice.correction.request", "invoice", map[string]any{
		"command_id": "payment-correction", "expected_version": 2, "reason": "原件金额录入有误",
		"proposed_invoice": domain.Invoice{InvoiceType: "VAT", InvoiceNumber: "payment-corrected", InvoiceDate: "2026-11-10", Total: 100, FileIDs: []string{"file"}},
	})
	if correction["id"] == nil {
		t.Fatal("待付款票据未建立重大更正案件")
	}
	var impact store.InvoiceControlImpact
	if e := db.First(&impact, "tenant_id = ? AND claim_id = ? AND kind = ?", "t", claim.ID, "CORRECTION").Error; e != nil || impact.Status != "PENDING_CORRECTION" {
		t.Fatalf("重大更正未建立待决影响：%+v %v", impact, e)
	}
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record"}}
	raw, _ := json.Marshal(map[string]any{"command_id": "payment-blocked", "expected_version": claim.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "txn-blocked", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}})
	_, e := s.Execute(context.Background(), cashier, "payment.record", claim.ID, raw)
	sourceErrorCode(t, e, "INVOICE_RECONFIRMATION_REQUIRED", 409)
	var payments int64
	if e = db.Model(&store.Payment{}).Count(&payments).Error; e != nil || payments != 0 {
		t.Fatalf("未决重大更正仍写入付款事实：%d %v", payments, e)
	}
	after, _, e := loadClaim(db, "t", claim.ID, false)
	if e != nil || after.Status != "APPROVED_PENDING_PAYMENT" || after.ActivePaymentID != "" || after.Version != claim.Version {
		t.Fatalf("被拒绝的出款改变单据状态：%s v%d %s %v", after.Status, after.Version, after.ActivePaymentID, e)
	}
}

// TestInvoiceSourceAppendCommandRunsThroughPreflightAndDispatch 验证来源追加命令经真实预检与事务分发写入。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestInvoiceSourceAppendCommandRunsThroughPreflightAndDispatch(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	app, _ := submissionFixture(t, db, true)
	app.Permissions = append(app.Permissions, "expense.invoice.review")
	raw, _ := json.Marshal(map[string]any{"command_id": "append-no-reason", "expected_version": 1, "file_ids": []string{"file"}})
	_, e := s.Execute(context.Background(), app, "invoice.source.append", "invoice", raw)
	sourceErrorCode(t, e, "REASON_REQUIRED", 422)
	receipt := runCommand(t, s, app, "invoice.source.append", "invoice", map[string]any{"command_id": "append-controlled", "expected_version": 1, "file_ids": []string{"file"}, "reason": "补充同票原件来源"})
	if receipt["invoice_id"] != "invoice" {
		t.Fatalf("来源追加回执缺少票据标识：%v", receipt)
	}
	var sources int64
	if e = db.Model(&store.InvoiceSource{}).Where("tenant_id = ? AND invoice_id = ?", "t", "invoice").Count(&sources).Error; e != nil || sources != 1 {
		t.Fatalf("受控来源追加未写入唯一来源关系：%d %v", sources, e)
	}
}
