package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestOwnInvoiceOptionsNeverExposeSharedInvoices 验证本人票据选择不继承财务共享读取范围。
// 参数：t 为测试上下文。
// 返回值：无；跨用户、跨租户或字面搜索错误时失败。
func TestOwnInvoiceOptionsNeverExposeSharedInvoices(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	for _, row := range []store.Invoice{
		{ID: "own", TenantID: "t", Owner: "app", ContentJSON: `{"invoice_number":"票%001","invoice_date":"2026-11-11"}`},
		{ID: "other", TenantID: "t", Owner: "other", ContentJSON: `{"invoice_number":"票%002","invoice_date":"2026-11-11"}`},
		{ID: "tenant", TenantID: "other", Owner: "app", ContentJSON: `{"invoice_number":"票%003","invoice_date":"2026-11-11"}`},
		{ID: "wildcard", TenantID: "t", Owner: "app", ContentJSON: `{"invoice_number":"票004","invoice_date":"2026-11-10"}`},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read", "expense.scope.tenant_all", "expense.finance.review"}}
	for _, keyword := range []string{"%", "2026-11-11"} {
		raw, err := s.OwnInvoiceOptions(context.Background(), a, keyword, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Items []domain.NameOption `json:"items"`
			Total int                 `json:"total"`
		}
		json.Unmarshal(raw, &page)
		if len(page.Items) != 1 || page.Items[0].ID != "own" || page.Total != 1 || page.Items[0].Name == "" {
			t.Fatalf("本人票据选项越界: %s", raw)
		}
	}
	a.Permissions = nil
	if _, err := s.OwnInvoiceOptions(context.Background(), a, "", 1, 20); err == nil {
		t.Fatal("无读票权限未拒绝")
	}
}

// TestOrganizationOptionsUseRealAppointmentsAndPolicyScope 验证组织名称只来自真实任职且政策目标受范围限制。
// 参数：t 为测试上下文。
// 返回值：无；普通用户查询他人、伪造组织名称或政策范围扩大时失败。
func TestOrganizationOptionsUseRealAppointmentsAndPolicyScope(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	s.deps.Personnel = &draftDirectoryFixture{person: domain.PersonOption{ID: "app", Name: "张申请",
		Organizations: []domain.OrganizationOption{{ID: "org", Name: "真实本人部门", IsPrimary: true}},
	}}
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.create"}}
	raw, err := s.OrganizationOptions(context.Background(), a, "", "真实", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items []domain.NameOption `json:"items"`
		Total int                 `json:"total"`
	}
	json.Unmarshal(raw, &page)
	if page.Total != 1 || page.Items[0].Name != "真实本人部门" {
		t.Fatalf("本人组织来源错误: %s", raw)
	}
	if _, err = s.OrganizationOptions(context.Background(), a, "other", "", 1, 20); err == nil {
		t.Fatal("普通申请人可查他人组织")
	}
	s.deps.Personnel = &selectionDirectoryFixture{people: map[string]domain.PersonOption{
		"other": {ID: "other", Name: "李人员", Organizations: []domain.OrganizationOption{
			{ID: "org", Name: "授权部门"}, {ID: "foreign", Name: "范围外部门"},
		}},
	}}
	a.Permissions = []string{"expense.config.manage"}
	if _, err = s.OrganizationOptions(context.Background(), a, "other", "", 1, 20); err == nil {
		t.Fatal("管理能力替代了组织范围")
	}
	seedPolicy(t, db, "scopes", domain.Scope{ID: "option-scope", SubjectID: "app", OrgIDs: []string{"org"}})
	raw, err = s.OrganizationOptions(context.Background(), a, "other", "", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &page)
	if page.Total != 1 || page.Items[0].ID != "org" || page.Items[0].Name != "授权部门" {
		t.Fatalf("政策组织范围越界: %s", raw)
	}
}

// TestConfigurationOptionsKeepBusinessNamesPrivate 验证配置选择返回业务名称而非外部账户引用。
// 参数：t 为测试上下文。
// 返回值：无；未发布、其它租户或账户引用作为名称返回时失败。
func TestConfigurationOptionsKeepBusinessNamesPrivate(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	if err := db.Where("kind = ?", "payment_accounts").Delete(&store.Config{}).Error; err != nil {
		t.Fatal(err)
	}
	seedPolicy(t, db, "finance_entities", domain.FinanceEntity{ID: "entity", Name: "付款公司", Active: true})
	seedPolicy(t, db, "payment_accounts", domain.PaymentAccount{ID: "account", Name: "基本户", AccountRef: "external-sensitive-ref", Active: true})
	seedPolicy(t, db, "payee_profiles", map[string]any{"id": "payee", "name": "本人收款资料", "subject_id": "app", "active": true, "account_ref": "never-a-label"})
	for _, row := range []store.Config{
		{ID: "draft-entity", TenantID: "t", Kind: "finance_entities", ContentJSON: `{"name":"未发布公司","active":true}`},
		{ID: "foreign-entity", TenantID: "foreign", Kind: "finance_entities", Published: true, ContentJSON: `{"name":"其它租户公司","active":true}`},
		{ID: "disabled-entity", TenantID: "t", Kind: "finance_entities", Published: true, ContentJSON: `{"name":"停用公司","active":false}`},
		{ID: "retired-entity", TenantID: "t", Kind: "finance_entities", Published: true, ContentJSON: `{"name":"已退休公司","active":true}`},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&store.PolicyLifecycle{ID: "retirement", TenantID: "t", ConfigID: "retired-entity", EffectiveAt: time.Now().UTC().Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.config.manage"}}
	for kind, want := range map[string]string{"finance_entities": "付款公司", "payment_accounts": "基本户", "payee_profiles": "本人收款资料"} {
		raw, err := s.ConfigurationOptions(context.Background(), a, kind, "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Items []domain.NameOption `json:"items"`
			Total int                 `json:"total"`
		}
		json.Unmarshal(raw, &page)
		if page.Total != 1 || page.Items[0].Name != want {
			t.Fatalf("配置业务标签错误: %s", raw)
		}
	}
	a.Permissions = nil
	if _, err := s.ConfigurationOptions(context.Background(), a, "finance_entities", "", 1, 20); err == nil {
		t.Fatal("普通用户可读取管理配置选项")
	}
}

// TestLegacyPayeeConfigurationOptionUsesRealPersonName 验证旧收款档案名称不会回退成外部账户引用。
// 参数：t 为测试上下文。
// 返回值：无；未命名资料不能解析真实姓名或泄漏外部引用时失败。
func TestLegacyPayeeConfigurationOptionUsesRealPersonName(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	s.deps.Personnel = &selectionDirectoryFixture{people: map[string]domain.PersonOption{
		"app": {ID: "app", Name: "张申请"},
	}}
	seedPolicy(t, db, "payee_profiles", domain.PayeeProfile{ID: "old-payee", SubjectID: "app", AccountRef: "private-reference", Active: true})
	a := domain.Actor{TenantID: "t", SubjectID: "operator", Permissions: []string{"expense.config.manage"}}
	raw, err := s.ConfigurationOptions(context.Background(), a, "payee_profiles", "张", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items []domain.NameOption `json:"items"`
		Total int                 `json:"total"`
	}
	json.Unmarshal(raw, &page)
	if page.Total != 1 || page.Items[0].Name != "张申请 · 收款资料（版本1）" {
		t.Fatalf("旧资料未使用真实姓名版本: %s", raw)
	}
}
