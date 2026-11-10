package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type recoveryDirectory struct {
	failed map[string]error
}

// VerifyApprover 为独立恢复场景模拟精确的当前人员资格响应。
// 参数：ctx 为调用边界，a 为可信租户身份，target/permission 为当前核验对象。
// 返回值：明确失效或依赖错误，其余目标使用现有完整资格夹具。
func (p recoveryDirectory) VerifyApprover(ctx context.Context, a domain.Actor, target, permission string) error {
	if e := p.failed[target]; e != nil {
		return e
	}
	return (workflowPersonnel{}).VerifyApprover(ctx, a, target, permission)
}

// approvalRecoveryFixture 建立已提交且原受理人明确离职的隔离任务。
// 参数：t 为测试上下文。
// 返回值：应用、数据库、项目夹具、任务及有恢复组织范围的操作者。
func approvalRecoveryFixture(t *testing.T) (*Service, *gorm.DB, *projectFixture, store.Task, domain.Actor) {
	t.Helper()
	s, db, p := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "recover-submit", "expected_version": 1})
	var task store.Task
	if e := db.Where("claim_id = ? AND node = 1", id).First(&task).Error; e != nil {
		t.Fatal(e)
	}
	seedPolicy(t, db, "scopes", domain.Scope{ID: "recover-scope", SubjectID: "operator", OrgIDs: []string{"org"}})
	seedPolicy(t, db, "assignments", domain.Assignment{ID: "recover-target", OrgID: "org", Role: "PROJECT_EXPENSE_OWNER", ProjectID: "p1", SubjectID: "lead2", ValidFrom: "2026-01-01"})
	s.deps.Personnel = recoveryDirectory{failed: map[string]error{"lead": domain.Fail("APPROVER_INELIGIBLE", "原受理人已离职", 422)}}
	a := domain.Actor{TenantID: "t", SubjectID: "operator", Permissions: []string{"expense.claim.read", "expense.approval.recover"}}
	return s, db, p, task, a
}

// recoveryPayload 生成版本绑定的单次恢复请求。
// 参数：command 为独立命令标识。
// 返回值：包含原因和明确目标的恢复请求。
func recoveryPayload(command string) json.RawMessage {
	return json.RawMessage(`{"command_id":"` + command + `","expected_version":2,"task_version":1,"round":1,"reason":"原负责人离职后接管","target_subject_id":"lead2"}`)
}

// TestApprovalRecoveryPreservesTransferHistory 验证管理员受控接管而不改写冻结审批责任。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalRecoveryPreservesTransferHistory(t *testing.T) {
	s, db, _, task, operator := approvalRecoveryFixture(t)
	raw := recoveryPayload("recover-first")
	result, e := s.Execute(context.Background(), operator, "approval.recover", task.ID, raw)
	if e != nil {
		t.Fatalf("离职任务应允许独立恢复：%v", e)
	}
	var current store.Task
	if e = db.First(&current, "id = ?", task.ID).Error; e != nil {
		t.Fatal(e)
	}
	if current.Assignee != "lead2" || current.Version != 2 || current.ContentJSON != task.ContentJSON || current.Status != "PENDING" {
		t.Fatalf("恢复未保持冻结历史或版本：%+v", current)
	}
	var action store.Action
	db.Where("task_id = ?", task.ID).First(&action)
	var detail store.ActionDetail
	db.Where("action_id = ?", action.ID).First(&detail)
	if action.Action != "TRANSFER" || action.Actor != "operator" || detail.FromSubjectID != "lead" || detail.ToSubjectID != "lead2" {
		t.Fatalf("恢复缺少真实TRANSFER责任链：%+v %+v", action, detail)
	}
	s.deps.Personnel = nil
	again, e := s.Execute(context.Background(), operator, "approval.recover", task.ID, raw)
	if e != nil || string(again) != string(result) {
		t.Fatalf("成功恢复不能在目录故障时稳定重放：%s %v", again, e)
	}
	var count int64
	db.Model(&store.Action{}).Where("task_id = ?", task.ID).Count(&count)
	if count != 1 {
		t.Fatal("恢复重放重复创建历史")
	}
	s.deps.Personnel = workflowPersonnel{}
	lead := domain.Actor{TenantID: "t", SubjectID: "lead2", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	runCommand(t, s, lead, "approval.action", task.ID, map[string]any{"command_id": "recover-approve", "expected_version": 3, "task_version": 2, "round": 1, "action": "APPROVE"})
}

// TestApprovalRecoveryCurrentAuthorityNeedsNoSubstituteMapping 验证换任但通用审批能力仍在时可直接恢复到新权威岗位。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalRecoveryCurrentAuthorityNeedsNoSubstituteMapping(t *testing.T) {
	s, db, p, task, operator := approvalRecoveryFixture(t)
	s.deps.Personnel = workflowPersonnel{}
	db.Where("tenant_id = ? AND id = ?", "t", "recover-target").Delete(&store.Config{})
	p.references = []domain.ProjectReference{{ProjectID: "p1", ServiceItemID: "s", Eligible: true, RequiredApproverRefs: []domain.ApproverRef{{SubjectID: "lead2", NodeRole: "PROJECT_EXPENSE_OWNER", SourceField: "project_manager_id", SourceVersion: 2}}}}
	if _, e := s.Execute(context.Background(), operator, "approval.recover", task.ID, recoveryPayload("current-manager")); e != nil {
		t.Fatalf("当前新项目经理被旧人的通用审批能力或替代映射阻断：%v", e)
	}
	lead := domain.Actor{TenantID: "t", SubjectID: "lead2", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	runCommand(t, s, lead, "approval.action", task.ID, map[string]any{"command_id": "current-manager-approve", "expected_version": 3, "task_version": 2, "round": 1, "action": "APPROVE"})
}

// TestApprovalRecoveryRejectsUnsafeRequests 验证恢复授权、范围和依赖均失败关闭。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalRecoveryRejectsUnsafeRequests(t *testing.T) {
	cases := []struct{ name, change, want string }{
		{"无恢复权限", "permission", "FORBIDDEN"}, {"无组织范围", "scope", "FORBIDDEN"},
		{"跨租户", "tenant", "RESOURCE_NOT_FOUND"}, {"申请人恢复", "applicant", "FORBIDDEN"},
		{"原人仍有效", "eligible", "APPROVAL_RECOVERY_NOT_REQUIRED"},
		{"目录故障", "directory", "PERSONNEL_UNAVAILABLE"}, {"角色未配置", "role", "APPROVER_ROLE_MISSING"},
		{"无目标资格", "target", "APPROVER_INELIGIBLE"}, {"旧任务版本", "version", "VERSION_CONFLICT"},
		{"错误轮次", "round", "STALE_APPROVAL_TASK"}, {"空白原因", "reason", "INVALID_APPROVAL_RECOVERY"},
		{"目标为申请人", "self", "INVALID_APPROVAL_RECOVERY"}, {"目标为恢复人", "operator", "INVALID_APPROVAL_RECOVERY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, db, _, task, actor := approvalRecoveryFixture(t)
			body := map[string]any{}
			json.Unmarshal(recoveryPayload("reject-"+tc.change), &body)
			switch tc.change {
			case "permission":
				actor.Permissions = []string{"expense.claim.read"}
			case "scope":
				actor.SubjectID = "outside"
			case "tenant":
				actor.TenantID = "other"
			case "applicant":
				actor.SubjectID = "app"
			case "eligible":
				s.deps.Personnel = workflowPersonnel{}
			case "directory":
				s.deps.Personnel = recoveryDirectory{failed: map[string]error{"lead": domain.Fail("PERSONNEL_UNAVAILABLE", "目录超时", 503)}}
			case "role":
				s.deps.Personnel = recoveryDirectory{failed: map[string]error{"lead": domain.Fail("APPROVER_ROLE_MISSING", "目录配置缺失", 422)}}
			case "target":
				s.deps.Personnel = recoveryDirectory{failed: map[string]error{"lead": domain.Fail("APPROVER_INELIGIBLE", "离职", 422), "lead2": domain.Fail("APPROVER_INELIGIBLE", "无能力", 422)}}
			case "version":
				body["task_version"] = 2
			case "round":
				body["round"] = 2
			case "reason":
				body["reason"] = "   "
			case "self":
				body["target_subject_id"] = "app"
			case "operator":
				body["target_subject_id"] = "operator"
			}
			raw, _ := json.Marshal(body)
			_, e := s.Execute(context.Background(), actor, "approval.recover", task.ID, raw)
			problem, ok := e.(*domain.Problem)
			if !ok || problem.Code != tc.want {
				t.Fatalf("应拒绝为%s，实际%v", tc.want, e)
			}
			var after store.Task
			db.First(&after, "id = ?", task.ID)
			if after.Assignee != task.Assignee || after.Version != task.Version {
				t.Fatal("拒绝仍修改了任务")
			}
		})
	}
}

// TestApprovalRecoveryQueryUsesIndependentScope 验证恢复列表独立授权且不扩大普通任务受理人边界。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalRecoveryQueryUsesIndependentScope(t *testing.T) {
	s, _, _, task, operator := approvalRecoveryFixture(t)
	s.deps.Personnel = nil
	raw, e := s.QueryApprovalRecovery(context.Background(), operator, "", map[string]string{"page_size": "1"})
	if e != nil {
		t.Fatal(e)
	}
	var page struct {
		Items []domain.Task `json:"items"`
		Total int64         `json:"total"`
	}
	if e = json.Unmarshal(raw, &page); e != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != task.ID {
		t.Fatalf("恢复范围或分页错误：%s %v", raw, e)
	}
	operator.SubjectID = "outside"
	if _, e = s.QueryApprovalRecovery(context.Background(), operator, task.ID, nil); e == nil {
		t.Fatal("恢复详情跨组织泄露")
	}
	operator.Permissions = []string{"expense.claim.read", "expense.approval.act", "expense.scope.tenant_all"}
	if _, e = s.QueryApprovalRecovery(context.Background(), operator, "", nil); e == nil {
		t.Fatal("普通审批能力获得恢复列表")
	}
}

// TestApprovalRecoveryVacantAuthorityAllowsControlledSubstitute 验证原岗位空缺时仍可按精确有效替代配置恢复。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalRecoveryVacantAuthorityAllowsControlledSubstitute(t *testing.T) {
	s, _, p, task, operator := approvalRecoveryFixture(t)
	s.deps.Personnel = workflowPersonnel{}
	p.references = []domain.ProjectReference{{ProjectID: "p1", ServiceItemID: "s", Eligible: true, RequiredApproverRefs: []domain.ApproverRef{}}}
	if _, e := s.Execute(context.Background(), operator, "approval.recover", task.ID, recoveryPayload("vacant-manager")); e != nil {
		t.Fatalf("明确空缺岗位阻断合法替代恢复：%v", e)
	}
}

// TestApprovalRecoveryRefusesBrokenOriginalTransferChain 验证接管不能替已有不明受理人变更补造合法责任链。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalRecoveryRefusesBrokenOriginalTransferChain(t *testing.T) {
	s, db, _, task, operator := approvalRecoveryFixture(t)
	if e := db.Model(&store.Task{}).Where("id = ?", task.ID).Update("assignee", "unknown").Error; e != nil {
		t.Fatal(e)
	}
	s.deps.Personnel = recoveryDirectory{failed: map[string]error{"unknown": domain.Fail("APPROVER_INELIGIBLE", "当前无资格", 422)}}
	_, e := s.Execute(context.Background(), operator, "approval.recover", task.ID, recoveryPayload("broken-chain"))
	problem, ok := e.(*domain.Problem)
	if !ok || problem.Code != "APPROVER_AUTHORITY_CHANGED" {
		t.Fatalf("缺原始转交依据仍接管：%v", e)
	}
}

// TestApprovalRecoveryOperatorCanReadScopedHistory 验证恢复人员能按独立范围查看接管证据且撤权后不可读。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalRecoveryOperatorCanReadScopedHistory(t *testing.T) {
	s, db, _, task, operator := approvalRecoveryFixture(t)
	if _, e := s.Execute(context.Background(), operator, "approval.recover", task.ID, recoveryPayload("history-recovery")); e != nil {
		t.Fatal(e)
	}
	view, e := s.approvalHistory(db, operator, task.ID)
	if e != nil {
		t.Fatalf("独立恢复权限无法读取接管证据：%v", e)
	}
	raw, _ := json.Marshal(view)
	var history struct {
		Items []struct {
			RecoveryActor string `json:"recovery_actor"`
		} `json:"items"`
	}
	if e = json.Unmarshal(raw, &history); e != nil || len(history.Items) != 1 || history.Items[0].RecoveryActor != operator.SubjectID {
		t.Fatalf("接管证据不完整：%s %v", raw, e)
	}
	operator.SubjectID = "outside"
	if _, e = s.approvalHistory(db, operator, task.ID); e == nil {
		t.Fatal("无组织范围的恢复人读取证据")
	}
	operator.SubjectID = "operator"
	operator.Permissions = []string{"expense.claim.read"}
	if _, e = s.approvalHistory(db, operator, task.ID); e == nil {
		t.Fatal("已撤恢复权限仍读取证据")
	}
}

// TestApprovalRecoveryExpiredProofCannotWrite 验证事务外资格过期后不得修改任务或补写转交账本。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalRecoveryExpiredProofCannotWrite(t *testing.T) {
	s, db, _, task, actor := approvalRecoveryFixture(t)
	payload := recoveryPayload("stale-proof")
	cc := commandContext{actor: actor, resourceID: task.ID, operation: "approval.recover"}
	if e := json.Unmarshal(payload, &cc.input); e != nil {
		t.Fatal(e)
	}
	if e := s.preflightApprovalRecovery(context.Background(), db, &cc, payload); e != nil {
		t.Fatal(e)
	}
	cc.recovery.ExpiresAt = time.Now().Add(-time.Second)
	e := db.Transaction(func(tx *gorm.DB) error { _, _, err := s.recoverApproval(tx, &cc, payload); return err })
	problem, ok := e.(*domain.Problem)
	if !ok || problem.Code != "APPROVER_QUALIFICATION_STALE" {
		t.Fatalf("过期资格证明仍写入：%v", e)
	}
	var count int64
	db.Model(&store.Action{}).Where("task_id = ?", task.ID).Count(&count)
	if count != 0 {
		t.Fatal("过期资格产生责任链历史")
	}
}

// TestApprovalRecoveryAccountantStillNeedsFinancialScope 验证会计离职接管仍要求目标财务范围且不跳过节点审核。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalRecoveryAccountantStillNeedsFinancialScope(t *testing.T) {
	s, db, _, task, operator := approvalRecoveryFixture(t)
	s.deps.Personnel = workflowPersonnel{}
	lead := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	runCommand(t, s, lead, "approval.action", task.ID, map[string]any{"command_id": "before-accountant", "expected_version": 2, "task_version": 1, "round": 1, "action": "APPROVE"})
	claimID := task.ClaimID
	task = store.Task{}
	if e := db.Where("claim_id = ? AND node_code = ?", claimID, "ACCOUNTANT").First(&task).Error; e != nil {
		t.Fatal(e)
	}
	seedPolicy(t, db, "assignments", domain.Assignment{ID: "recover-accountant", SubjectID: "accountant", OrgID: "org", Role: "ACCOUNTANT", ValidFrom: "2026-01-01"})
	s.deps.Personnel = recoveryDirectory{failed: map[string]error{"acct": domain.Fail("APPROVER_INELIGIBLE", "会计离职", 422)}}
	payload := json.RawMessage(`{"command_id":"accountant-recover","expected_version":3,"task_version":2,"round":1,"reason":"会计离职交接","target_subject_id":"accountant"}`)
	_, e := s.Execute(context.Background(), operator, "approval.recover", task.ID, payload)
	problem, ok := e.(*domain.Problem)
	if !ok || problem.Code != "APPROVER_FINANCE_SCOPE_MISSING" {
		t.Fatalf("财务目标无明确范围仍接管：%v", e)
	}
	seedPolicy(t, db, "scopes", domain.Scope{ID: "new-accountant-scope", SubjectID: "accountant", OrgIDs: []string{"org"}})
	if _, e = s.Execute(context.Background(), operator, "approval.recover", task.ID, payload); e != nil {
		t.Fatal(e)
	}
	current, _, e := loadClaim(db, "t", task.ClaimID, false)
	if e != nil || current.Status != "IN_APPROVAL" {
		t.Fatalf("恢复跳过会计审核：%s %v", current.Status, e)
	}
}
