package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestApprovalsKeepVerifiedApplicant 验证本人提交冻结可信姓名，审批仅向获准受理人展示。
// 参数：t 为临时 SQLite 工作流的测试上下文。
// 返回值：无；假姓名、错身份、无权访问或快照丢失时失败。
func TestApprovalsKeepVerifiedApplicant(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	applicant, id := submissionFixture(t, db, true)
	applicant.Permissions = append(applicant.Permissions, "expense.claim.create")
	directory := &draftDirectoryFixture{person: domain.PersonOption{
		ID: "app", Name: "本人草稿姓名", Organizations: []domain.OrganizationOption{{ID: "org", Name: "技术部"}},
	}}
	s.deps.Personnel = directory
	draft := runCommand(t, s, applicant, "claim.create", "", map[string]any{
		"command_id": "new-draft", "title": "未完成草稿", "applicant_name": "客户端伪造姓名",
	})
	if draft["applicant_name"] != "本人草稿姓名" || draft["applicant_subject_id"] != "app" {
		t.Fatalf("草稿未保存目录核验身份: %v", draft)
	}

	directory.person.Name = "提交时姓名"
	submitted := runCommand(t, s, applicant, "claim.submit", id, map[string]any{
		"command_id": "submit-with-name", "expected_version": 1, "applicant_name": "客户端伪造姓名",
	})
	if submitted["applicant_name"] != "提交时姓名" || submitted["applicant_subject_id"] != "app" {
		t.Fatalf("提交未保存目录核验身份: %v", submitted)
	}
	var snapshot store.Snapshot
	if err := db.First(&snapshot, "tenant_id = ? AND claim_id = ?", "t", id).Error; err != nil {
		t.Fatal(err)
	}
	if err := VerifyApprovalSnapshot(json.RawMessage(snapshot.ContentJSON), snapshot.ContentHash); err != nil {
		t.Fatal(err)
	}
	directory.person.Name = "当前目录其他姓名"
	readApprovalApplicant(t, s, applicant, "claim", id, "提交时姓名")
	readApprovalApplicant(t, s, applicant, "claims", "", "提交时姓名")

	var task store.Task
	if err := db.First(&task, "tenant_id = ? AND claim_id = ? AND status = ?", "t", id, "PENDING").Error; err != nil {
		t.Fatal(err)
	}
	approver := domain.Actor{TenantID: "t", SubjectID: task.Assignee, Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	readApprovalApplicant(t, s, approver, "approval-task", task.ID, "提交时姓名")
	readApprovalApplicant(t, s, approver, "approval-tasks", "", "提交时姓名")
	readApprovalApplicant(t, s, approver, "dashboard", "", "提交时姓名")

	approver.SubjectID = "unassigned"
	_, err := s.Query(context.Background(), approver, "approval-task", task.ID, nil)
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Status != 404 {
		t.Fatalf("无权主体访问审批任务未返回404: %v", err)
	}
	if directory.calls != 2 {
		t.Fatalf("查询依赖实时目录: %d calls", directory.calls)
	}
}

// TestHistoricalApprovalRetainsApplicantSubject 验证旧审批保持审批人已获准查看的申请人身份。
// 参数：t 为旧快照、局部范围测试的上下文。
// 返回值：无；缺姓名被伪造或裁剪后的旧快照无法读取时失败。
func TestHistoricalApprovalRetainsApplicantSubject(t *testing.T) {
	s, _, _, approver, _ := scopedPrivacyFixture(t)
	readApprovalApplicant(t, s, approver, "approval-task", "scoped-task", "")
}

// readApprovalApplicant 核对查询只返回单据原申请人的已保存身份。
// 参数：t 为测试上下文，s 为服务，a 为查询主体，kind/id 为查询入口，name 为期望姓名。
// 返回值：无；申请人身份不符或旧记录伪造姓名时失败。
func readApprovalApplicant(t *testing.T, s *Service, a domain.Actor, kind, id, name string) {
	t.Helper()
	raw, err := s.Query(context.Background(), a, kind, id, nil)
	if err != nil {
		t.Fatal(err)
	}

	type identity struct {
		Subject string  `json:"applicant_subject_id"`
		Name    *string `json:"applicant_name"`
	}
	type task struct {
		Snapshot identity `json:"snapshot"`
	}
	var response struct {
		identity
		task
		Pending []task `json:"pending_tasks"`
		Items   []struct {
			identity
			task
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	people := []identity{}
	switch kind {
	case "claim":
		people = append(people, response.identity)
	case "claims":
		for _, item := range response.Items {
			people = append(people, item.identity)
		}
	case "approval-task":
		people = append(people, response.Snapshot)
	case "approval-tasks":
		for _, item := range response.Items {
			people = append(people, item.Snapshot)
		}
	case "dashboard":
		for _, item := range response.Pending {
			people = append(people, item.Snapshot)
		}
	}
	for _, person := range people {
		if person.Subject == "app" && (person.Name == nil && name == "" ||
			person.Name != nil && *person.Name == name) {
			return
		}
	}
	t.Fatalf("%s 未返回所需可信申请人身份和姓名: %s", kind, raw)
}
