package application

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// resolutionSelectionFixture 通过真实审批、异常和付款命令建立可匹配账本。
// 参数：t 为隔离测试上下文。
// 返回值：服务、临时库、原单据、现金员、独立核实人、异常和付款坐标。
func resolutionSelectionFixture(t *testing.T) (*Service, *gorm.DB, domain.Claim, domain.Actor, domain.Actor, string, string) {
	t.Helper()
	s, db, c, _, head := approvedControlFixture(t)
	seedPaymentSelectionAccount(t, db)
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{
		"expense.claim.read", "expense.payment.read", "expense.payment.record",
		"expense.payment.exception.record", "expense.payment.reversal.request"}}
	body := bindingPaymentBody("resolution-fact", c.Version, "same-transaction")
	body["reason"] = "原始异常待核实"
	fact := runCommand(t, s, cashier, "payment.exception.record", c.ID, body)
	body["command_id"] = "resolution-payment"
	paid := runCommand(t, s, cashier, "payment.record", c.ID, body)
	return s, db, c, cashier, head, fact["id"].(string), paid["id"].(string)
}

// TestExceptionResolutionOptionsExposeOnlyMatchingActivePayment 验证只有同交易合法付款进入选项。
// 参数：t 为真实隔离 SQLite 测试上下文。
// 返回值：无。
func TestExceptionResolutionOptionsExposeOnlyMatchingActivePayment(t *testing.T) {
	s, db, c, _, head, exceptionID, paymentID := resolutionSelectionFixture(t)
	var payment store.Payment
	if err := db.First(&payment, "id = ?", paymentID).Error; err != nil {
		t.Fatal(err)
	}
	other := payment
	other.ID, other.ClaimID = "other-claim-payment", "other-claim"
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	other.ID, other.TenantID, other.ClaimID = "other-tenant-payment", "elsewhere", c.ID
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := s.ExceptionResolutionOptions(context.Background(), head, exceptionID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Payments []struct {
			ID string `json:"id"`
		} `json:"payments"`
		Reversals []struct {
			ID string `json:"id"`
		} `json:"reversals"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Payments) != 1 || result.Payments[0].ID != paymentID || len(result.Reversals) != 0 {
		t.Fatalf("合法同交易选项错误: %s", raw)
	}
	var fact store.PaymentExceptionFact
	if err = db.First(&fact, "id = ?", exceptionID).Error; err != nil {
		t.Fatal(err)
	}
	var value domain.PaymentExceptionFact
	_ = json.Unmarshal([]byte(fact.ContentJSON), &value)
	value.TransactionNo = "different-transaction"
	hash, _ := digest(value)
	content, _ := encode(value)
	if err = db.Model(&fact).Updates(map[string]any{"content_hash": hash, "content_json": content}).Error; err != nil {
		t.Fatal(err)
	}
	raw, err = s.ExceptionResolutionOptions(context.Background(), head, exceptionID)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &result)
	if len(result.Payments) != 0 {
		t.Fatal("不同交易仍出现合法付款选项")
	}
}

// TestExceptionResolutionOptionsUseActualReversalID 验证冲正选项绑定财务事实而非申请编号。
// 参数：t 为真实隔离 SQLite 测试上下文。
// 返回值：无。
func TestExceptionResolutionOptionsUseActualReversalID(t *testing.T) {
	s, db, c, cashier, head, exceptionID, paymentID := resolutionSelectionFixture(t)
	head.Permissions = append(head.Permissions, "expense.payment.reversal.approve")
	request := runCommand(t, s, cashier, "reversal.request", paymentID, map[string]any{
		"command_id": "selection-refund", "expected_version": c.Version + 1,
		"reason": "全部退款", "fact_kind": "FULL_REFUND", "evidence_file_ids": []string{"proof"}})
	runCommand(t, s, head, "reversal.decide", request["id"].(string), map[string]any{
		"command_id": "selection-refund-approve", "expected_version": 1,
		"approve": true, "reason": "独立核实全额退款"})
	var reversal store.Reversal
	if err := db.First(&reversal, "payment_id = ?", paymentID).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := s.ExceptionResolutionOptions(context.Background(), head, exceptionID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Payments []struct {
			ID string `json:"id"`
		} `json:"payments"`
		Reversals []struct{ ID, PaymentID string } `json:"reversals"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Payments) != 1 || len(result.Reversals) != 1 ||
		result.Reversals[0].ID != reversal.ID || result.Reversals[0].ID == request["id"] {
		t.Fatalf("冲正申请编号冒充真实冲正或遗漏合法账本: %s", raw)
	}
}

// TestExceptionResolutionOptionsRejectScopeAndFinalState 验证整单权限、独立核实和终结状态。
// 参数：t 为真实隔离 SQLite 测试上下文。
// 返回值：无。
func TestExceptionResolutionOptionsRejectScopeAndFinalState(t *testing.T) {
	s, db, _, cashier, head, exceptionID, _ := resolutionSelectionFixture(t)
	for _, a := range []domain.Actor{
		{TenantID: "other", SubjectID: head.SubjectID, Permissions: head.Permissions},
		{TenantID: "t", SubjectID: "outsider", Permissions: head.Permissions},
		{TenantID: "t", SubjectID: head.SubjectID, Permissions: []string{"expense.claim.read"}},
		{TenantID: "t", SubjectID: "cashier", Permissions: append(cashier.Permissions, "expense.payment.exception.review")},
	} {
		if _, err := s.ExceptionResolutionOptions(context.Background(), a, exceptionID); err == nil {
			t.Fatal("无范围或不独立人员获得异常解除选项")
		}
	}
	if err := db.Model(&store.PaymentExceptionCursor{}).Where("id = ?", exceptionID).
		Update("status", "RESOLVED").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExceptionResolutionOptions(context.Background(), head, exceptionID); err == nil {
		t.Fatal("终结异常仍返回可操作账本")
	}
}
