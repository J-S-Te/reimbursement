package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestInvoiceDeductionPriorityRejectsTaxDecisions 验证票种优先复核配置不能冒充已抵扣或正式验真。
// 参数：t 为票种政策边界测试上下文。
// 返回值：无。
func TestInvoiceDeductionPriorityRejectsTaxDecisions(t *testing.T) {
	for _, value := range []string{"DEDUCTIBLE", "VERIFIED", "PRIORITY", "priority_review"} {
		raw, _ := json.Marshal(map[string]any{"deduction_priority": value})
		if _, err := decodeInvoicePolicy(string(raw)); err == nil {
			t.Fatalf("接受伪造税务结论%s", value)
		}
	}
	for _, value := range []string{"", "NORMAL", "PRIORITY_REVIEW"} {
		raw, _ := json.Marshal(map[string]any{"deduction_priority": value})
		rule, err := decodeInvoicePolicy(string(raw))
		if err != nil || rule.DeductionPriority != value {
			t.Fatalf("拒绝合法人工复核优先级%s: %v", value, err)
		}
	}
}

// TestInvoicePrioritySuggestionIsOnlyManualFinancialReview 验证专票优先仅形成建议且不改变财务事实。
// 参数：t 为隔离 SQLite 费用建议测试上下文。
// 返回值：无。
func TestInvoicePrioritySuggestionIsOnlyManualFinancialReview(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, _ := submissionFixture(t, db, true)
	actor.Permissions = []string{"expense.invoice.read", "expense.claim.create"}
	setInvoiceDatePolicyFixture(t, db, time.Now().Format("2006-01-02"), "BLOCK")
	patchPolicyFixture(t, db, "rule", map[string]any{"fee_type": "TRAVEL", "deduction_priority": "PRIORITY_REVIEW"})
	var before store.Invoice
	if err := db.First(&before, "id = ?", "invoice").Error; err != nil {
		t.Fatal(err)
	}
	values, err := s.InvoiceExpenseSuggestions(context.Background(), actor, []string{"invoice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].DeductionPriority != "PRIORITY_REVIEW" ||
		!values[0].DeductionReviewRequired || values[0].Requested != 100 {
		t.Fatalf("缺少人工优先复核建议: %+v", values)
	}
	var after store.Invoice
	if err = db.First(&after, "id = ?", "invoice").Error; err != nil {
		t.Fatal(err)
	}
	if before.ContentJSON != after.ContentJSON || before.Version != after.Version || after.ReservationClaimID != "" {
		t.Fatal("生成优先建议修改票据事实或占票")
	}
	var inv domain.Invoice
	if err = json.Unmarshal([]byte(after.ContentJSON), &inv); err != nil {
		t.Fatal(err)
	}
	if inv.VerificationStatus != "NOT_CHECKED" {
		t.Fatal("人工建议变成正式验真")
	}
	patchPolicyFixture(t, db, "rule", map[string]any{"deduction_priority": "NORMAL"})
	values, err = s.InvoiceExpenseSuggestions(context.Background(), actor, []string{"invoice"})
	if err != nil || len(values) != 1 || values[0].DeductionReviewRequired {
		t.Fatalf("普通票据误标优先抵扣建议: %+v %v", values, err)
	}
}
