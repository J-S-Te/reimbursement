package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestTravelInvoiceCorrectionRequiresApprovalReconfirmAndNewRound 验证已确认行程经独立更正保留前后证据。
// 参数：t为隔离应用事务测试上下文。
// 返回值：无。
func TestTravelInvoiceCorrectionRequiresApprovalReconfirmAndNewRound(t *testing.T) {
	s, db, project := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	app.Permissions = append(app.Permissions, "expense.invoice.review", "expense.invoice.read")
	setClaimTravelDeclarationJSON(t, db, id, nil)
	setInvoiceTravelJSON(t, db, "invoice", invoiceTravelFactsJSON("RAIL", "110100", "310100", "2026-11-10", "2026-11-10"))
	ref := terminalFixtureReference()
	ref.Eligible, ref.ReasonCodes, ref.TravelSnapshot = true, nil, completeTravelCrossSnapshot(t)
	project.references = []domain.ProjectReference{ref}
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "travel-before-correction", "expected_version": 1})
	var old store.Snapshot
	db.Where("claim_id = ? AND round = ?", id, 1).First(&old)
	proposed := domain.Invoice{InvoiceType: "VAT", InvoiceNumber: "travel-corrected", InvoiceDate: "2026-11-10", Total: 100, FileIDs: []string{"file"},
		TravelFacts: &domain.InvoiceTravelFacts{Kind: "RAIL", OriginCityCode: "110100", DestinationCityCode: "440100", StartDate: "2026-11-10", EndDate: "2026-11-10"}}
	request := runCommand(t, s, app, "invoice.correction.request", "invoice", map[string]any{
		"command_id": "travel-correction-request", "expected_version": 2, "reason": "按原件更正目的地", "proposed_invoice": proposed})
	correctionID := request["id"].(string)
	seedPolicy(t, db, "scopes", domain.Scope{ID: "travel-review-scope", SubjectID: "reviewer", OrgIDs: []string{"org"}})
	reviewer := domain.Actor{TenantID: "t", SubjectID: "reviewer", Permissions: []string{"expense.invoice.read", "expense.invoice.correct", "expense.finance.review", "expense.claim.read"}}
	runCommand(t, s, reviewer, "invoice.correction.decide", correctionID, map[string]any{
		"command_id": "travel-correction-approved", "expected_version": 1, "approve": true, "reason": "独立复核原件同意更正"})
	var row store.Invoice
	db.First(&row, "id = ?", "invoice")
	var invoice domain.Invoice
	json.Unmarshal([]byte(row.ContentJSON), &invoice)
	if invoice.TravelFacts == nil || invoice.TravelFacts.DestinationCityCode != "440100" {
		t.Fatalf("正式更正忽略票面行程: %s", row.ContentJSON)
	}
	claim, _, _ := loadClaim(db, "t", id, false)
	setClaimTravelDeclarationJSON(t, db, id, map[string]any{"decision": "ACKNOWLEDGED_MISMATCH", "reason": "仅确认行程差异，尚未完成票据更正确认", "evidence_file_ids": []string{"travel-proof"}})
	_, err := s.Execute(context.Background(), app, "claim.submit", id, json.RawMessage(`{"command_id":"travel-before-reconfirm","expected_version":3}`))
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "INVOICE_RECONFIRMATION_REQUIRED" {
		t.Fatalf("更正绕过申请人重新确认: %v", err)
	}
	runCommand(t, s, app, "claim.invoice.reconfirm", id, map[string]any{
		"command_id": "travel-correction-reconfirm", "expected_version": claim.Version, "correction_id": correctionID, "reason": "已核对更正后的行程"})
	setClaimTravelDeclarationJSON(t, db, id, map[string]any{"decision": "ACKNOWLEDGED_MISMATCH", "reason": "按实际变更行程申请审核", "evidence_file_ids": []string{"travel-proof"}})
	claim, _, _ = loadClaim(db, "t", id, false)
	runCommand(t, s, app, "claim.submit", id, map[string]any{
		"command_id": "travel-correction-new-round", "expected_version": claim.Version})
	var newSnapshot store.Snapshot
	db.Where("claim_id = ? AND round = ?", id, 2).First(&newSnapshot)
	var frozen approvalSnapshot
	json.Unmarshal([]byte(newSnapshot.ContentJSON), &frozen)
	if len(frozen.Invoices) != 1 || frozen.Invoices[0].TravelFacts.DestinationCityCode != "440100" || frozen.Claim.Items[0].TravelAssessment.Status != "MISMATCH" {
		t.Fatal("新轮次未冻结更正后行程和真实矛盾")
	}
	var retained store.Snapshot
	db.First(&retained, "id = ?", old.ID)
	if retained.ContentJSON != old.ContentJSON || retained.ContentHash != old.ContentHash {
		t.Fatal("更正改写原审批行程")
	}
	if err := VerifyApprovalSnapshot(json.RawMessage(newSnapshot.ContentJSON), newSnapshot.ContentHash); err != nil {
		t.Fatal(err)
	}
	frozen.Invoices[0].TravelFacts.DestinationCityCode = "310100"
	raw, _ := json.Marshal(frozen)
	if err := VerifyApprovalSnapshot(raw, newSnapshot.ContentHash); err == nil {
		t.Fatal("票面行程篡改未触发摘要失败")
	}
}

// TestTravelInvoicePaymentRecheckDetectsNewFacts 验证付款前用批准交叉证据比较而不重造旧结论。
// 参数：t为隔离项目与票面变化测试上下文。
// 返回值：无。
func TestTravelInvoicePaymentRecheckDetectsNewFacts(t *testing.T) {
	for _, change := range []string{"PROJECT", "INVOICE"} {
		t.Run(change, func(t *testing.T) {
			s, db, project := setupWorkflow(t)
			app, id := submissionFixture(t, db, true)
			setClaimTravelDeclarationJSON(t, db, id, nil)
			setInvoiceTravelJSON(t, db, "invoice", invoiceTravelFactsJSON("RAIL", "110100", "310100", "2026-11-10", "2026-11-10"))
			ref := terminalFixtureReference()
			ref.Eligible, ref.ReasonCodes, ref.TravelSnapshot = true, nil, completeTravelCrossSnapshot(t)
			project.references = []domain.ProjectReference{ref}
			runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "travel-approved-facts", "expected_version": 1})
			claim, _, _ := loadClaim(db, "t", id, false)
			if change == "PROJECT" {
				project.references[0].TravelSnapshot.Destination = "440100"
				refreshTravelChangeDigest(t, project.references[0].TravelSnapshot)
			}
			if change == "INVOICE" {
				setInvoiceTravelJSON(t, db, "invoice", invoiceTravelFactsJSON("RAIL", "110100", "440100", "2026-11-10", "2026-11-10"))
			}
			refs, err := project.Validate(context.Background(), app, id, claim.Version, claimReferences(claim))
			if err != nil {
				t.Fatal(err)
			}
			err = validatePaymentProjectPolicies(db, claim, refs)
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != "REOPEN_REQUIRED" {
				t.Fatalf("付款前新增矛盾未要求重开: %v", err)
			}
		})
	}
}
