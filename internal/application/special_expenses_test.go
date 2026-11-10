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

// patchPolicyFixture 修改隔离测试中一份已发布政策的指定业务字段。
// 参数：t 为测试上下文，db 为临时库，id 为夹具政策标识，fields 为替换字段。
// 返回值：无。
func patchPolicyFixture(t *testing.T, db *gorm.DB, id string, fields map[string]any) {
	t.Helper()
	var row store.Config
	if err := db.First(&row, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(row.ContentJSON), &value); err != nil {
		t.Fatal(err)
	}
	for key, field := range fields {
		value[key] = field
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&row).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
}

// TestPrepaymentRequiresExplicitPolicyReasonAndEvidence 验证预付必须政策允许且具备独立说明证据。
// 参数：t 为隔离标准匹配测试上下文。
// 返回值：无。
func TestPrepaymentRequiresExplicitPolicyReasonAndEvidence(t *testing.T) {
	cases := []struct {
		name, nature, reason string
		allowed              bool
		evidence             []string
		code                 string
	}{
		{"无预付政策", "PREPAYMENT", "采购预付", false, []string{"proof"}, "PREPAYMENT_NOT_ALLOWED"},
		{"缺少预付说明", "PREPAYMENT", " ", true, []string{"proof"}, "PREPAYMENT_REASON_REQUIRED"},
		{"缺少独立证明", "PREPAYMENT", "采购预付", true, nil, "PREPAYMENT_EVIDENCE_REQUIRED"},
		{"重复证明", "PREPAYMENT", "采购预付", true, []string{"proof", "proof"}, "PREPAYMENT_EVIDENCE_REQUIRED"},
		{"非法性质", "LOAN", "借款", true, []string{"proof"}, "INVALID_EXPENSE_NATURE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, db, _ := setupWorkflow(t)
			_, id := submissionFixture(t, db, true)
			patchPolicyFixture(t, db, "standard", map[string]any{"allow_prepayment": tc.allowed})
			c, _, err := loadClaim(db, "t", id, false)
			if err != nil {
				t.Fatal(err)
			}
			c.Items[0].ExpenseNature = tc.nature
			c.Items[0].Reason = tc.reason
			c.Items[0].NoInvoiceEvidenceFileIDs = tc.evidence
			err = applyStandards(db, &c)
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != tc.code {
				t.Fatalf("应拒绝%s，实际%v", tc.code, err)
			}
		})
	}
}

// TestPrepaymentSubmissionFreezesFinancialReview 验证真实提交冻结财务重点复核且不改变应付或验真。
// 参数：t 为临时 SQLite 业务事务测试上下文。
// 返回值：无。
func TestPrepaymentSubmissionFreezesFinancialReview(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	patchPolicyFixture(t, db, "standard", map[string]any{"allow_prepayment": true})
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	c.Items[0].ExpenseNature = "PREPAYMENT"
	c.Items[0].Reason = "项目设备预付款，提供订购合同"
	c.Items[0].NoInvoiceEvidenceFileIDs = []string{"prepay-proof"}
	raw, _ := json.Marshal(c)
	if err = db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	runCommand(t, s, actor, "claim.submit", id, map[string]any{"command_id": "prepay-submit", "expected_version": 1})
	var row store.Snapshot
	if err = db.First(&row, "claim_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	var snapshot approvalSnapshot
	if err = json.Unmarshal([]byte(row.ContentJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	item := snapshot.Claim.Items[0]
	if item.ExpenseNature != "PREPAYMENT" || !item.FinanceReviewRequired || len(item.ReviewHints) == 0 || snapshot.Claim.Payable != 100 {
		t.Fatalf("预付未冻结人工复核或改动应付: %+v", snapshot.Claim)
	}
	if len(snapshot.Approval.Tasks) != 2 || snapshot.Approval.Tasks[1].NodeCode != "ACCOUNTANT" {
		t.Fatal("预付绕过原业务审批和财务复核")
	}
	var inv store.Invoice
	if err = db.First(&inv, "id = ?", "invoice").Error; err != nil {
		t.Fatal(err)
	}
	var facts domain.Invoice
	if err = json.Unmarshal([]byte(inv.ContentJSON), &facts); err != nil {
		t.Fatal(err)
	}
	if facts.VerificationStatus != "NOT_CHECKED" {
		t.Fatal("预付擅自改变验真事实")
	}
}

// TestDraftClearsForgedFinancialReviewFlags 验证实际费用不会保留客户端伪造的财务结论。
// 参数：t 为隔离草稿保存测试上下文。
// 返回值：无。
func TestDraftClearsForgedFinancialReviewFlags(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	actor.Permissions = append(actor.Permissions, "expense.claim.update")
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	c.Items[0].ExpenseNature = "ACTUAL"
	c.Items[0].FinanceReviewRequired = true
	c.Items[0].ReviewHints = []string{"客户端伪造已允许预付"}
	raw, _ := json.Marshal(c)
	var body map[string]any
	if err = json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["command_id"], body["expected_version"] = "save-fake-review", 1
	runCommand(t, s, actor, "claim.update", id, body)
	saved, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Items[0].FinanceReviewRequired || len(saved.Items[0].ReviewHints) != 0 {
		t.Fatal("草稿接受客户端财务复核结论")
	}
}

type rejectingPrepaymentEvidence struct{ filesFixture }

// Validate 仅允许原件用途，模拟费用证明用途校验被文件网关拒绝。
// 参数：ctx 为上下文，actor 为身份，ids 为待核验文件，purpose 为业务用途。
// 返回值：费用证明用途返回拒绝，其余用途成功。
func (rejectingPrepaymentEvidence) Validate(ctx context.Context, actor domain.Actor, ids []string, purpose string) error {
	if purpose == "reimbursement.claim.evidence" {
		return domain.Fail("FILE_PURPOSE_MISMATCH", "证明用途不匹配", 422)
	}
	return nil
}

// TestPrepaymentCannotReuseInvoiceAsEvidence 验证预付证明仍需独立claim.evidence用途授权。
// 参数：t 为隔离事务测试上下文。
// 返回值：无。
func TestPrepaymentCannotReuseInvoiceAsEvidence(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	s.deps.Files = rejectingPrepaymentEvidence{}
	patchPolicyFixture(t, db, "standard", map[string]any{"allow_prepayment": true})
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	c.Items[0].ExpenseNature = "PREPAYMENT"
	c.Items[0].Reason = "设备预付"
	c.Items[0].NoInvoiceEvidenceFileIDs = []string{"file"}
	raw, _ := json.Marshal(c)
	if err = db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	_, err = s.Execute(context.Background(), actor, "claim.submit", id, json.RawMessage(`{"command_id":"bad-prepay-proof","expected_version":1}`))
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "FILE_PURPOSE_MISMATCH" {
		t.Fatalf("应拒绝错误用途证明，实际%v", err)
	}
	var count int64
	if err = db.Model(&store.Snapshot{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("无独立证明仍提交预付")
	}
}
