package application

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type paidCorrectionRecoveryFixture struct {
	service      *Service
	db           *gorm.DB
	claim        domain.Claim
	cashier      domain.Actor
	reviewer     domain.Actor
	closer       domain.Actor
	paymentID    string
	correctionID string
	riskID       string
}

// newPaidCorrectionRecoveryFixture 通过真实业务命令建立已付更正及独立复核身份。
// 参数：t 为使用临时 SQLite 的本地测试上下文。
// 返回值：已付款、已批准更正且风险待核实的隔离场景。
func newPaidCorrectionRecoveryFixture(t *testing.T) paidCorrectionRecoveryFixture {
	t.Helper()
	s, db, c, app, head := approvedControlFixture(t)
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{
		"expense.claim.read", "expense.payment.read", "expense.payment.record",
		"expense.payment.reversal.request",
	}}
	paid := runCommand(t, s, cashier, "payment.record", c.ID,
		bindingPaymentBody("correction-paid", c.Version, "correction-original"))
	app.Permissions = append(app.Permissions, "expense.invoice.review", "expense.invoice.read")
	var invoice store.Invoice
	if err := db.First(&invoice, "tenant_id = ? AND id = ?", "t", "invoice").Error; err != nil {
		t.Fatal(err)
	}
	request := runCommand(t, s, app, "invoice.correction.request", invoice.ID, map[string]any{
		"command_id": "paid-correction-request", "expected_version": invoice.Version,
		"reason": "付款后核对原件发现号码需独立复核",
		"proposed_invoice": domain.Invoice{InvoiceType: "VAT", InvoiceNumber: "corrected-paid-number",
			InvoiceDate: "2026-11-10", Total: 100, FileIDs: []string{"file"}},
	})
	head.Permissions = append(head.Permissions, "expense.invoice.read", "expense.invoice.correct",
		"expense.finance.review", "expense.payment.reversal.approve")
	runCommand(t, s, head, "invoice.correction.decide", request["id"].(string), map[string]any{
		"command_id": "paid-correction-approve", "expected_version": 1,
		"approve": true, "reason": "独立批准后交由另一财务核实资金影响",
	})
	var risk store.InvoiceRisk
	if err := db.First(&risk, "tenant_id = ? AND invoice_id = ? AND kind = ?",
		"t", invoice.ID, "CORRECTION").Error; err != nil {
		t.Fatal(err)
	}
	closer := domain.Actor{TenantID: "t", SubjectID: "acct", Permissions: []string{
		"expense.claim.read", "expense.invoice.read", "expense.invoice.correct", "expense.finance.review",
	}}
	return paidCorrectionRecoveryFixture{s, db, c, cashier, head, closer,
		paid["id"].(string), request["id"].(string), risk.ID}
}

// TestPaidCorrectionDismissalAllowsReversalAndRepayment 验证已付更正排除风险后可以合法冲正重付。
// 参数：t 为单个临时 SQLite 业务链测试上下文。
// 返回值：无；遗留更正影响继续阻断重付时失败。
func TestPaidCorrectionDismissalAllowsReversalAndRepayment(t *testing.T) {
	f := newPaidCorrectionRecoveryFixture(t)
	var payment store.Payment
	var snapshot store.Snapshot
	var invoice store.Invoice
	for _, row := range []any{&payment, &snapshot, &invoice} {
		if err := f.db.Where("tenant_id = ?", "t").First(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	runCommand(t, f.service, f.closer, "invoice.risk.close", f.riskID, map[string]any{
		"command_id": "paid-correction-dismiss", "expected_version": 1,
		"resolution": "DISMISSED", "reason": "独立核实票面差异不构成资金风险",
	})
	request := runCommand(t, f.service, f.cashier, "reversal.request", f.paymentID, map[string]any{
		"command_id": "paid-correction-reverse", "expected_version": f.claim.Version + 1,
		"reason": "核实银行原流水实际未付款", "fact_kind": "NO_PAYMENT",
		"evidence_file_ids": []string{"proof"},
	})
	runCommand(t, f.service, f.reviewer, "reversal.decide", request["id"].(string), map[string]any{
		"command_id": "paid-correction-reverse-approve", "expected_version": 1,
		"approve": true, "reason": "独立确认实际未付后允许重付",
	})
	runCommand(t, f.service, f.cashier, "payment.record", f.claim.ID,
		bindingPaymentBody("paid-correction-repaid", f.claim.Version+2, "correction-repaid"))
	assertPaidCorrectionHistoryUnchanged(t, f.db, payment, snapshot, invoice)
}

// assertPaidCorrectionHistoryUnchanged 核验更正复核和重付未改旧付款、批准快照或已批准票面。
// 参数：t/db 为隔离测试边界，payment/snapshot/invoice 为闭环前保存的原始事实。
// 返回值：无；任一历史内容或批准摘要变化时失败。
func assertPaidCorrectionHistoryUnchanged(
	t *testing.T, db *gorm.DB, payment store.Payment, snapshot store.Snapshot, invoice store.Invoice,
) {
	t.Helper()
	var currentPayment store.Payment
	var currentSnapshot store.Snapshot
	var currentInvoice store.Invoice
	for _, row := range []struct {
		value any
		id    string
	}{
		{&currentPayment, payment.ID}, {&currentSnapshot, snapshot.ID}, {&currentInvoice, invoice.ID},
	} {
		if err := db.First(row.value, "tenant_id = ? AND id = ?", "t", row.id).Error; err != nil {
			t.Fatal(err)
		}
	}
	if currentPayment.ContentJSON != payment.ContentJSON ||
		currentSnapshot.ContentJSON != snapshot.ContentJSON ||
		currentSnapshot.ContentHash != snapshot.ContentHash ||
		currentInvoice.ContentJSON != invoice.ContentJSON {
		t.Fatal("已付更正复核或重付改写了旧付款、批准快照或原票面")
	}
}

// TestPaidCorrectionConfirmedRiskStillBlocksPayment 验证确认真实风险不会解除原更正影响或放行重付。
// 参数：t 为单个临时 SQLite 风险确认场景。
// 返回值：无；确认风险后通过付款控制或原更正影响被解除时失败。
func TestPaidCorrectionConfirmedRiskStillBlocksPayment(t *testing.T) {
	f := newPaidCorrectionRecoveryFixture(t)
	runCommand(t, f.service, f.closer, "invoice.risk.close", f.riskID, map[string]any{
		"command_id": "paid-correction-confirm-risk", "expected_version": 1,
		"resolution": "CONFIRMED_RISK", "reason": "独立核实确有资金风险必须继续阻断",
	})
	var impact store.InvoiceControlImpact
	if err := f.db.First(&impact, "tenant_id = ? AND control_id = ? AND kind = ?",
		"t", f.correctionID, "CORRECTION").Error; err != nil {
		t.Fatal(err)
	}
	if impact.Status != "FINANCE_REVIEW_REQUIRED" {
		t.Fatal("确认真实风险却解除了原更正影响", impact.Status)
	}
	c, _, err := loadClaim(f.db, "t", f.claim.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	sourceErrorCode(t, VerifyInvoiceControls(f.db, c, "PAYMENT"), "INVOICE_RISK_REVIEW_REQUIRED", 409)
}

// TestPaidCorrectionDismissalKeepsOtherControls 验证排除一案仅解除精确关联，不扩大到其他更正或租户。
// 参数：t 为单个临时 SQLite 案件隔离场景。
// 返回值：无；无关更正影响或其他租户记录被改动时失败。
func TestPaidCorrectionDismissalKeepsOtherControls(t *testing.T) {
	f := newPaidCorrectionRecoveryFixture(t)
	rows := []store.InvoiceControlImpact{
		{ID: "unrelated", TenantID: "t", InvoiceID: "invoice", ClaimID: f.claim.ID,
			ControlID: "other-correction", Kind: "CORRECTION", Status: "FINANCE_REVIEW_REQUIRED", ContentJSON: "{}"},
		{ID: "foreign", TenantID: "foreign", InvoiceID: "invoice", ClaimID: "foreign-claim",
			ControlID: f.correctionID, Kind: "CORRECTION", Status: "FINANCE_REVIEW_REQUIRED", ContentJSON: "{}"},
	}
	if err := f.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	runCommand(t, f.service, f.closer, "invoice.risk.close", f.riskID, map[string]any{
		"command_id": "paid-correction-exact-dismiss", "expected_version": 1,
		"resolution": "DISMISSED", "reason": "只排除本案件而不触及其他风险",
	})
	for _, id := range []string{"unrelated", "foreign"} {
		var impact store.InvoiceControlImpact
		if err := f.db.First(&impact, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		if impact.Status != "FINANCE_REVIEW_REQUIRED" {
			t.Fatal("无关更正被解除", id, impact.Status)
		}
	}
	c, _, err := loadClaim(f.db, "t", f.claim.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	sourceErrorCode(t, VerifyInvoiceControls(f.db, c, "PAYMENT"), "INVOICE_RECONFIRMATION_REQUIRED", 409)
}

// TestPaidCorrectionClosureRequiresIndependentScope 验证已付更正风险仍要求独立身份及当前租户财务范围。
// 参数：t 为单个临时 SQLite 权限边界场景。
// 返回值：无；申请人、记录人、无范围或其他租户身份关闭风险时失败。
func TestPaidCorrectionClosureRequiresIndependentScope(t *testing.T) {
	f := newPaidCorrectionRecoveryFixture(t)
	owner, unscoped, foreign := f.closer, f.closer, f.closer
	owner.SubjectID, unscoped.SubjectID, foreign.TenantID = "app", "unscoped", "foreign"
	for _, tc := range []struct {
		name   string
		actor  domain.Actor
		code   string
		status int
	}{
		{"owner", owner, "SELF_FINANCIAL_ACTION_FORBIDDEN", 403},
		{"recorder", f.reviewer, "SELF_FINANCIAL_ACTION_FORBIDDEN", 403},
		{"unscoped", unscoped, "FORBIDDEN", 403},
		{"foreign", foreign, "RESOURCE_NOT_FOUND", 404},
	} {
		raw, err := json.Marshal(map[string]any{"command_id": "reject-" + tc.name,
			"expected_version": 1, "resolution": "DISMISSED", "reason": "验证更正风险关闭边界"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.service.Execute(context.Background(), tc.actor, "invoice.risk.close", f.riskID, raw)
		sourceErrorCode(t, err, tc.code, tc.status)
	}
	if risk := invoiceRiskRow(t, f.db, f.riskID); risk.Status != "OPEN" || risk.Version != 1 {
		t.Fatal("被拒身份仍修改了风险案件", risk.Status, risk.Version)
	}
}
