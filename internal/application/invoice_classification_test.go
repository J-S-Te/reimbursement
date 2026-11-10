package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/j-s-te/reimbursement/internal/domain"
	"strings"
	"testing"
	"time"

	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// TestInvoiceContentSuggestionsUseConfirmedDetails 验证确认明细及已发布规则决定建议而非票种默认。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestInvoiceContentSuggestionsUseConfirmedDetails(t *testing.T) {
	cases := []struct{ name, lines, mappings, fee, status, source string }{
		{"税码前缀", `[{"name":"服务","tax_classification_code":"123456"}]`, `[{"id":"tax","tax_code_prefixes":["123"],"fee_type":"HOTEL","default_claim_type":"TRAVEL","priority":10}]`, "HOTEL", "MATCHED", "CONTENT"},
		{"明细关键词", `[{"name":"住宿服务"}]`, `[{"id":"word","keywords":["住宿"],"fee_type":"HOTEL","priority":1}]`, "HOTEL", "MATCHED", "CONTENT"},
		{"空明细兼容", `[]`, `[]`, "DEFAULT", "MATCHED", "INVOICE_TYPE_DEFAULT"},
		{"无命中兼容", `[{"name":"办公用品"}]`, `[{"id":"word","keywords":["住宿"],"fee_type":"HOTEL"}]`, "DEFAULT", "MATCHED", "INVOICE_TYPE_DEFAULT"},
		{"纯字面非正则", `[{"name":"住宿服务"}]`, `[{"id":"word","keywords":[".*"],"fee_type":"HOTEL"}]`, "DEFAULT", "MATCHED", "INVOICE_TYPE_DEFAULT"},
		{"不同明细混合", `[{"name":"住宿"},{"name":"餐饮"}]`, `[{"id":"hotel","keywords":["住宿"],"fee_type":"HOTEL"},{"id":"meal","keywords":["餐饮"],"fee_type":"MEAL"}]`, "", "NEEDS_REVIEW", "CONTENT"},
		{"同费用同级规则冲突", `[{"name":"住宿服务"}]`, `[{"id":"hotel","keywords":["住宿"],"fee_type":"HOTEL"},{"id":"service","keywords":["服务"],"fee_type":"HOTEL"}]`, "", "NEEDS_REVIEW", "CONTENT"},
		{"同优先级冲突", `[{"name":"住宿餐饮"}]`, `[{"id":"hotel","keywords":["住宿"],"fee_type":"HOTEL"},{"id":"meal","keywords":["餐饮"],"fee_type":"MEAL"}]`, "", "NEEDS_REVIEW", "CONTENT"},
		{"部分明细未命中", `[{"name":"住宿"},{"name":"其他"}]`, `[{"id":"hotel","keywords":["住宿"],"fee_type":"HOTEL"}]`, "", "NEEDS_REVIEW", "CONTENT"},
		{"更高优先级", `[{"name":"住宿餐饮"}]`, `[{"id":"hotel","keywords":["住宿"],"fee_type":"HOTEL","priority":2},{"id":"meal","keywords":["餐饮"],"fee_type":"MEAL","priority":1}]`, "HOTEL", "MATCHED", "CONTENT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			actor, _ := submissionFixture(t, db, true)
			actor.Permissions = []string{"expense.invoice.read", "expense.claim.create"}
			setInvoiceDatePolicyFixture(t, db, time.Now().Format("2006-01-02"), "WARN")
			classificationJSONFixture(t, db, &store.Invoice{}, "invoice", "content", json.RawMessage(`{"lines":`+tc.lines+`}`))
			classificationJSONFixture(t, db, &store.Config{}, "rule", "content_mappings", json.RawMessage(tc.mappings))
			classificationJSONFixture(t, db, &store.Config{}, "rule", "fee_type", "DEFAULT")
			if tc.name == "税码前缀" {
				classificationJSONFixture(t, db, &store.Config{}, "rule", "fee_type", "")
			}
			results, err := s.InvoiceExpenseSuggestions(context.Background(), actor, []string{"invoice"})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(results[0])
			var got map[string]any
			json.Unmarshal(raw, &got)
			if got["fee_type"] != tc.fee || got["classification_status"] != tc.status || got["classification_source"] != tc.source {
				t.Fatalf("未按可信明细归类: %s", raw)
			}
			if tc.source == "CONTENT" && (got["invoice_version"] != float64(1) || got["rule_id"] != "rule" || got["classification_matches"] == nil) {
				t.Fatalf("缺少可追溯归类依据: %s", raw)
			}
			if tc.source == "CONTENT" {
				matches := got["classification_matches"].([]any)
				first := matches[0].(map[string]any)
				if first["content_origin"] != "HUMAN_CONFIRMED" || first["line_index"] != float64(0) || first["line_name"] == "" || first["matched_value"] == "" {
					t.Fatalf("明细来源或命中值丢失: %s", raw)
				}
				if tc.name == "税码前缀" && (first["mapping_id"] != "tax" || first["matched_by"] != "TAX_CODE_PREFIX" || first["matched_value"] != "123" || first["tax_classification_code"] != "123456") {
					t.Fatalf("税码依据错误: %s", raw)
				}
			}
		})
	}
}

// TestInvoiceContentConfigurationRejectsUnsafeMappings 验证规则边界与纯数字税码在保存发布前校验。
// 参数：t 为测试上下文。
// 返回值：无。
func TestInvoiceContentConfigurationRejectsUnsafeMappings(t *testing.T) {
	cases := []string{
		`[{"id":"same","keywords":["住宿"],"fee_type":"HOTEL"},{"id":"same","keywords":["餐饮"],"fee_type":"MEAL"}]`,
		`[{"id":"a","tax_code_prefixes":["12.*"],"fee_type":"HOTEL"}]`,
		`[{"id":"a","keywords":[""],"fee_type":"HOTEL"}]`,
		`[{"id":"a","keywords":["住宿"],"fee_type":"HOTEL","priority":10001}]`,
		`[{"id":"a","keywords":["住宿"],"fee_type":" HOTEL "}]`,
		`[{"id":"a","keywords":["住宿"],"fee_type":"HOTEL","default_claim_type":"BAD"}]`,
		`[{"id":"a","fee_type":"HOTEL"}]`,
		`[{"id":"a","keywords":["` + strings.Repeat("a", 129) + `"],"fee_type":"HOTEL"}]`,
	}
	for _, mappings := range cases {
		raw := `{"invoice_type":"VAT","identity_fields":["invoice_number"],"content_mappings":` + mappings + `}`
		if err := validateConfig("invoice_rules", raw); err == nil {
			t.Errorf("危险规则被接受: %s", mappings)
		}
	}
}

// classificationJSONFixture 仅向隔离夹具写入指定票面或配置JSON字段。
// 参数：t 为测试上下文，db 为隔离库，model 为表类型，id 为主键，key/value 为JSON字段。
// 返回值：无。
func classificationJSONFixture(t *testing.T, db *gorm.DB, model any, id, key string, value any) {
	t.Helper()
	var row struct{ ContentJSON string }
	if err := db.Model(model).Select("content_json").Where("id = ?", id).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(row.ContentJSON), &body); err != nil {
		t.Fatal(err)
	}
	body[key] = value
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(model).Where("id = ?", id).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
}

// TestInvoiceContentConfirmationRetainsOriginalAndHistory 验证录入与确认保留明细、原件和人工来源。
// 参数：t 为隔离业务命令测试上下文。
// 返回值：无。
func TestInvoiceContentConfirmationRetainsOriginalAndHistory(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "invoice_rules", domain.InvoiceRule{ID: "vat", InvoiceType: "VAT", IdentityFields: []string{"invoice_number"}, Active: true})
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload", "expense.invoice.review", "expense.invoice.read"}}
	content := map[string]any{"lines": []any{map[string]any{"name": "住宿服务", "tax_classification_code": "123456", "amount_minor": "80", "tax_minor": "20"}}}
	body := map[string]any{"command_id": "content-create", "invoice_type": "VAT", "invoice_number": "content-1", "invoice_date": "2026-11-10", "total_minor": "100", "file_ids": []string{"file"}, "content": content}
	created := runCommand(t, s, actor, "invoice.create", "", body)
	if created["content"] == nil || created["review_status"] != "PENDING" {
		t.Fatalf("创建明细或复核状态错误: %+v", created)
	}
	id := created["id"].(string)
	body["command_id"], body["expected_version"], body["reason"] = "content-confirm", 1, "核对原件商品服务明细"
	confirmed := runCommand(t, s, actor, "invoice.confirm", id, body)
	if confirmed["content"] == nil || confirmed["review_status"] != "CONFIRMED" || confirmed["verification_status"] != "NOT_CHECKED" {
		t.Fatalf("确认丢失明细或伪造验真: %+v", confirmed)
	}
	var sources, revisions int64
	db.Model(&store.InvoiceSource{}).Where("invoice_id = ?", id).Count(&sources)
	db.Model(&store.InvoiceRevision{}).Where("invoice_id = ?", id).Count(&revisions)
	if sources != 1 || revisions != 1 {
		t.Fatalf("未保留原件或人工历史: sources=%d revisions=%d", sources, revisions)
	}
	body["command_id"], body["expected_version"] = "content-reconfirm", 2
	body["content"] = map[string]any{"lines": []any{map[string]any{"name": "餐饮"}}}
	reconfirmed := runCommand(t, s, actor, "invoice.confirm", id, body)
	if reconfirmed["version"] != float64(3) {
		t.Fatalf("未占用票据不能人工重确认内容: %+v", reconfirmed)
	}
	db.Model(&store.InvoiceRevision{}).Where("invoice_id = ?", id).Count(&revisions)
	if revisions != 2 {
		t.Fatal("内容重确认未追加独立历史")
	}
	db.Model(&store.Invoice{}).Where("id = ?", id).Updates(map[string]any{"reservation_state": "RESERVED", "reservation_claim_id": "claim"})
	body["command_id"], body["expected_version"] = "content-reserved-overwrite", 3
	body["content"] = content
	raw, _ := json.Marshal(body)
	_, err := s.Execute(context.Background(), actor, "invoice.confirm", id, raw)
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "INVOICE_LOCKED" {
		t.Fatalf("占用票据普通确认改写内容: %v", err)
	}
}

// TestInvoiceContentRejectsMaliciousAndExcessiveInput 验证非法明细不能通过创建写入正式票据。
// 参数：t 为隔离数据库命令测试上下文。
// 返回值：无。
func TestInvoiceContentRejectsMaliciousAndExcessiveInput(t *testing.T) {
	cases := []any{
		[]any{map[string]any{"name": strings.Repeat("住", 257)}},
		[]any{map[string]any{"name": "住宿\n服务"}},
		[]any{map[string]any{"name": "住宿", "tax_classification_code": "123<script>"}},
		[]any{map[string]any{"name": "住宿", "amount_minor": "-1"}},
		[]any{map[string]any{"name": "住宿", "tax_minor": "-1"}},
		make([]any, 101),
	}
	for index, lines := range cases {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload"}}
			raw, _ := json.Marshal(map[string]any{"command_id": "bad-content", "file_ids": []string{"file"}, "content": map[string]any{"lines": lines}})
			_, err := s.Execute(context.Background(), actor, "invoice.create", "", raw)
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != "INVALID_INVOICE_CONTENT" {
				t.Fatalf("危险明细未拒绝: %v", err)
			}
			var count int64
			db.Model(&store.Invoice{}).Count(&count)
			if count != 0 {
				t.Fatal("危险明细失败仍留票据")
			}
		})
	}
}

// TestInvoiceContentCorrectionUsesIndependentApproval 验证占用明细经独立批准保留旧快照并要求重新确认。
// 参数：t 为隔离业务事务测试上下文。
// 返回值：无。
func TestInvoiceContentCorrectionUsesIndependentApproval(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	app.Permissions = append(app.Permissions, "expense.invoice.review", "expense.invoice.read")
	setInvoiceDatePolicyFixture(t, db, "2026-11-10", "WARN")
	classificationJSONFixture(t, db, &store.Invoice{}, "invoice", "content", json.RawMessage(`{"lines":[{"name":"住宿"}]}`))
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "content-before-correction", "expected_version": 1})
	var old store.Snapshot
	db.Where("claim_id = ? AND round = ?", id, 1).First(&old)
	proposed := domain.Invoice{InvoiceType: "VAT", InvoiceNumber: "content-corrected", InvoiceDate: "2026-11-10", Total: 100, FileIDs: []string{"file"}, Content: &domain.InvoiceContent{Lines: []domain.InvoiceContentLine{{Name: "餐饮"}}}}
	request := runCommand(t, s, app, "invoice.correction.request", "invoice", map[string]any{"command_id": "content-correction-request", "expected_version": 2, "reason": "核实原件明细为餐饮", "proposed_invoice": proposed})
	correctionID := request["id"].(string)
	seedPolicy(t, db, "scopes", domain.Scope{ID: "review-scope", SubjectID: "reviewer", OrgIDs: []string{"org"}})
	reviewer := domain.Actor{TenantID: "t", SubjectID: "reviewer", Permissions: []string{"expense.invoice.read", "expense.invoice.correct", "expense.finance.review", "expense.claim.read"}}
	runCommand(t, s, reviewer, "invoice.correction.decide", correctionID, map[string]any{"command_id": "content-correction-approved", "expected_version": 1, "approve": true, "reason": "独立复核原件同意"})
	var row store.Invoice
	db.First(&row, "id = ?", "invoice")
	var invoice domain.Invoice
	json.Unmarshal([]byte(row.ContentJSON), &invoice)
	if invoice.Content == nil || invoice.Content.Lines[0].Name != "餐饮" || invoice.VerificationStatus != "NOT_CHECKED" {
		t.Fatalf("正式更正未采用内容或放宽验真: %s", row.ContentJSON)
	}
	var retained store.Snapshot
	db.First(&retained, "id = ?", old.ID)
	if retained.ContentJSON != old.ContentJSON || retained.ContentHash != old.ContentHash {
		t.Fatal("更正改写原审批快照")
	}
	claim, _, _ := loadClaim(db, "t", id, false)
	raw, _ := json.Marshal(map[string]any{"command_id": "content-before-reconfirm", "expected_version": claim.Version})
	_, err := s.Execute(context.Background(), app, "claim.submit", id, raw)
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "INVOICE_RECONFIRMATION_REQUIRED" {
		t.Fatalf("更正绕过申请人重新确认: %v", err)
	}
	runCommand(t, s, app, "claim.invoice.reconfirm", id, map[string]any{"command_id": "content-correction-reconfirm", "expected_version": claim.Version, "correction_id": correctionID, "reason": "已核对更正商品明细"})
	claim, _, _ = loadClaim(db, "t", id, false)
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "content-correction-new-round", "expected_version": claim.Version})
	var next store.Snapshot
	if err := db.Where("claim_id = ? AND round = ?", id, 2).First(&next).Error; err != nil {
		t.Fatal(err)
	}
	var frozen approvalSnapshot
	if err := json.Unmarshal([]byte(next.ContentJSON), &frozen); err != nil {
		t.Fatal(err)
	}
	if len(frozen.Invoices) != 1 || frozen.Invoices[0].Content == nil || frozen.Invoices[0].Content.Lines[0].Name != "餐饮" {
		t.Fatal("新轮次未冻结正式更正明细")
	}
	if err := VerifyApprovalSnapshot(json.RawMessage(next.ContentJSON), next.ContentHash); err != nil {
		t.Fatal(err)
	}
}

// TestInvoiceClassificationEvidenceRebuiltAtDraftSave 验证草稿证据从当前票面规则重建且允许人工调整。
// 参数：t 为隔离命令与持久化测试上下文。
// 返回值：无。
func TestInvoiceClassificationEvidenceRebuiltAtDraftSave(t *testing.T) {
	for _, selected := range []string{"HOTEL", "MEAL"} {
		t.Run(selected, func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			actor, id := submissionFixture(t, db, true)
			actor.Permissions = append(actor.Permissions, "expense.claim.update")
			classificationJSONFixture(t, db, &store.Invoice{}, "invoice", "content", json.RawMessage(`{"lines":[{"name":"住宿服务"}]}`))
			classificationJSONFixture(t, db, &store.Config{}, "rule", "content_mappings", json.RawMessage(`[{"id":"hotel","keywords":["住宿"],"fee_type":"HOTEL"}]`))
			claim, _, _ := loadClaim(db, "t", id, false)
			claim.Items[0].FeeType = selected
			claim.Items[0].ClassificationEvidence = []domain.InvoiceClassificationEvidence{{InvoiceID: "forged", InvoiceVersion: 999, RuleID: "fake", SuggestedFeeType: "ATTACK", ManualOverride: false}}
			raw, _ := json.Marshal(claim)
			var body map[string]any
			json.Unmarshal(raw, &body)
			body["command_id"], body["expected_version"] = "evidence-update", 1
			result := runCommand(t, s, actor, "claim.update", id, body)
			saved, _, err := loadClaim(db, "t", id, false)
			if err != nil {
				t.Fatal(err)
			}
			evidence := saved.Items[0].ClassificationEvidence
			if len(evidence) != 1 {
				t.Fatalf("未重建可信归类证据: %+v result=%+v", evidence, result)
			}
			got := evidence[0]
			if got.InvoiceID != "invoice" || got.InvoiceVersion != 1 || got.RuleID != "rule" || got.RuleVersion != 1 || got.Status != "MATCHED" || got.Source != "CONTENT" || got.SuggestedFeeType != "HOTEL" || got.SelectedFeeType != selected || got.ManualOverride != (selected != "HOTEL") || len(got.Matches) != 1 || got.Matches[0].MappingID != "hotel" {
				t.Fatalf("证据信任客户端或丢失人工改类: %+v", got)
			}
		})
	}
}

// TestInvoiceClassificationEvidenceClearsLegacyForgery 验证旧规则与无票据草稿不保存客户端伪造结论。
// 参数：t 为隔离草稿命令测试上下文。
// 返回值：无。
func TestInvoiceClassificationEvidenceClearsLegacyForgery(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	actor.Permissions = append(actor.Permissions, "expense.claim.update")
	claim, _, _ := loadClaim(db, "t", id, false)
	claim.Items[0].ClassificationEvidence = []domain.InvoiceClassificationEvidence{{InvoiceID: "forged", SuggestedFeeType: "ATTACK"}}
	raw, _ := json.Marshal(claim)
	var body map[string]any
	json.Unmarshal(raw, &body)
	body["command_id"], body["expected_version"] = "legacy-evidence-update", 1
	runCommand(t, s, actor, "claim.update", id, body)
	saved, _, _ := loadClaim(db, "t", id, false)
	if len(saved.Items[0].ClassificationEvidence) != 0 {
		t.Fatalf("旧规则保存伪造建议: %+v", saved.Items[0].ClassificationEvidence)
	}
}

// TestInvoiceClassificationEvidenceBoundsAmplification 验证重复引用去重且大规模服务端证据放大失败关闭。
// 参数：t 为隔离草稿保存测试上下文。
// 返回值：无。
func TestInvoiceClassificationEvidenceBoundsAmplification(t *testing.T) {
	for _, excessive := range []bool{false, true} {
		t.Run(fmt.Sprint(excessive), func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			actor, id := submissionFixture(t, db, true)
			actor.Permissions = append(actor.Permissions, "expense.claim.update")
			mappings := []domain.InvoiceContentMapping{{ID: "hotel", Keywords: []string{"住宿"}, FeeType: "HOTEL"}}
			lines := []domain.InvoiceContentLine{{Name: "住宿服务"}}
			if excessive {
				mappings = nil
				lines = nil
				for index := 0; index < 100; index++ {
					mappings = append(mappings, domain.InvoiceContentMapping{ID: fmt.Sprintf("hotel%d", index), Keywords: []string{"住宿"}, FeeType: "HOTEL"})
				}
				for index := 0; index < 10; index++ {
					lines = append(lines, domain.InvoiceContentLine{Name: "住宿服务"})
				}
			}
			classificationJSONFixture(t, db, &store.Invoice{}, "invoice", "content", domain.InvoiceContent{Lines: lines})
			classificationJSONFixture(t, db, &store.Config{}, "rule", "content_mappings", mappings)
			claim, _, _ := loadClaim(db, "t", id, false)
			if excessive {
				item := claim.Items[0]
				claim.Items = nil
				for index := 0; index < 11; index++ {
					next := item
					next.ID = fmt.Sprint(index)
					claim.Items = append(claim.Items, next)
				}
			} else {
				claim.Items[0].InvoiceUsages = append(claim.Items[0].InvoiceUsages, claim.Items[0].InvoiceUsages[0])
			}
			raw, _ := json.Marshal(claim)
			var body map[string]any
			json.Unmarshal(raw, &body)
			body["command_id"], body["expected_version"] = "bounded-evidence-update", 1
			raw, _ = json.Marshal(body)
			_, err := s.Execute(context.Background(), actor, "claim.update", id, raw)
			saved, _, _ := loadClaim(db, "t", id, false)
			if excessive {
				var problem *domain.Problem
				if !errors.As(err, &problem) || problem.Code != "CLASSIFICATION_EVIDENCE_LIMIT" || saved.Version != 1 {
					t.Fatalf("证据放大未原子拒绝: err=%v version=%d", err, saved.Version)
				}
			} else if err != nil || len(saved.Items[0].ClassificationEvidence) != 1 {
				t.Fatalf("重复用票放大证据: err=%v evidence=%d", err, len(saved.Items[0].ClassificationEvidence))
			}
		})
	}
}
