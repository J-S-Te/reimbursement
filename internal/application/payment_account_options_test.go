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

// TestPaymentAccountConfigurationRejectsRawBankData 验证付款目录拒绝缺失归属及银行账号原文。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestPaymentAccountConfigurationRejectsRawBankData(t *testing.T) {
	for _, raw := range []string{
		`{"name":"公司付款","account_ref":"ledger:main","active":true}`,
		`{"name":"公司付款","finance_entity_id":"entity","account_ref":"6222021234567891234","active":true}`,
		`{"name":"公司付款","finance_entity_id":"entity","account_ref":"ledger:main","bank_account":"6222021234567891234","active":true}`,
	} {
		if err := validateConfig("payment_accounts", raw); err == nil {
			t.Fatalf("非法付款目录仍可保存: %s", raw)
		}
	}
}

// seedPaymentSelectionAccount 装载仅当前临时库使用的真实目录夹具。
// 参数：t/db 为测试上下文和隔离库。
// 返回值：无。
func seedPaymentSelectionAccount(t *testing.T, db *gorm.DB) {
	t.Helper()
	var count int64
	if err := db.Model(&store.Config{}).Where("kind = ?", "payment_accounts").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		seedPolicy(t, db, "payment_accounts", domain.PaymentAccount{ID: "selection-source",
			Name: "公司日常付款", FinanceEntityID: "entity", AccountRef: "source", Active: true})
	}
}

// TestPaymentOptionsAreScopedToPayerAndApplicant 验证候选来自主体目录和申请人有效收款档案。
// 参数：t 为真实隔离 SQLite 测试上下文。
// 返回值：无。
func TestPaymentOptionsAreScopedToPayerAndApplicant(t *testing.T) {
	s, db, c, _, _ := approvedControlFixture(t)
	seedPaymentSelectionAccount(t, db)
	seedPolicy(t, db, "payment_accounts", domain.PaymentAccount{ID: "other-payer",
		Name: "其他主体", FinanceEntityID: "another", AccountRef: "ledger:other", Active: true})
	seedPolicy(t, db, "payment_accounts", domain.PaymentAccount{ID: "inactive-source",
		Name: "已停用", FinanceEntityID: "entity", AccountRef: "ledger:inactive"})
	seedPolicy(t, db, "payee_profiles", domain.PayeeProfile{ID: "alternate-payee", SubjectID: c.Applicant,
		AccountRef: "payee:alternate", Version: 1, Active: true, Reviewed: true})
	seedPolicy(t, db, "payee_profiles", domain.PayeeProfile{ID: "other-user-payee", SubjectID: "outsider",
		AccountRef: "payee:other", Version: 1, Active: true, Reviewed: true})
	a := domain.Actor{TenantID: "t", SubjectID: "cashier",
		Permissions: []string{"expense.claim.read", "expense.payment.exception.record"}}
	raw, err := s.PaymentOptions(context.Background(), a, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Sources []domain.PaymentAccount `json:"source_accounts"`
		Payees  []domain.PayeeProfile   `json:"payee_profiles"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 1 || result.Sources[0].AccountRef != "source" || len(result.Payees) != 2 {
		t.Fatalf("候选泄漏其他主体或未保留实际收款选择: %s", raw)
	}
}

// TestPaymentOptionsRejectUnauthorizedAndOrdinaryWrongState 验证授权和状态不能由前端选择控件替代。
// 参数：t 为真实隔离 SQLite 测试上下文。
// 返回值：无。
func TestPaymentOptionsRejectUnauthorizedAndOrdinaryWrongState(t *testing.T) {
	s, db, c, _, _ := approvedControlFixture(t)
	seedPaymentSelectionAccount(t, db)
	for _, a := range []domain.Actor{
		{TenantID: "other", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.record"}},
		{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read"}},
		{TenantID: "t", SubjectID: "outsider", Permissions: []string{"expense.claim.read", "expense.payment.record"}},
		{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.payment.record"}},
		{TenantID: "t", SubjectID: "acct", Permissions: []string{"expense.claim.read", "expense.payment.record"}},
	} {
		if _, err := s.PaymentOptions(context.Background(), a, c.ID); err == nil {
			t.Fatal("越界身份仍获得付款选项")
		}
	}
	c.Status = "DRAFT"
	raw, _ := json.Marshal(c)
	if err := db.Model(&store.Claim{}).Where("id = ?", c.ID).
		Updates(map[string]any{"content_json": string(raw), "status": c.Status}).Error; err != nil {
		t.Fatal(err)
	}
	a := domain.Actor{TenantID: "t", SubjectID: "cashier",
		Permissions: []string{"expense.claim.read", "expense.payment.record"}}
	if _, err := s.PaymentOptions(context.Background(), a, c.ID); err == nil {
		t.Fatal("普通付款在草稿状态仍取得可选账户")
	}
}

// TestFormalPaymentRejectsUnknownAndInactiveSource 验证正式命令不能绕过真实目录。
// 参数：t 为真实隔离 SQLite 测试上下文。
// 返回值：无。
func TestFormalPaymentRejectsUnknownAndInactiveSource(t *testing.T) {
	s, db, c, _, _ := approvedControlFixture(t)
	seedPaymentSelectionAccount(t, db)
	seedPolicy(t, db, "payment_accounts", domain.PaymentAccount{ID: "disabled-source",
		Name: "已停用", FinanceEntityID: "entity", AccountRef: "ledger:disabled"})
	seedPolicy(t, db, "payment_accounts", domain.PaymentAccount{ID: "foreign-source",
		Name: "其他付款主体", FinanceEntityID: "foreign", AccountRef: "ledger:foreign", Active: true})
	a := domain.Actor{TenantID: "t", SubjectID: "cashier",
		Permissions: []string{"expense.claim.read", "expense.payment.record"}}
	for _, reference := range []string{"unknown", "ledger:disabled", "ledger:foreign"} {
		body := bindingPaymentBody("reject-"+reference, c.Version, "txn-unknown")
		body["source_account_ref"] = reference
		raw, _ := json.Marshal(body)
		_, err := s.Execute(context.Background(), a, "payment.record", c.ID, raw)
		var problem *domain.Problem
		if !errors.As(err, &problem) || problem.Code != "PAYMENT_ACCOUNT_UNAVAILABLE" {
			t.Fatalf("未知或失效账户未被正式拒绝: %v", err)
		}
	}
	var count int64
	if err := db.Model(&store.Payment{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("拒绝账户后仍持久化了付款")
	}
}

// TestPaymentAccountDirectoryPublishesThroughCommands 验证目录采用已有权限、版本及发布机制。
// 参数：t 为真实隔离 SQLite 测试上下文。
// 返回值：无。
func TestPaymentAccountDirectoryPublishesThroughCommands(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "finance_entities", domain.FinanceEntity{ID: "entity", Name: "付款主体",
		TaxID: "tax", OrgIDs: []string{"org"}, Active: true})
	a := domain.Actor{TenantID: "t", SubjectID: "admin",
		Permissions: []string{"expense.config.manage", "expense.policy.publish"}}
	body := map[string]any{"command_id": "account-save", "name": "公司付款账户",
		"finance_entity_id": "entity", "account_ref": "ledger:main", "active": true}
	draft := runCommand(t, s, a, "config.save", "payment_accounts", body)
	runCommand(t, s, a, "config.publish", draft["id"].(string), map[string]any{
		"command_id": "account-publish", "kind": "payment_accounts", "expected_version": 1})
	rows, err := paymentAccountChoices(db, domain.Claim{TenantID: "t", OrgID: "org", FinanceEntityID: "entity"})
	found := false
	for _, row := range rows {
		found = found || row.AccountRef == "ledger:main"
	}
	if err != nil || !found {
		t.Fatalf("发布账户未进入真实目录: %v", err)
	}
}

// TestPaymentSelectionRetirementIsRecheckedAtCommand 验证选择后退休的账户不能继续用于正式付款。
// 参数：t 为真实隔离 SQLite 测试上下文。
// 返回值：无。
func TestPaymentSelectionRetirementIsRecheckedAtCommand(t *testing.T) {
	s, db, c, _, _ := approvedControlFixture(t)
	seedPaymentSelectionAccount(t, db)
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier",
		Permissions: []string{"expense.claim.read", "expense.payment.record"}}
	if _, err := s.PaymentOptions(context.Background(), cashier, c.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := configs(db, "t", "payment_accounts", true)
	if err != nil || len(rows) != 1 {
		t.Fatal("账户夹具不完整")
	}
	admin := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.policy.publish"}}
	runCommand(t, s, admin, "config.retire", rows[0].ID, map[string]any{
		"command_id": "retire-selected-source", "kind": "payment_accounts",
		"expected_version": 1, "reason": "公司停用此付款来源"})
	raw, _ := json.Marshal(bindingPaymentBody("stale-selected-source", c.Version, "stale-txn"))
	_, err = s.Execute(context.Background(), cashier, "payment.record", c.ID, raw)
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "PAYMENT_ACCOUNT_UNAVAILABLE" {
		t.Fatalf("退休账户仍可正式付款: %v", err)
	}
}

// TestPaymentAccountPolicyHasLifecycle 验证付款目录参与已有不可变版本和冲突检测。
// 参数：t 为测试上下文。
// 返回值：无。
func TestPaymentAccountPolicyHasLifecycle(t *testing.T) {
	raw := `{"name":"公司付款","finance_entity_id":"entity","account_ref":"ledger:main","active":true}`
	if err := validateConfig("payment_accounts", raw); err != nil {
		t.Fatal(err)
	}
	_, _, _, active, err := policyShape("payment_accounts", raw)
	if err != nil || !active {
		t.Fatalf("付款目录没有有效生命周期: %v", err)
	}
}
