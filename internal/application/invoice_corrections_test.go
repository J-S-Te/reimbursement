package application

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestInvoiceCorrectionRequiresIndependentApproval 验证占用票据重大更正须独立批准且不改旧审批快照。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestInvoiceCorrectionRequiresIndependentApproval(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	app, claimID := submissionFixture(t, db, true)
	app.Permissions = append(app.Permissions, "expense.invoice.review", "expense.invoice.read")
	runCommand(t, s, app, "claim.submit", claimID, map[string]any{"command_id": "invoice-control-submit", "expected_version": 1})
	var oldSnapshot store.Snapshot
	db.Where("claim_id = ?", claimID).First(&oldSnapshot)
	proposed := domain.Invoice{InvoiceType: "VAT", InvoiceNumber: "corrected-number", InvoiceDate: "2026-11-10", Total: 100, FileIDs: []string{"file"}}
	body := map[string]any{"command_id": "correction-request", "expected_version": 2, "proposed_invoice": proposed, "reason": "核对原件后号码更正"}
	request := runCommand(t, s, app, "invoice.correction.request", "invoice", body)
	correctionID, _ := request["id"].(string)
	if correctionID == "" {
		t.Fatal("未返回重大更正申请标识")
	}
	self := app
	self.Permissions = append(self.Permissions, "expense.invoice.correct", "expense.finance.review", "expense.scope.tenant_all")
	raw := json.RawMessage(`{"command_id":"self-correction","expected_version":1,"approve":true,"reason":"自行批准"}`)
	if _, e := s.Execute(context.Background(), self, "invoice.correction.decide", correctionID, raw); e == nil {
		t.Fatal("更正申请人自批")
	}
	seedPolicy(t, db, "scopes", domain.Scope{ID: "correction-scope", SubjectID: "reviewer", OrgIDs: []string{"org"}})
	reviewer := domain.Actor{TenantID: "t", SubjectID: "reviewer", Permissions: []string{"expense.invoice.read", "expense.invoice.correct", "expense.finance.review", "expense.claim.read"}}
	runCommand(t, s, reviewer, "invoice.correction.decide", correctionID, map[string]any{"command_id": "correction-approved", "expected_version": 1, "approve": true, "reason": "复核原件后批准"})
	claim, _, e := loadClaim(db, "t", claimID, false)
	if e != nil || claim.Status != "RETURNED" {
		t.Fatalf("重大更正未要求重审：%s %v", claim.Status, e)
	}
	var frozen store.Snapshot
	db.First(&frozen, "id = ?", oldSnapshot.ID)
	if frozen.ContentJSON != oldSnapshot.ContentJSON || frozen.ContentHash != oldSnapshot.ContentHash {
		t.Fatal("重大更正改写了原审批快照")
	}
}
