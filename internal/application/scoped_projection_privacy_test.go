package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// scopedPrivacyFixture 构造共享比例项及包含隐藏用票来源的真实审批快照。
// 参数：t为临时测试上下文。
// 返回值：服务、临时库、权威单据、局部受理身份及原始快照。
func scopedPrivacyFixture(t *testing.T) (*Service, *gorm.DB, domain.Claim, domain.Actor, []byte) {
	t.Helper()
	s, db, _ := setupWorkflow(t)
	decl := &domain.TravelDeclaration{OriginCityCode: "110100", DestinationCityCode: "310100",
		DepartureDate: "2026-11-09", ReturnDate: "2026-11-12"}
	cross := &domain.TravelCrossCheck{Declaration: decl, Status: "MISMATCH", ProjectStatus: "MATCH",
		Invoices: []domain.TravelInvoiceCheck{{InvoiceID: "hidden-invoice", SourceIDs: []string{"hidden-source"},
			Status: "MISMATCH", Facts: &domain.InvoiceTravelFacts{Kind: "HOTEL", DestinationCityCode: "440100"}}}}
	c := domain.Claim{ID: "scoped-ratio", TenantID: "t", Applicant: "app", OrgID: "org",
		Title: "共享费用", Status: "IN_APPROVAL", Version: 1, CurrentRound: 1, Currency: "CNY",
		Items: []domain.Item{{ID: "shared", Requested: 10000, AllocationMode: "RATIO",
			AllocationRuleVersion: "largest-remainder-bp-v1", TravelDeclaration: decl,
			Allocations: []domain.Allocation{
				{ProjectID: "p1", ServiceItemID: "s1", Amount: 5000, RatioBasisPoints: 5000},
				{ProjectID: "hidden-project", Amount: 5000, RatioBasisPoints: 5000}},
			InvoiceUsages: []domain.InvoiceUsage{{InvoiceID: "hidden-invoice", Used: 10000}},
			ClassificationEvidence: []domain.InvoiceClassificationEvidence{{
				InvoiceID: "hidden-invoice", Source: "CONTENT", Matches: []domain.InvoiceClassificationMatch{{
					LineName: "hidden-goods-detail", TaxClassificationCode: "9988776655",
				}},
			}},
			TravelAssessment: &domain.TravelAssessment{Status: "MISMATCH", References: []domain.TravelAssessmentReference{
				{ProjectID: "p1", ServiceItemID: "s1", Status: "MISMATCH", CrossCheck: cross},
				{ProjectID: "hidden-project", Status: "MISMATCH", CrossCheck: cross}}}}}}
	task := domain.Task{ID: "scoped-task", ClaimID: c.ID, Round: 1, Node: 1,
		Assignee: "lead", Status: "PENDING", Version: 1, RequiredPermission: "expense.approval.act",
		AllocationKeys: []string{"shared/p1/s1"}}
	plan := approvalPlan{Tasks: []domain.Task{task}, RouteDecision: &routeThresholdDecision{
		Items: []routeThresholdItem{{ItemID: "shared", Requested: 10000, Excess: 7000}}}}
	hash, err := approvedContentHash(c, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ContentHash = hash
	seedQueryClaim(t, db, c, "TRAVEL")
	raw, _ := json.Marshal(task)
	if err = db.Create(&store.Task{ID: task.ID, TenantID: "t", ClaimID: c.ID, Round: 1, Node: 1,
		Assignee: "lead", Status: "PENDING", Version: 1, ContentJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(approvalSnapshot{Claim: c, Approval: plan})
	if err = db.Create(&store.Snapshot{ID: "scoped-snapshot", TenantID: "t", ClaimID: c.ID,
		Round: 1, ContentHash: hash, ContentJSON: string(snapshot)}).Error; err != nil {
		t.Fatal(err)
	}
	a := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{
		"expense.claim.read", "expense.approval.act", "expense.claim.update"}}
	return s, db, c, a, snapshot
}

// TestScopedClaimHidesRatioAndSharedTravelSources 防止通过完整比例反推隐藏金额或读取整项票据来源。
// 参数：t为测试上下文。
// 返回值：无。
func TestScopedClaimHidesRatioAndSharedTravelSources(t *testing.T) {
	s, _, claim, actor, _ := scopedPrivacyFixture(t)
	for _, kind := range []string{"claim", "approval-task"} {
		t.Run(kind, func(t *testing.T) {
			id := claim.ID
			if kind == "approval-task" {
				id = "scoped-task"
			}
			raw, err := s.Query(context.Background(), actor, kind, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			var view map[string]any
			_ = json.Unmarshal(raw, &view)
			if kind == "approval-task" {
				view = view["snapshot"].(map[string]any)
			}
			if view["scope_limited"] != true || view["payable_minor"] != "5000" {
				t.Fatalf("应保留局部范围及授权金额: %+v", view)
			}
			item := view["items"].([]any)[0].(map[string]any)
			allocation := item["allocations"].([]any)[0].(map[string]any)
			if item["requested_minor"] != "5000" || allocation["amount_minor"] != "5000" {
				t.Fatalf("局部费用金额应仍可读: %+v", item)
			}
			for _, field := range []string{"allocation_mode", "allocation_rule_version", "travel_declaration", "classification_evidence"} {
				if _, exists := item[field]; exists {
					t.Fatalf("局部视图泄露完整费用元数据%s: %+v", field, item)
				}
			}
			if _, exists := allocation["ratio_basis_points"]; exists {
				t.Fatalf("局部比例可反推原始整项金额: %+v", allocation)
			}
			encoded, _ := json.Marshal(view)
			for _, secret := range []string{"hidden-invoice", "hidden-source", "hidden-project", "hidden-goods-detail", "9988776655", "route_decision", `"cross_check"`} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("局部视图泄露共享来源%s: %s", secret, encoded)
				}
			}
			assessment := item["travel_assessment"].(map[string]any)
			if assessment["status"] != "UNKNOWN" {
				t.Fatalf("裁掉完整交叉依据后不得保留其评估结果: %+v", assessment)
			}
		})
	}
}

// TestScopedQueryPreservesAuthorityAndRejectsEditing 防止投影清洗回写权威字段或使局部受理人编辑原单。
// 参数：t为测试上下文。
// 返回值：无。
func TestScopedQueryPreservesAuthorityAndRejectsEditing(t *testing.T) {
	s, db, claim, actor, original := scopedPrivacyFixture(t)
	if _, err := s.claimView(db, actor, claim); err != nil {
		t.Fatal(err)
	}
	if claim.Items[0].AllocationMode != "RATIO" || claim.Items[0].Allocations[0].RatioBasisPoints != 5000 ||
		claim.Items[0].TravelDeclaration == nil || claim.Items[0].TravelAssessment.References[0].CrossCheck == nil {
		t.Fatal("局部投影修改了共享的权威内存对象")
	}
	owner := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	raw, err := s.Query(context.Background(), owner, "claim", claim.ID, nil)
	if err != nil || !strings.Contains(string(raw), `"ratio_basis_points":5000`) ||
		!strings.Contains(string(raw), `"travel_declaration"`) || !strings.Contains(string(raw), "hidden-invoice") || !strings.Contains(string(raw), "hidden-goods-detail") {
		t.Fatalf("申请人完整视图应保留权威内容: %s %v", raw, err)
	}
	if _, err = s.Execute(context.Background(), actor, "claim.update", claim.ID,
		json.RawMessage(`{"command_id":"cannot-edit-scoped","expected_version":1,"items":[]}`)); err == nil {
		t.Fatal("局部受理人不得将裁剪视图编辑回原单")
	}
	var snapshot store.Snapshot
	if err = db.First(&snapshot, "id = ?", "scoped-snapshot").Error; err != nil {
		t.Fatal(err)
	}
	if snapshot.ContentJSON != string(original) {
		t.Fatal("读取投影改写了不可变审批快照")
	}
	if err = VerifyApprovalSnapshot(original, snapshot.ContentHash); err != nil {
		t.Fatalf("原始审批快照应仍通过摘要复验: %v", err)
	}
}
