package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// TestTravelDeclarationRejectsInvalidDatesAndFreeTextCities 验证草稿不能保存畸形结构化行程。
// 参数：t 为隔离命令测试上下文。
// 返回值：无。
func TestTravelDeclarationRejectsInvalidDatesAndFreeTextCities(t *testing.T) {
	cases := []struct{ field, value string }{
		{"departure_date", "2026-02-30"}, {"return_date", "2026-11-08"},
		{"origin_city_code", "北京市"}, {"destination_city_code", "000000"},
	}
	for _, tc := range cases {
		t.Run(tc.field+tc.value, func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			actor, id := submissionFixture(t, db, true)
			actor.Permissions = append(actor.Permissions, "expense.claim.update")
			claim, _, _ := loadClaim(db, "t", id, false)
			raw, _ := json.Marshal(claim)
			var body map[string]any
			json.Unmarshal(raw, &body)
			decl := travelDeclarationJSON()
			decl[tc.field] = tc.value
			body["items"].([]any)[0].(map[string]any)["travel_declaration"] = decl
			body["command_id"], body["expected_version"] = "invalid-declaration", 1
			raw, _ = json.Marshal(body)
			_, err := s.Execute(context.Background(), actor, "claim.update", id, raw)
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Status != 422 {
				t.Fatalf("畸形行程未拒绝: %v", err)
			}
		})
	}
}

// TestInvoiceTravelFactsValidateAndBecomeImmutableAfterConfirmation 验证票面事实需人工确认且不能普通覆盖。
// 参数：t 为隔离票据命令测试上下文。
// 返回值：无。
func TestInvoiceTravelFactsValidateAndBecomeImmutableAfterConfirmation(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "invoice_rules", domain.InvoiceRule{ID: "rail", InvoiceType: "RAIL", IdentityFields: []string{"invoice_number"}, Active: true})
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload", "expense.invoice.review", "expense.invoice.read"}}
	body := map[string]any{"command_id": "travel-invoice-create", "invoice_type": "RAIL", "invoice_number": "rail-1",
		"invoice_date": "2026-11-10", "total_minor": "100", "file_ids": []string{"file"}}
	created := runCommand(t, s, actor, "invoice.create", "", body)
	id := created["id"].(string)
	facts := invoiceTravelFactsJSON("RAIL", "110100", "310100", "2026-11-10", "2026-11-10")
	body["command_id"], body["expected_version"], body["reason"] = "travel-invoice-invalid", 1, "人工核对原件"
	body["travel_facts"] = invoiceTravelFactsJSON("RAIL", "北京市", "310100", "2026-11-10", "2026-11-09")
	raw, _ := json.Marshal(body)
	if _, err := s.Execute(context.Background(), actor, "invoice.confirm", id, raw); err == nil {
		t.Fatal("非法票面行程被确认")
	}
	body["command_id"], body["travel_facts"] = "travel-invoice-confirm", facts
	confirmed := runCommand(t, s, actor, "invoice.confirm", id, body)
	if confirmed["travel_facts"] == nil {
		t.Fatal("人工确认丢失票面行程")
	}
	var sources int64
	db.Model(&store.InvoiceSource{}).Where("invoice_id = ?", id).Count(&sources)
	if sources == 0 {
		t.Fatal("人工票面事实未绑定既有原件来源")
	}
	body["command_id"], body["expected_version"] = "travel-invoice-overwrite", 2
	body["travel_facts"] = invoiceTravelFactsJSON("RAIL", "110100", "440100", "2026-11-10", "2026-11-10")
	raw, _ = json.Marshal(body)
	_, err := s.Execute(context.Background(), actor, "invoice.confirm", id, raw)
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "INVOICE_TRAVEL_FACTS_IMMUTABLE" {
		t.Fatalf("已确认行程被普通覆盖: %v", err)
	}
}

// TestTravelInvoiceMismatchNeedsAcknowledgmentAndDirector 验证票面矛盾不能被客户端MATCH覆盖。
// 参数：t 为真实本地提交事务测试上下文。
// 返回值：无。
func TestTravelInvoiceMismatchNeedsAcknowledgmentAndDirector(t *testing.T) {
	s, db, project := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	setClaimTravelDeclarationJSON(t, db, id, nil)
	setInvoiceTravelJSON(t, db, "invoice", invoiceTravelFactsJSON("RAIL", "110100", "440100", "2026-11-10", "2026-11-10"))
	ref := terminalFixtureReference()
	ref.Eligible, ref.ReasonCodes = true, nil
	ref.TravelSnapshot = completeTravelCrossSnapshot(t)
	project.references = []domain.ProjectReference{ref}
	_, err := s.Execute(context.Background(), actor, "claim.submit", id, json.RawMessage(`{"command_id":"cross-no-ack","expected_version":1}`))
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "TRAVEL_CONFIRMATION_REQUIRED" {
		t.Fatalf("票据矛盾未要求明确确认: %v", err)
	}
	setClaimTravelDeclarationJSON(t, db, id, map[string]any{"decision": "ACKNOWLEDGED_MISMATCH", "reason": "实际行程变更，原件已复核", "evidence_file_ids": []string{"travel-proof"}})
	runCommand(t, s, actor, "claim.submit", id, map[string]any{"command_id": "cross-ack", "expected_version": 1})
	claim, _, _ := loadClaim(db, "t", id, false)
	if !claim.Items[0].TravelException || claim.Items[0].TravelAssessment == nil || claim.Items[0].TravelAssessment.Status != "MISMATCH" {
		t.Fatalf("未冻结真实矛盾: %+v", claim.Items[0])
	}
	var directors int64
	db.Model(&store.Task{}).Where("claim_id = ? AND node_code = ?", id, "TECHNICAL_DIRECTOR").Count(&directors)
	if directors != 1 {
		t.Fatal("已确认差旅矛盾未走总监全路径")
	}
}

// travelDeclarationJSON 提供明确六位城市代码的申请人行程夹具。
// 参数：无。
// 返回值：独立声明JSON字段集合。
func travelDeclarationJSON() map[string]any {
	return map[string]any{"origin_city_code": "110100", "destination_city_code": "310100", "departure_date": "2026-11-09", "return_date": "2026-11-12"}
}

// invoiceTravelFactsJSON 构造人工票面事实JSON，不模拟OCR或税务验真。
// 参数：kind为票面类型，origin/destination为城市代码，start/end为票面起止日。
// 返回值：独立事实JSON字段集合。
func invoiceTravelFactsJSON(kind, origin, destination, start, end string) map[string]any {
	return map[string]any{"kind": kind, "origin_city_code": origin, "destination_city_code": destination, "start_date": start, "end_date": end}
}

// setClaimTravelDeclarationJSON 在专用库准备声明与故意伪造的客户端结论。
// 参数：t/db为隔离上下文，id为草稿，confirmation为可选人工决定。
// 返回值：无。
func setClaimTravelDeclarationJSON(t *testing.T, db *gorm.DB, id string, confirmation map[string]any) {
	t.Helper()
	var row store.Claim
	if err := db.First(&row, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal([]byte(row.ContentJSON), &body)
	item := body["items"].([]any)[0].(map[string]any)
	item["travel_declaration"], item["travel_confirmation"] = travelDeclarationJSON(), confirmation
	item["travel_assessment"], item["travel_exception"] = map[string]any{"status": "MATCH", "cross_check_status": "MATCH"}, false
	raw, _ := json.Marshal(body)
	if err := db.Model(&row).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
}

// setInvoiceTravelJSON 在专用库准备已确认票面事实及真实本地来源关系。
// 参数：t/db为隔离上下文，id为票据，facts为人工票面字段。
// 返回值：无。
func setInvoiceTravelJSON(t *testing.T, db *gorm.DB, id string, facts map[string]any) {
	t.Helper()
	var row store.Invoice
	if err := db.First(&row, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal([]byte(row.ContentJSON), &body)
	body["travel_facts"] = facts
	raw, _ := json.Marshal(body)
	if err := db.Model(&row).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	var inv domain.Invoice
	json.Unmarshal(raw, &inv)
	if err := LinkManualInvoiceSources(db, inv, "人工复核原件上的行程"); err != nil {
		t.Fatal(err)
	}
}

// completeTravelCrossSnapshot 提供可校验摘要及结构化城市日期的权威项目夹具。
// 参数：t为测试上下文。
// 返回值：带正确内容摘要的结构化事实。
func completeTravelCrossSnapshot(t *testing.T) *domain.TravelSnapshot {
	facts := travelFixtureSnapshot("MATCH")
	facts.Origin, facts.Destination = "110100", "310100"
	facts.DepartureDate, facts.ReturnDate = "2026-11-01", "2026-11-20"
	facts.CityCodeStatus, facts.Transport, facts.AccommodationNeed = "MATCH", "TRAIN", "YES"
	facts.ReasonCodes = nil
	refreshTravelChangeDigest(t, facts)
	return facts
}
