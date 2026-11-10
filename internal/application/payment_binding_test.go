package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// bindingPaymentBody 创建真实付款命令的最小证据载荷。
// 参数：command 为幂等键，version 为单据版本，transaction 为银行流水。
// 返回值：使用相同批准金额和收款资料的请求。
func bindingPaymentBody(command string, version int64, transaction string) map[string]any {
	return map[string]any{"command_id": command, "expected_version": version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": transaction, "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}}
}

// TestNoPaymentReleasesBindingWithoutChangingHistory 验证误填流水经独立批准可复用且旧重放不释放新占用。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestNoPaymentReleasesBindingWithoutChangingHistory(t *testing.T) {
	s, db, c, _, head := approvedControlFixture(t)
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record", "expense.payment.reversal.request"}}
	head.Permissions = append(head.Permissions, "expense.payment.reversal.approve")
	paid := runCommand(t, s, cashier, "payment.record", c.ID, bindingPaymentBody("binding-paid", c.Version, "mistyped"))
	var original store.Payment
	if err := db.First(&original, "id = ?", paid["id"]).Error; err != nil {
		t.Fatal(err)
	}
	request := runCommand(t, s, cashier, "reversal.request", paid["id"].(string), map[string]any{"command_id": "binding-request", "expected_version": c.Version + 1, "reason": "误填流水，实际未付", "fact_kind": "NO_PAYMENT", "evidence_file_ids": []string{"proof"}})
	decision := map[string]any{"command_id": "binding-approve", "expected_version": 1, "approve": true, "reason": "独立核实实际未付款"}
	runCommand(t, s, head, "reversal.decide", request["id"].(string), decision)
	repaid := runCommand(t, s, cashier, "payment.record", c.ID, bindingPaymentBody("binding-repaid", c.Version+2, "mistyped"))
	runCommand(t, s, head, "reversal.decide", request["id"].(string), decision)
	var retained store.Payment
	if err := db.First(&retained, "id = ?", original.ID).Error; err != nil {
		t.Fatal(err)
	}
	if original.ContentJSON != retained.ContentJSON || original.TransactionNo != retained.TransactionNo {
		t.Fatal("纠错改写旧付款事实")
	}
	var owner string
	if err := db.Table("exp_payment_binding").Select("payment_id").Where("tenant_id = ? AND transaction_no = ?", "t", "mistyped").Scan(&owner).Error; err != nil {
		t.Fatal(err)
	}
	if owner != repaid["id"] {
		t.Fatal("旧冲正重放清除了后来的同流水绑定")
	}
}

// TestFullRefundRetainsTransactionBinding 验证真实退款冲正后旧流水仍阻止再次登记。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestFullRefundRetainsTransactionBinding(t *testing.T) {
	s, _, c, _, head := approvedControlFixture(t)
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record", "expense.payment.reversal.request"}}
	head.Permissions = append(head.Permissions, "expense.payment.reversal.approve")
	paid := runCommand(t, s, cashier, "payment.record", c.ID, bindingPaymentBody("refund-paid", c.Version, "paid-transaction"))
	request := runCommand(t, s, cashier, "reversal.request", paid["id"].(string), map[string]any{"command_id": "refund-request", "expected_version": c.Version + 1, "reason": "实际全额退款", "fact_kind": "FULL_REFUND", "evidence_file_ids": []string{"proof"}})
	runCommand(t, s, head, "reversal.decide", request["id"].(string), map[string]any{"command_id": "refund-approve", "expected_version": 1, "approve": true, "reason": "核实退款到账"})
	raw, _ := json.Marshal(bindingPaymentBody("refund-duplicate", c.Version+2, "paid-transaction"))
	_, err := s.Execute(context.Background(), cashier, "payment.record", c.ID, raw)
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "PAYMENT_TRANSACTION_CONFLICT" {
		t.Fatalf("退款流水丢失防重约束: %v", err)
	}
}
