package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestCityMappingConfigurationRejectsInvalidCode 验证新增城市配置不能靠未识别类型跳过合法性检查。
// 参数：t 为测试上下文。
// 返回值：无。
func TestCityMappingConfigurationRejectsInvalidCode(t *testing.T) {
	err := validateConfig("city_mappings", `{"org_id":"org","source_city":"上海市","city_code":"arbitrary","city_level":"A","valid_from":"2026-01-01","active":true}`)
	if err == nil {
		t.Fatal("非结构化城市代码仍可保存")
	}
}

// TestCityMappingCanBeManagedWithExplicitPermission 验证新政策种类进入真实受控配置命令。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestCityMappingCanBeManagedWithExplicitPermission(t *testing.T) {
	s, _, _ := setupWorkflow(t)
	actor := domain.Actor{TenantID: "t", SubjectID: "policy-admin", Permissions: []string{"expense.config.manage"}}
	result := runCommand(t, s, actor, "config.save", "city_mappings", map[string]any{"command_id": "city-save", "org_id": "org", "source_city": "上海市", "city_code": "310100", "city_level": "A", "valid_from": "2026-01-01", "active": true})
	if result["id"] == nil {
		t.Fatal("未持久城市映射草稿")
	}
}

// TestStandardSeparatesTransportCategories 验证同费用职级的铁路与航空标准互不冲突且独立选择。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestStandardSeparatesTransportCategories(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	_, id := submissionFixture(t, db, true)
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	body["items"].([]any)[0].(map[string]any)["transport_category"] = "TRAIN"
	raw, _ = json.Marshal(body)
	_ = json.Unmarshal(raw, &c)
	if err = db.Where("kind = ?", "standards").Delete(&store.Config{}).Error; err != nil {
		t.Fatal(err)
	}
	for _, category := range []string{"TRAIN", "AIR"} {
		raw, _ = json.Marshal(map[string]any{"id": category, "version": 1, "org_id": "org", "fee_type": "TRAVEL", "city_level": "", "grade": "G", "unit": "PER_OCCURRENCE", "limit_minor": "200", "valid_from": "2026-01-01", "transport_category": category})
		if err = db.Create(&store.Config{ID: category, TenantID: "t", Kind: "standards", Version: 1, Published: true, ContentJSON: string(raw)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err = applyStandards(db, &c); err != nil {
		t.Fatalf("独立交通分类发生冲突: %v", err)
	}
	if c.Items[0].StandardVersionID != "TRAIN" {
		t.Fatal("铁路费用误用航空标准")
	}
}

// TestNoInvoicePolicyRequiresIndependentProof 验证允许无票并不代表只写理由就满足证明政策。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestNoInvoicePolicyRequiresIndependentProof(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	_, id := submissionFixture(t, db, true)
	c, _, _ := loadClaim(db, "t", id, false)
	c.Items[0].InvoiceUsages = nil
	c.Items[0].NoInvoiceReason = "无法取得发票，已说明实际费用"
	raw := `{"id":"standard","org_id":"org","fee_type":"TRAVEL","grade":"G","unit":"PER_OCCURRENCE","limit_minor":"200","valid_from":"2026-01-01","allow_no_invoice":true,"require_no_invoice_proof":true,"no_invoice_proof_types":["PAYMENT_PROOF"]}`
	if err := db.Model(&store.Config{}).Where("id = ?", "standard").Update("content_json", raw).Error; err != nil {
		t.Fatal(err)
	}
	err := applyStandards(db, &c)
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "NO_INVOICE_PROOF_REQUIRED" {
		t.Fatalf("缺独立证明仍放行: %v", err)
	}
}

// TestCityMappingConfigurationRequiresPermission 验证非管理人员不能直接保存城市政策。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestCityMappingConfigurationRequiresPermission(t *testing.T) {
	s, _, _ := setupWorkflow(t)
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	_, err := s.Execute(context.Background(), actor, "config.save", "city_mappings", json.RawMessage(`{"command_id":"unauthorized-city"}`))
	if err == nil {
		t.Fatal("无配置能力仍能保存城市政策")
	}
}
