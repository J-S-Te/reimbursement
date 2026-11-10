package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestTravelInvoiceUnknownNeverBecomesMatch 验证缺票面、缺声明或项目自由文均保留UNKNOWN。
// 参数：t为独立数据库提交测试上下文。
// 返回值：无。
func TestTravelInvoiceUnknownNeverBecomesMatch(t *testing.T) {
	for _, mode := range []string{"MISSING_INVOICE_FACTS", "PROJECT_FREE_TEXT", "MISSING_DECLARATION"} {
		t.Run(mode, func(t *testing.T) {
			s, db, project := setupWorkflow(t)
			actor, id := submissionFixture(t, db, true)
			setClaimTravelDeclarationJSON(t, db, id, nil)
			if mode != "MISSING_INVOICE_FACTS" {
				setInvoiceTravelJSON(t, db, "invoice", invoiceTravelFactsJSON("RAIL", "110100", "310100", "2026-11-10", "2026-11-10"))
			}
			ref := terminalFixtureReference()
			ref.Eligible, ref.ReasonCodes, ref.TravelSnapshot = true, nil, completeTravelCrossSnapshot(t)
			if mode == "PROJECT_FREE_TEXT" {
				ref.TravelSnapshot.Origin, ref.TravelSnapshot.Destination = "北京市", "上海市"
				refreshTravelChangeDigest(t, ref.TravelSnapshot)
			}
			project.references = []domain.ProjectReference{ref}
			claim, _, _ := loadClaim(db, "t", id, false)
			if mode == "MISSING_DECLARATION" {
				claim.Items[0].TravelDeclaration = nil
			}
			claim.Items[0].TravelConfirmation = &domain.TravelConfirmation{Decision: "ACKNOWLEDGED_MISMATCH", Reason: "不能把未知写成匹配", EvidenceFileIDs: []string{"travel-proof"}}
			raw, _ := json.Marshal(claim)
			db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw))
			_, err := s.Execute(context.Background(), actor, "claim.submit", id, json.RawMessage(`{"command_id":"unknown-wrong-decision","expected_version":1}`))
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != "TRAVEL_CONFIRMATION_REQUIRED" {
				t.Fatalf("UNKNOWN接受错误确认: %v", err)
			}
			claim.Items[0].TravelConfirmation.Decision = "CONFIRMED_UNKNOWN"
			raw, _ = json.Marshal(claim)
			db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw))
			runCommand(t, s, actor, "claim.submit", id, map[string]any{"command_id": "unknown-confirmed", "expected_version": 1})
			claim, _, _ = loadClaim(db, "t", id, false)
			a := claim.Items[0].TravelAssessment
			if a == nil || a.Status != "UNKNOWN" || a.CrossCheckStatus != "UNKNOWN" || !a.ConfirmedUnknown || claim.Items[0].TravelException {
				t.Fatalf("未知被改MATCH或误判差旅违规: %+v", a)
			}
		})
	}
}

// TestTravelInvoiceCompleteMatchAndLegacyCompatibility 验证新完整匹配及无新字段旧范围互不混淆。
// 参数：t为真实本地提交测试上下文。
// 返回值：无。
func TestTravelInvoiceCompleteMatchAndLegacyCompatibility(t *testing.T) {
	for _, mode := range []string{"COMPLETE", "LEGACY_TRAVEL", "LEGACY_OTHER"} {
		t.Run(mode, func(t *testing.T) {
			s, db, project := setupWorkflow(t)
			actor, id := submissionFixture(t, db, true)
			facts := completeTravelCrossSnapshot(t)
			if mode == "LEGACY_TRAVEL" {
				enableTravelFixture(t, s, id, nil, facts)
			}
			if mode == "COMPLETE" {
				setClaimTravelDeclarationJSON(t, db, id, nil)
				setInvoiceTravelJSON(t, db, "invoice", invoiceTravelFactsJSON("RAIL", "110100", "310100", "2026-11-10", "2026-11-10"))
			}
			ref := terminalFixtureReference()
			ref.Eligible, ref.ReasonCodes, ref.TravelSnapshot = true, nil, facts
			project.references = []domain.ProjectReference{ref}
			runCommand(t, s, actor, "claim.submit", id, map[string]any{"command_id": "travel-mode-submit", "expected_version": 1})
			claim, _, _ := loadClaim(db, "t", id, false)
			a := claim.Items[0].TravelAssessment
			if mode == "LEGACY_OTHER" {
				if a != nil {
					t.Fatalf("旧普通项被强制行程: %+v", a)
				}
				return
			}
			want := "MATCH"
			if mode == "LEGACY_TRAVEL" {
				want = "NOT_REQUESTED_LEGACY"
			}
			if a == nil || a.Status != "MATCH" || a.CrossCheckStatus != want {
				t.Fatalf("新旧核验范围混淆: %+v", a)
			}
			if mode == "COMPLETE" {
				invoice := a.References[0].CrossCheck.Invoices[0]
				if invoice.SourceKind != "MANUAL_FACE_CONFIRMATION" || len(invoice.SourceIDs) == 0 || invoice.FactDigest == "" {
					t.Fatal("匹配结论丢失人工原件来源")
				}
			}
		})
	}
}

// TestTravelInvoiceConnectionsAndHotelHalfOpenIntervals 验证联程不误报同时冲突且住宿按半开晚数。
// 参数：t为结构化事实核验测试上下文。
// 返回值：无。
func TestTravelInvoiceConnectionsAndHotelHalfOpenIntervals(t *testing.T) {
	d := &domain.TravelDeclaration{OriginCityCode: "110100", DestinationCityCode: "310100", DepartureDate: "2026-11-09", ReturnDate: "2026-11-12"}
	first := domain.TravelInvoiceCheck{Facts: &domain.InvoiceTravelFacts{Kind: "AIR", OriginCityCode: "110100", DestinationCityCode: "440100", StartDate: "2026-11-10", EndDate: "2026-11-10"}}
	second := domain.TravelInvoiceCheck{Facts: &domain.InvoiceTravelFacts{Kind: "RAIL", OriginCityCode: "440100", DestinationCityCode: "310100", StartDate: "2026-11-10", EndDate: "2026-11-10"}}
	checks := []domain.TravelInvoiceCheck{first, second}
	for _, check := range checks {
		status, reasons := compareDeclaredInvoiceTravel(d, check.Facts, checks)
		if status != "UNKNOWN" || len(reasons) != 1 || reasons[0] != "INVOICE_MULTI_LEG_REQUIRES_CONFIRMATION" {
			t.Fatalf("同日联程被误判冲突或猜MATCH: %s %+v", status, reasons)
		}
	}
	checks[0].Facts = &domain.InvoiceTravelFacts{Kind: "HOTEL", DestinationCityCode: "310100", StartDate: "2026-11-09", EndDate: "2026-11-10"}
	checks[1].Facts = &domain.InvoiceTravelFacts{Kind: "HOTEL", DestinationCityCode: "440100", StartDate: "2026-11-10", EndDate: "2026-11-11"}
	if conflictingHotelNights(checks) {
		t.Fatal("同日退房入住被误判晚数重叠")
	}
	checks[0].Facts.EndDate = "2026-11-11"
	if !conflictingHotelNights(checks) {
		t.Fatal("不同城市真实住宿晚数重叠未识别")
	}
	item := domain.Item{OccurredDate: "2026-11-10", TravelDeclaration: d}
	ref := domain.ProjectReference{TravelSnapshot: completeTravelCrossSnapshot(t)}
	base := crossCheckTravelReference(item, ref, domain.TravelAssessmentReference{Status: "MATCH"}, checks)
	if base.Status != "MISMATCH" || !strings.Contains(strings.Join(base.ReasonCodes, ","), "INVOICE_HOTEL_OVERLAP") {
		t.Fatalf("跨票矛盾未合并: %+v", base)
	}
}

// TestTravelInvoiceScopePreservesOnlyVisibleCrossChecks 验证任务范围过滤不泄露其他项目核验原因。
// 参数：t为无外部依赖的范围测试上下文。
// 返回值：无。
func TestTravelInvoiceScopePreservesOnlyVisibleCrossChecks(t *testing.T) {
	a := domain.TravelAssessment{References: []domain.TravelAssessmentReference{
		{ProjectID: "visible", Status: "MATCH", CrossCheck: &domain.TravelCrossCheck{Status: "MATCH"}},
		{ProjectID: "hidden", Status: "MISMATCH", ReasonCodes: []string{"HIDDEN_PROJECT_REASON"}, CrossCheck: &domain.TravelCrossCheck{Status: "MISMATCH"}},
	}}
	out := scopedTravelAssessment(a, []domain.Allocation{{ProjectID: "visible"}})
	if out.Status != "MATCH" || out.CrossCheckStatus != "MATCH" || len(out.References) != 1 || len(out.ReasonCodes) != 0 {
		t.Fatalf("范围外项目影响可见结论: %+v", out)
	}
}

// TestTravelClaimEditingKeepsDeclarationAndDropsForgedAssessment 验证声明可编辑而客户端结论不入库。
// 参数：t为隔离草稿命令测试上下文。
// 返回值：无。
func TestTravelClaimEditingKeepsDeclarationAndDropsForgedAssessment(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	actor.Permissions = append(actor.Permissions, "expense.claim.update")
	setClaimTravelDeclarationJSON(t, db, id, nil)
	claim, _, _ := loadClaim(db, "t", id, false)
	raw, _ := json.Marshal(claim)
	var body map[string]any
	json.Unmarshal(raw, &body)
	body["command_id"], body["expected_version"] = "drop-forged-cross-check", 1
	runCommand(t, s, actor, "claim.update", id, body)
	claim, _, _ = loadClaim(db, "t", id, false)
	if claim.Items[0].TravelDeclaration == nil || claim.Items[0].TravelAssessment != nil || claim.Items[0].RequiresTravelFacts {
		t.Fatalf("草稿丢声明或保留伪造服务器核验: %+v", claim.Items[0])
	}
}

// TestTravelInvoiceFactsRequireOriginalSource 验证已确认人工事实不能脱离既有本人原件来源。
// 参数：t为独立测试库上下文。
// 返回值：无。
func TestTravelInvoiceFactsRequireOriginalSource(t *testing.T) {
	s, db, project := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	setClaimTravelDeclarationJSON(t, db, id, nil)
	setInvoiceTravelJSON(t, db, "invoice", invoiceTravelFactsJSON("RAIL", "110100", "310100", "2026-11-10", "2026-11-10"))
	if err := db.Where("invoice_id = ?", "invoice").Delete(&store.InvoiceSource{}).Error; err != nil {
		t.Fatal(err)
	}
	ref := terminalFixtureReference()
	ref.Eligible, ref.ReasonCodes, ref.TravelSnapshot = true, nil, completeTravelCrossSnapshot(t)
	project.references = []domain.ProjectReference{ref}
	_, err := s.Execute(context.Background(), actor, "claim.submit", id, json.RawMessage(`{"command_id":"missing-travel-source","expected_version":1}`))
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "INVOICE_TRAVEL_SOURCE_REQUIRED" {
		t.Fatalf("无来源的人工票面事实产生核验结论: %v", err)
	}
}
