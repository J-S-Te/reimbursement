package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestInvoiceFeedbackKeepsExpenseRows 用途：验证票据拒绝保留费用行且无权对象不披露详情。
// 参数：t 为使用临时 SQLite 的测试上下文。
// 返回值：无；业务拒绝、字段坐标或权限边界错误时失败。
func TestInvoiceFeedbackKeepsExpenseRows(t *testing.T) {
	for _, tc := range []struct{ name, review, verification, owner, want string }{
		{"未确认", "PENDING", "NOT_CHECKED", "app", "请先.*人工复核"},
		{"核验异常", "CONFIRMED", "MISMATCH", "app", "核验.*异常"},
		{"超票面金额", "CONFIRMED", "NOT_CHECKED", "app", "累计使用额.*票面金额"},
		{"无权票据", "PENDING", "MISMATCH", "other", "发票不存在或无权使用"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db, _ := setupWorkflow(t)
			submissionFixture(t, db, true)
			claim, _, err := loadClaim(db, "t", "claim", false)
			if err != nil {
				t.Fatal(err)
			}
			claim.Items = append(claim.Items, claim.Items[0])
			var row store.Invoice
			if err = db.First(&row, "id = ?", "invoice").Error; err != nil {
				t.Fatal(err)
			}
			var inv domain.Invoice
			if err = json.Unmarshal([]byte(row.ContentJSON), &inv); err != nil {
				t.Fatal(err)
			}
			inv.ReviewStatus, inv.VerificationStatus = tc.review, tc.verification
			content, _ := json.Marshal(inv)
			if err = db.Model(&row).Updates(map[string]any{"owner": tc.owner, "content_json": string(content)}).Error; err != nil {
				t.Fatal(err)
			}
			err = validateClaimInvoices(db, &claim, false)
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != "INVOICE_NOT_AVAILABLE" {
				t.Fatalf("拒绝分类错误: %v", err)
			}
			if tc.owner != "app" {
				if problem.Message != tc.want || len(problem.FieldErrors) != 0 {
					t.Fatalf("无权对象泄漏详情: %+v", problem)
				}
				return
			}
			parts := strings.Split(tc.want, ".*")
			for _, part := range parts {
				if !strings.Contains(problem.Message, part) {
					t.Fatalf("原因缺失 %q: %+v", part, problem)
				}
			}
			if len(problem.FieldErrors) != 2 {
				t.Fatalf("费用行丢失: %+v", problem.FieldErrors)
			}
			for index, issue := range problem.FieldErrors {
				if issue.Field != fmt.Sprintf("items[%d].invoice_usages", index) || !strings.Contains(issue.Message, fmt.Sprintf("第%d项费用", index+1)) {
					t.Fatalf("费用坐标错误: %+v", issue)
				}
			}
		})
	}
}

// TestInvoiceFeedbackExplainsDatePolicy 用途：验证票种时效拒绝包含实际天数和下一步。
// 参数：t 为纯日期政策测试上下文。
// 返回值：无；缺少现有政策天数或核对指引时失败。
func TestInvoiceFeedbackExplainsDatePolicy(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	_, err := invoiceDateWarning(domain.Invoice{InvoiceDate: "2026-10-01"},
		domain.InvoicePolicy{MaxInvoiceAgeDays: 30, DatePolicy: "BLOCK"}, now)
	if err == nil || !strings.Contains(err.Error(), "30天") || !strings.Contains(err.Error(), "请核对") {
		t.Fatalf("超期提示缺少政策与下一步: %v", err)
	}
}

// TestPaymentFeedbackTargetsEditableFields 用途：验证付款拒绝定位真实输入和自动批准字段边界。
// 参数：t 为真实应用事务和临时 SQLite 测试上下文。
// 返回值：无；字段路径、公开原因、事务回滚错误时失败。
func TestPaymentFeedbackTargetsEditableFields(t *testing.T) {
	for _, tc := range []struct {
		name, key, field, code string
		value                  any
	}{
		{"付款日期", "paid_date", "paid_date", "INVALID_DATE", "2026-02-30"},
		{"付款方式", "method", "method", "INVALID_PAYMENT_METHOD", "WIRE"},
		{"空账户", "source_account_ref", "source_account_ref", "PAYMENT_EVIDENCE_REQUIRED", ""},
		{"流水超长", "transaction_no", "transaction_no", "PAYMENT_EVIDENCE_REQUIRED", strings.Repeat("x", 129)},
		{"缺少凭证", "voucher_file_ids", "voucher_file_ids", "FILE_REFERENCE_LIMIT", []string{}},
		{"自动金额已过期", "amount_minor", "", "PAYMENT_MISMATCH", "99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db, claim, _, _ := approvedControlFixture(t)
			body := bindingPaymentBody("feedback-payment", claim.Version, "txn-feedback")
			body[tc.key] = tc.value
			raw, _ := json.Marshal(body)
			actor := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.payment.record", "expense.claim.read"}}
			_, err := s.Execute(context.Background(), actor, "payment.record", claim.ID, raw)
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != tc.code {
				t.Fatalf("付款拒绝分类错误: %v", err)
			}
			if tc.field == "" {
				if len(problem.FieldErrors) != 0 || !strings.Contains(problem.Message, "刷新") {
					t.Fatalf("要求编辑自动批准字段: %+v", problem)
				}
			} else if len(problem.FieldErrors) != 1 || problem.FieldErrors[0].Field != tc.field {
				t.Fatalf("付款字段缺失: %+v", problem)
			}
			var count int64
			if err = db.Model(&store.Payment{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("拒绝后仍登记付款: %d %v", count, err)
			}
		})
	}
}
