package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"testing"
)

// TestPaymentNewTravelMismatchRequiresReopen 验证已批准一项矛盾后新增日期矛盾仍须重开。
// 参数：t为隔离数据库和权威项目替身测试上下文。
// 返回值：无；新重大事实未复核仍登记付款时失败。
func TestPaymentNewTravelMismatchRequiresReopen(t *testing.T) {
	for _, scenario := range []string{"new_dimension", "other_reference_unknown"} {
		t.Run(scenario, func(t *testing.T) {
			s, db, p := setupWorkflow(t)
			applicant, id := submissionFixture(t, db, true)
			seedPolicy(t, db, "assignments", domain.Assignment{ID: "director", OrgID: "org", Role: "TECHNICAL_DIRECTOR", SubjectID: "director", ValidFrom: "2026-01-01"})
			for _, subject := range []string{"acct", "cashier"} {
				seedPolicy(t, db, "scopes", domain.Scope{ID: "scope-" + subject, SubjectID: subject, OrgIDs: []string{"org"}})
			}
			old := travelFixtureSnapshot("MISMATCH")
			old.ReasonCodes = append(old.ReasonCodes, "SITE_MISMATCH")
			old.ContentDigest = ""
			raw, _ := json.Marshal(old)
			sum := sha256.Sum256(raw)
			old.ContentDigest = hex.EncodeToString(sum[:])
			enableTravelFixture(t, s, id, &domain.TravelConfirmation{Decision: "ACKNOWLEDGED_MISMATCH", Reason: "仅就现场差异提交批准", EvidenceFileIDs: []string{"proof"}}, old)
			if scenario == "other_reference_unknown" {
				second := p.references[0]
				second.ServiceItemID = "s2"
				second.TravelSnapshot = travelFixtureSnapshot("MATCH")
				second.TravelSnapshot.CityCodeStatus = "MATCH"
				second.TravelSnapshot.Transport = "TRAIN"
				second.TravelSnapshot.AccommodationNeed = "YES"
				second.TravelSnapshot.ReasonCodes = nil
				refreshTravelChangeDigest(t, second.TravelSnapshot)
				p.references = append(p.references, second)
				c, _, err := loadClaim(db, "t", id, false)
				if err != nil {
					t.Fatal(err)
				}
				c.Items[0].Allocations[0].Amount = 50
				secondAllocation := c.Items[0].Allocations[0]
				secondAllocation.ServiceItemID = "s2"
				c.Items[0].Allocations = append(c.Items[0].Allocations, secondAllocation)
				raw, _ = json.Marshal(c)
				if err = db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw)).Error; err != nil {
					t.Fatal(err)
				}
			}
			runCommand(t, s, applicant, "claim.submit", id, map[string]any{"command_id": "submit-old-mismatch", "expected_version": 1})
			var tasks []store.Task
			if err := db.Where("claim_id = ?", id).Order("node").Find(&tasks).Error; err != nil {
				t.Fatal(err)
			}
			version := int64(2)
			for _, task := range tasks {
				if err := db.First(&task, "id = ?", task.ID).Error; err != nil {
					t.Fatal(err)
				}
				approver := domain.Actor{TenantID: "t", SubjectID: task.Assignee, Permissions: []string{"expense.claim.read", "expense.approval.act", "expense.finance.review"}}
				runCommand(t, s, approver, "approval.action", task.ID, map[string]any{"command_id": "approve-" + task.ID, "expected_version": version, "task_version": task.Version, "round": 1, "action": "APPROVE"})
				version++
			}
			if scenario == "new_dimension" {
				changed := *old
				changed.DateMatchStatus = "MISMATCH"
				refreshTravelChangeDigest(t, &changed)
				p.references[0].TravelSnapshot = &changed
			} else {
				changed := *p.references[1].TravelSnapshot
				changed.SiteMatchStatus = "UNKNOWN"
				refreshTravelChangeDigest(t, &changed)
				p.references[1].TravelSnapshot = &changed
			}
			cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record"}}
			raw, _ = json.Marshal(map[string]any{"command_id": "payment-new-date-mismatch", "expected_version": version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "new-mismatch", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}})
			_, err := s.Execute(context.Background(), cashier, "payment.record", id, raw)
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != "REOPEN_REQUIRED" {
				var count int64
				db.Model(&store.Payment{}).Count(&count)
				t.Fatalf("new date mismatch bypassed reapproval: err=%v payments=%d", err, count)
			}
			var count int64
			db.Model(&store.Payment{}).Count(&count)
			if count != 0 {
				t.Fatal("被拒绝的新行程事实仍留下付款")
			}
		})
	}

}

// refreshTravelChangeDigest 为改变后的测试权威事实按真实DTO重算摘要。
// 参数：t为测试上下文；facts为当前最小行程事实。
// 返回值：无；编码失败终止测试。
func refreshTravelChangeDigest(t *testing.T, facts *domain.TravelSnapshot) {
	t.Helper()
	facts.ContentDigest = ""
	raw, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	facts.ContentDigest = hex.EncodeToString(hash[:])
}
