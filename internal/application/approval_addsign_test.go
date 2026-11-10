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

// addsignFixture 建立真实提交及受控替代映射的独立加签夹具。
// 参数：t为测试上下文。
// 返回值：服务、临时库、原受理人和父审批任务。
func addsignFixture(t *testing.T) (*Service, *gorm.DB, domain.Actor, store.Task) {
	t.Helper()
	s, db, _ := setupWorkflow(t)
	a, claimID := submissionFixture(t, db, true)
	runCommand(t, s, a, "claim.submit", claimID, map[string]any{
		"command_id": "addsign-submit", "expected_version": 1,
	})
	seedPolicy(t, db, "assignments", domain.Assignment{ID: "addsign-target", OrgID: "org",
		ProjectID: "p1", Role: "PROJECT_EXPENSE_OWNER", SubjectID: "lead2", ValidFrom: "2026-01-01"})
	var task store.Task
	if err := db.Where("claim_id = ? AND status = ?", claimID, "PENDING").First(&task).Error; err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{TenantID: "t", SubjectID: task.Assignee, Permissions: []string{
		"expense.approval.act", "expense.approval.transfer", "expense.claim.read",
	}}
	return s, db, actor, task
}

// TestApprovalAddsignKeepsAllAndImmutableOrigin 防止加签自动通过原任务、漏审或丢失真实来源。
// 参数：t为测试上下文。
// 返回值：无。
func TestApprovalAddsignKeepsAllAndImmutableOrigin(t *testing.T) {
	s, db, actor, task := addsignFixture(t)
	body := map[string]any{"command_id": "addsign-1", "expected_version": 2,
		"task_version": task.Version, "round": 1, "action": "ADDSIGN",
		"target_subject_id": "lead2", "reason": "追加同等范围独立复核"}
	r := runCommand(t, s, actor, "approval.action", task.ID, body)
	addedID, _ := r["added_task_id"].(string)
	if addedID == "" || r["task_id"] != task.ID || r["claim_version"] != float64(3) {
		t.Fatalf("加签回执缺少父子和版本: %+v", r)
	}
	var added, parent store.Task
	if err := db.First(&added, "id = ?", addedID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&parent, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if parent.Status != "PENDING" || parent.Version != 2 || added.Status != "PENDING" ||
		added.Assignee != "lead2" || added.Node != parent.Node || added.Round != parent.Round {
		t.Fatalf("加签应保留同节点ALL任务: parent=%+v added=%+v", parent, added)
	}
	var detail store.ActionDetail
	if err := db.Where("task_id = ?", task.ID).First(&detail).Error; err != nil {
		t.Fatal(err)
	}
	var frozen map[string]any
	if err := json.Unmarshal([]byte(detail.ContentJSON), &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen["added_task_id"] != addedID || frozen["parent_task_id"] != task.ID ||
		detail.ToSubjectID != "lead2" || detail.Reason != "追加同等范围独立复核" {
		t.Fatalf("缺少不可变加签证据: %+v", frozen)
	}
	// 同一命令重放不会再创建一个加签人。
	replayed := runCommand(t, s, actor, "approval.action", task.ID, body)
	if replayed["added_task_id"] != addedID {
		t.Fatal("幂等重放未返回原加签任务")
	}
	runCommand(t, s, actor, "approval.action", task.ID, map[string]any{
		"command_id": "addsign-parent-approve", "expected_version": 3,
		"task_version": 2, "round": 1, "action": "APPROVE",
	})
	var accountant store.Task
	if err := db.Where("claim_id = ? AND node_code = ?", task.ClaimID, "ACCOUNTANT").First(&accountant).Error; err != nil {
		t.Fatal(err)
	}
	if accountant.Status != "WAITING" {
		t.Fatal("加签人未通过前不应推进会计节点")
	}
	actor.SubjectID = "lead2"
	runCommand(t, s, actor, "approval.action", addedID, map[string]any{
		"command_id": "addsign-child-approve", "expected_version": 4,
		"task_version": 1, "round": 1, "action": "APPROVE",
	})
	if err := db.First(&accountant, "id = ?", accountant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if accountant.Status != "PENDING" {
		t.Fatal("原人与加签人均通过后应激活下一节点")
	}
}

// TestApprovalAddsignRejectsInvalidTargetAndPermission 防止跨范围、自签、重复及无授权加签。
// 参数：t为测试上下文。
// 返回值：无。
func TestApprovalAddsignRejectsInvalidTargetAndPermission(t *testing.T) {
	for _, name := range []string{"self", "applicant", "unmapped", "permission", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			s, db, actor, task := addsignFixture(t)
			target := "lead2"
			switch name {
			case "self":
				target = actor.SubjectID
			case "applicant":
				target = "app"
			case "unmapped":
				target = "lead3"
			case "permission":
				actor.Permissions = []string{"expense.approval.act"}
			case "duplicate":
				seed := task
				seed.ID, seed.Assignee = "already-in-node", "lead2"
				if err := db.Create(&seed).Error; err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(map[string]any{"command_id": "invalid-addsign-" + name,
				"expected_version": 2, "task_version": 1, "round": 1,
				"action": "ADDSIGN", "target_subject_id": target, "reason": "独立复核"})
			_, err := s.Execute(context.Background(), actor, "approval.action", task.ID, raw)
			var p *domain.Problem
			if !errors.As(err, &p) || (p.Status != 422 && p.Status != 403 && p.Status != 409) {
				t.Fatalf("非法加签未受控拒绝: %v", err)
			}
			var count int64
			if err := db.Model(&store.Action{}).Where("action = ?", "ADDSIGN").Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("非法加签留下动作: %d %v", count, err)
			}
		})
	}
}

// TestApprovalAddsignRechecksSubstituteAndOrigin 防止加签人继续使用失效映射或被篡改来源办理。
// 参数：t为测试上下文。
// 返回值：无。
func TestApprovalAddsignRechecksSubstituteAndOrigin(t *testing.T) {
	for _, name := range []string{"mapping-revoked", "origin-missing", "frozen-task-changed"} {
		t.Run(name, func(t *testing.T) {
			s, db, actor, parent := addsignFixture(t)
			added := runCommand(t, s, actor, "approval.action", parent.ID, map[string]any{
				"command_id": "addsign-origin", "expected_version": 2, "task_version": 1,
				"round": 1, "action": "ADDSIGN", "target_subject_id": "lead2", "reason": "独立复核",
			})
			id := added["added_task_id"].(string)
			var err error
			switch name {
			case "mapping-revoked":
				err = db.Model(&store.Config{}).Where("id = ?", "addsign-target").Update("published", false).Error
			case "origin-missing":
				err = db.Where("task_id = ?", parent.ID).Delete(&store.ActionDetail{}).Error
			case "frozen-task-changed":
				var child store.Task
				if err = db.First(&child, "id = ?", id).Error; err != nil {
					t.Fatal(err)
				}
				var content map[string]any
				_ = json.Unmarshal([]byte(child.ContentJSON), &content)
				content["allocation_keys"] = []string{"item/another-project/s"}
				raw, _ := json.Marshal(content)
				err = db.Model(&child).Update("content_json", string(raw)).Error
			}
			if err != nil {
				t.Fatal(err)
			}
			actor.SubjectID = "lead2"
			raw, _ := json.Marshal(map[string]any{"command_id": "invalid-child-" + name,
				"expected_version": 3, "task_version": 1, "round": 1, "action": "APPROVE"})
			if _, err = s.Execute(context.Background(), actor, "approval.action", id, raw); err == nil {
				t.Fatal("当前替代映射或真实来源失效仍允许审批")
			}
		})
	}
}

// TestApprovalAddsignRequiresCurrentFinanceScope 防止财务加签仅凭节点能力跨组织扩大审核范围。
// 参数：t为测试上下文。
// 返回值：无。
func TestApprovalAddsignRequiresCurrentFinanceScope(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-scope", true: "same-scope"}[scoped], func(t *testing.T) {
			s, db, actor, parent := addsignFixture(t)
			runCommand(t, s, actor, "approval.action", parent.ID, map[string]any{
				"command_id": "finance-parent-approve", "expected_version": 2,
				"task_version": 1, "round": 1, "action": "APPROVE",
			})
			seedPolicy(t, db, "assignments", domain.Assignment{ID: "additional-accountant",
				OrgID: "org", Role: "ACCOUNTANT", SubjectID: "accountant", ValidFrom: "2026-01-01"})
			if scoped {
				seedPolicy(t, db, "scopes", domain.Scope{ID: "additional-scope", SubjectID: "accountant",
					OrgIDs: []string{"org"}})
			}
			var task store.Task
			if err := db.Where("claim_id = ? AND node_code = ?", parent.ClaimID,
				"ACCOUNTANT").First(&task).Error; err != nil {
				t.Fatal(err)
			}
			actor.SubjectID = "acct"
			actor.Permissions = append(actor.Permissions, "expense.finance.review")
			raw, _ := json.Marshal(map[string]any{"command_id": "finance-addsign",
				"expected_version": 3, "task_version": task.Version, "round": 1,
				"action": "ADDSIGN", "target_subject_id": "accountant", "reason": "独立财务复核"})
			_, err := s.Execute(context.Background(), actor, "approval.action", task.ID, raw)
			if (err == nil) != scoped {
				t.Fatalf("财务目标必须有当前同组织范围，scoped=%t: %v", scoped, err)
			}
		})
	}
}

// TestApprovalAddsignChildHistoryIncludesParent 防止加签受理人只能看见空历史而丢失来源和原因。
// 参数：t为测试上下文。
// 返回值：无。
func TestApprovalAddsignChildHistoryIncludesParent(t *testing.T) {
	s, db, actor, parent := addsignFixture(t)
	added := runCommand(t, s, actor, "approval.action", parent.ID, map[string]any{
		"command_id": "history-addsign", "expected_version": 2, "task_version": 1,
		"round": 1, "action": "ADDSIGN", "target_subject_id": "lead2", "reason": "核对共同费用",
	})
	actor.SubjectID = "lead2"
	view, err := s.approvalHistory(db, actor, added["added_task_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	origin, ok := view.(map[string]any)["addsign_origin"].(map[string]any)
	if !ok || origin["parent_task_id"] != parent.ID || origin["action"] != "ADDSIGN" ||
		origin["reason"] != "核对共同费用" || origin["from_subject_id"] != parent.Assignee {
		t.Fatalf("加签子任务应展示真实父动作来源: %+v", view)
	}
}
