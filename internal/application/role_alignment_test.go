package application

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"strings"
	"testing"
)

// TestRoleAlignmentScopeBoundary 验证全租户能力显式授权且不能跨租户。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentScopeBoundary(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.claim.read", "expense.audit.read", "expense.scope.tenant_all"}}
	c := domain.Claim{TenantID: "t", Applicant: "other", OrgID: "org"}
	if ok, e := s.fullRead(db, a, c); e != nil || !ok {
		t.Fatalf("显式全租户能力应允许本租户: %v %v", ok, e)
	}
	c.TenantID = "foreign"
	if ok, _ := s.fullRead(db, a, c); ok {
		t.Fatal("全租户能力不能跨租户")
	}
	a.Permissions = []string{"expense.claim.read", "expense.audit.read"}
	c.TenantID = "t"
	if ok, _ := s.fullRead(db, a, c); ok {
		t.Fatal("普通财务能力不能推断范围")
	}
}

// TestRoleAlignmentAuthorityIgnoresLocalBusinessMapping 验证首次业务节点仅使用项目经理权威来源。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentAuthorityIgnoresLocalBusinessMapping(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "route_rules", domain.RouteRule{ID: "r", OrgID: "org", RouteType: "TECHNICAL", ValidFrom: "2020-01-01", BaseRole: "PROJECT_EXPENSE_OWNER", ExtraRole: "TECHNICAL_DIRECTOR", AccountingRole: "ACCOUNTANT"})
	seedPolicy(t, db, "assignments", domain.Assignment{ID: "local", OrgID: "org", ProjectID: "p", Role: "PROJECT_EXPENSE_OWNER", SubjectID: "wrong", ValidFrom: "2020-01-01"})
	seedPolicy(t, db, "assignments", domain.Assignment{ID: "acct", OrgID: "org", Role: "ACCOUNTANT", SubjectID: "acct", ValidFrom: "2020-01-01"})
	c := domain.Claim{TenantID: "t", ID: "c", Applicant: "app", OrgID: "org", RouteType: "TECHNICAL", Items: []domain.Item{{ID: "i", Allocations: []domain.Allocation{{ProjectID: "p"}}}}}
	var refs []domain.ProjectReference
	_ = json.Unmarshal([]byte(`[{"project_id":"p","required_approver_refs":[{"subject_id":"manager","node_role":"PROJECT_EXPENSE_OWNER","source_field":"manager_identity_id","source_version":3}]}]`), &refs)
	plan, e := buildApprovalPlan(db, c, refs)
	if e != nil {
		t.Fatal(e)
	}
	if plan.Tasks[0].Assignee != "manager" {
		t.Fatalf("本地映射覆盖项目经理: %+v", plan.Tasks)
	}
	refs[0].RequiredApproverRefs = nil
	if _, e = buildApprovalPlan(db, c, refs); e == nil {
		t.Fatal("缺权威经理不可回退本地映射")
	}
}

// TestRoleAlignmentChangedManagerCannotApprove 防止历史受理人凭通用能力继续办理已失效项目岗位。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentChangedManagerCannotApprove(t *testing.T) {
	s, db, p := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	runCommand(t, s, a, "claim.submit", id, map[string]any{"command_id": "role-submit", "expected_version": 1})
	var task store.Task
	if e := db.Where("claim_id = ? AND node = 1", id).First(&task).Error; e != nil {
		t.Fatal(e)
	}
	p.references = []domain.ProjectReference{{ProjectID: "p1", ServiceItemID: "s", Eligible: true, RequiredApproverRefs: []domain.ApproverRef{{NodeRole: "PROJECT_EXPENSE_OWNER", SubjectID: "lead2", SourceField: "project_manager_id", SourceVersion: 2}}}}
	actor := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	raw, _ := json.Marshal(map[string]any{"command_id": "old-manager", "expected_version": 2, "task_version": task.Version, "round": 1, "action": "APPROVE"})
	if _, e := s.Execute(context.Background(), actor, "approval.action", task.ID, raw); e == nil {
		t.Fatal("已失效项目经理仍可审批")
	}
}

// TestRoleAlignmentSameServiceDatesRemainSeparate 防止同服务不同费用日的权威目标互相扩大分摊范围。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentSameServiceDatesRemainSeparate(t *testing.T) {
	c := domain.Claim{Items: []domain.Item{{ID: "early", OccurredDate: "2026-11-01", Allocations: []domain.Allocation{{ProjectID: "p", ServiceItemID: "s"}}}, {ID: "late", OccurredDate: "2026-11-02", Allocations: []domain.Allocation{{ProjectID: "p", ServiceItemID: "s"}}}}}
	refs := []domain.ProjectReference{{ProjectID: "p", ServiceItemID: "s", RequiredApproverRefs: []domain.ApproverRef{{SubjectID: "old", NodeRole: "PROJECT_EXPENSE_OWNER", SourceField: "project_manager_id", SourceVersion: 1}}}, {ProjectID: "p", ServiceItemID: "s", RequiredApproverRefs: []domain.ApproverRef{{SubjectID: "new", NodeRole: "PROJECT_EXPENSE_OWNER", SourceField: "project_manager_id", SourceVersion: 2}}}}
	keys, e := projectRoleKeys(c, refs, "PROJECT_EXPENSE_OWNER")
	if e != nil {
		t.Fatal(e)
	}
	if len(keys["old"]) != 1 || keys["old"][0] != "early/p/s" || len(keys["new"]) != 1 || keys["new"][0] != "late/p/s" {
		t.Fatalf("日期责任范围扩散: %+v", keys)
	}
}

// TestRoleAlignmentAdminReadsTenantUnreservedInvoice 验证全租户读取覆盖未占用票据但保持租户边界。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentAdminReadsTenantUnreservedInvoice(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	submissionFixture(t, db, true)
	a := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.invoice.read", "expense.scope.tenant_all"}}
	if _, e := s.Query(context.Background(), a, "invoice", "invoice", nil); e != nil {
		t.Fatalf("未占用票据被全量范围误拒:%v", e)
	}
	a.TenantID = "foreign"
	if _, e := s.Query(context.Background(), a, "invoice", "invoice", nil); e == nil {
		t.Fatal("跨租户票据泄露")
	}
}

// TestRoleAlignmentManagerSubstituteRequiresExactProject 防止旧通用替代映射扩大经理的项目边界。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentManagerSubstituteRequiresExactProject(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "assignments", domain.Assignment{ID: "generic", SubjectID: "lead2", OrgID: "org", Role: "BUSINESS_REVIEW", ValidFrom: "2020-01-01"})
	task := domain.Task{NodeRole: "PROJECT_EXPENSE_OWNER", NodeCode: "BUSINESS_REVIEW", AllocationKeys: []string{"i/p/s"}}
	if _, e := taskTransferAssignments(db, domain.Claim{TenantID: "t", OrgID: "org"}, task, "lead2"); e == nil {
		t.Fatal("无project通用替代映射扩大经理范围")
	}
}

// TestRoleAlignmentDirectorProofAndSalesOrganization 验证多总监ALL与销售组织绑定且不要求伪造目录数字版本。
// 参数：t 为权威来源测试上下文。
// 返回值：无。
func TestRoleAlignmentDirectorProofAndSalesOrganization(t *testing.T) {
	c := domain.Claim{OrgID: "sales-org", RouteType: "SALES", Items: []domain.Item{{ID: "i", OccurredDate: "2026-11-01", Allocations: []domain.Allocation{{ProjectID: "p", ServiceItemID: "s"}}}}}
	refs := []domain.ProjectReference{{ProjectID: "p", ServiceItemID: "s", RequiredApproverRefs: []domain.ApproverRef{{SubjectID: "d1", NodeRole: "SALES_DIRECTOR", SourceField: "role:sales_director", SourceOrgID: "sales-org", SourceDigest: strings.Repeat("a", 64)}, {SubjectID: "d2", NodeRole: "SALES_DIRECTOR", SourceField: "role:sales_director", SourceOrgID: "sales-org", SourceDigest: strings.Repeat("b", 64)}}}}
	keys, e := projectRoleKeys(c, refs, "SALES_DIRECTOR")
	if e != nil || len(keys) != 2 {
		t.Fatalf("目录无数字版本的多总监应全部生成任务:%+v %v", keys, e)
	}
	requests := claimReferences(c)
	if len(requests) != 1 || requests[0].ApprovalOrgID != "sales-org" {
		t.Fatal("销售审批组织未绑定服务端费用组织")
	}
	refs[0].RequiredApproverRefs[0].SourceOrgID = "technical-org"
	refs[0].RequiredApproverRefs = refs[0].RequiredApproverRefs[:1]
	if _, e = projectRoleKeys(c, refs, "SALES_DIRECTOR"); e == nil {
		t.Fatal("误用项目技术组织的销售总监")
	}
	refs[0].RequiredApproverRefs[0].SourceOrgID = "sales-org"
	refs[0].RequiredApproverRefs[0].SourceDigest = ""
	if _, e = projectRoleKeys(c, refs, "SALES_DIRECTOR"); e == nil {
		t.Fatal("无真实目录摘要被接受")
	}
}

// TestRoleAlignmentAdminCannotPaySelfOrActAsCashierAfterAccounting 验证全量能力不会绕过自付和会计出纳分离。
// 参数：t 为真实应用隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentAdminCannotPaySelfOrActAsCashierAfterAccounting(t *testing.T) {
	s, _, c, _, _ := approvedControlFixture(t)
	for _, tc := range []struct{ subject, code string }{{"app", "SELF_PAYMENT_FORBIDDEN"}, {"acct", "ACCOUNTANT_CASHIER_CONFLICT"}} {
		actor := domain.Actor{TenantID: "t", SubjectID: tc.subject, Permissions: []string{"expense.claim.read", "expense.payment.record", "expense.scope.tenant_all"}}
		raw, _ := json.Marshal(map[string]any{"command_id": "admin-pay-" + tc.subject, "expected_version": c.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "admin-attempt", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}})
		_, e := s.Execute(context.Background(), actor, "payment.record", c.ID, raw)
		var problem *domain.Problem
		if !errors.As(e, &problem) || problem.Code != tc.code {
			t.Fatalf("全量能力绕过职责隔离: %v", e)
		}
	}
}

// TestRoleAlignmentAdminCannotApproveUnassignedTask 验证超级管理员仍不能越过任务受理人边界。
// 参数：t 为真实应用隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentAdminCannotApproveUnassignedTask(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "admin-assignee-submit", "expected_version": 1})
	var task store.Task
	db.Where("claim_id = ? AND node = 1", id).First(&task)
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read", "expense.approval.act", "expense.scope.tenant_all"}}
	raw, _ := json.Marshal(map[string]any{"command_id": "admin-assignee", "expected_version": 2, "task_version": 1, "round": 1, "action": "APPROVE"})
	if _, e := s.Execute(context.Background(), actor, "approval.action", task.ID, raw); e == nil {
		t.Fatal("全租户能力允许自审或办理他人任务")
	}
}

// TestRoleAlignmentCurrentTaskChecksOnlyCoveredAllocations 验证另一项目岗位缺失不会阻断当前仍有效项目受理人。
// 参数：t 为真实应用隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentCurrentTaskChecksOnlyCoveredAllocations(t *testing.T) {
	s, db, p := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	c, _, _ := loadClaim(db, "t", id, false)
	c.Items[0].Allocations = []domain.Allocation{{ProjectID: "p1", ServiceItemID: "s", Amount: 50}, {ProjectID: "p2", ServiceItemID: "s", Amount: 50}}
	raw, _ := encode(c)
	db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", raw)
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "scope-submit", "expected_version": 1})
	var task store.Task
	db.Where("claim_id = ? AND assignee = ?", id, "lead").First(&task)
	p.references = []domain.ProjectReference{{ProjectID: "p1", ServiceItemID: "s", Eligible: true, RequiredApproverRefs: []domain.ApproverRef{{SubjectID: "lead", NodeRole: "PROJECT_EXPENSE_OWNER", SourceField: "project_manager_id", SourceVersion: 2}}}, {ProjectID: "p2", ServiceItemID: "s", Eligible: true}}
	actor := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	rawJSON, _ := json.Marshal(map[string]any{"command_id": "scope-approve", "expected_version": 2, "task_version": task.Version, "round": 1, "action": "APPROVE"})
	if _, e := s.Execute(context.Background(), actor, "approval.action", task.ID, rawJSON); e != nil {
		t.Fatalf("无关项目岗位缺失阻断当前覆盖范围:%v", e)
	}
}

type roleAlignmentDirectory struct{ marker bool }

// VerifyApprover 仅授予夹具明确的财务审核能力，独立模拟当前全量标记。
// 参数：ctx 为边界，actor 为操作者，target/permission 为精确待核能力。
// 返回值：当前目标明确有权时成功，否则拒绝。
func (p roleAlignmentDirectory) VerifyApprover(ctx context.Context, actor domain.Actor, target, permission string) error {
	if target == "admin-acct" {
		if permission == "expense.finance.review" || (permission == "expense.scope.tenant_all" && p.marker) {
			return nil
		}
		return domain.Fail("APPROVER_INELIGIBLE", "夹具当前无该能力", 422)
	}
	return (workflowPersonnel{}).VerifyApprover(ctx, actor, target, permission)
}

// TestRoleAlignmentFinancialTargetRequiresPublishedScopeOrCurrentMarker 验证财务首次目标不能靠旧管理员身份或普通能力获得范围。
// 参数：t 为真实应用隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentFinancialTargetRequiresPublishedScopeOrCurrentMarker(t *testing.T) {
	for _, marker := range []bool{false, true} {
		s, db, _ := setupWorkflow(t)
		app, id := submissionFixture(t, db, false)
		seedPolicy(t, db, "assignments", domain.Assignment{ID: "admin-accountant", SubjectID: "admin-acct", OrgID: "org", Role: "ACCOUNTANT", ValidFrom: "2020-01-01"})
		s.deps.Personnel = roleAlignmentDirectory{marker: marker}
		_, e := s.Execute(context.Background(), app, "claim.submit", id, json.RawMessage(`{"command_id":"target-scope","expected_version":1}`))
		if marker && e != nil {
			t.Fatalf("已实时证明全租户标记的目标被误拒:%v", e)
		}
		if !marker && e == nil {
			t.Fatal("普通能力目标无发布范围仍通过")
		}
	}
}

// TestRoleAlignmentExportDownloadRechecksCurrentMarker 验证已生成结果下载仍按当前收窄后的对象权限复核。
// 参数：t 为真实应用隔离测试上下文。
// 返回值：无。
func TestRoleAlignmentExportDownloadRechecksCurrentMarker(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	submissionFixture(t, db, true)
	payload := `{"actor":{"tenant_id":"t","subject_id":"admin"},"result":{"claim_ids":["claim"],"content":"approved csv"}}`
	if e := db.Create(&store.Job{ID: "export", TenantID: "t", Kind: "EXPORT", Status: "DONE", PayloadJSON: payload}).Error; e != nil {
		t.Fatal(e)
	}
	actor := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.claim.read", "expense.audit.read", "expense.archive.export", "expense.scope.tenant_all"}}
	if _, e := s.Query(context.Background(), actor, "exports", "export", nil); e != nil {
		t.Fatal(e)
	}
	actor.Permissions = actor.Permissions[:3]
	if _, e := s.Query(context.Background(), actor, "exports", "export", nil); e == nil {
		t.Fatal("撤全量标记后仍下载旧全量结果")
	}
}

// TestRoleAlignmentSalesFirstAndTechnicalExceptionUseSourceALL 验证两线路顺序和技术异常全体权威总监任务。
// 参数：t 为真实路线生成上下文。
// 返回值：无。
func TestRoleAlignmentSalesFirstAndTechnicalExceptionUseSourceALL(t *testing.T) {
	for _, route := range []string{"TECHNICAL", "SALES"} {
		_, db, _ := setupWorkflow(t)
		base, extra := "PROJECT_EXPENSE_OWNER", "TECHNICAL_DIRECTOR"
		if route == "SALES" {
			base = "SALES_DIRECTOR"
			extra = "FINANCE_DIRECTOR"
		}
		seedPolicy(t, db, "route_rules", domain.RouteRule{ID: "route", OrgID: "sales-org", RouteType: route, BaseRole: base, ExtraRole: extra, AccountingRole: "ACCOUNTANT", ValidFrom: "2020-01-01"})
		for _, role := range []string{"ACCOUNTANT", "FINANCE_DIRECTOR", "TECHNICAL_DIRECTOR", "SALES_DIRECTOR"} {
			seedPolicy(t, db, "assignments", domain.Assignment{ID: "local-" + role, OrgID: "sales-org", Role: role, SubjectID: "local-" + role, ValidFrom: "2020-01-01"})
		}
		c := domain.Claim{TenantID: "t", OrgID: "sales-org", Applicant: "app", RouteType: route, Items: []domain.Item{{ID: "i", OverStandard: true, Allocations: []domain.Allocation{{ProjectID: "p", ServiceItemID: "s"}}}}}
		refs := []domain.ProjectReference{{ProjectID: "p", ServiceItemID: "s", RequiredApproverRefs: []domain.ApproverRef{{SubjectID: "pm", NodeRole: "PROJECT_EXPENSE_OWNER", SourceField: "project_manager_id", SourceVersion: 3}, {SubjectID: "tech1", NodeRole: "TECHNICAL_DIRECTOR", SourceField: "role:technical_director", SourceOrgID: "technical-org", SourceDigest: strings.Repeat("a", 64)}, {SubjectID: "tech2", NodeRole: "TECHNICAL_DIRECTOR", SourceField: "role:technical_director", SourceOrgID: "technical-org", SourceDigest: strings.Repeat("b", 64)}, {SubjectID: "sales", NodeRole: "SALES_DIRECTOR", SourceField: "role:sales_director", SourceOrgID: "sales-org", SourceDigest: strings.Repeat("c", 64)}}}}
		plan, e := buildApprovalPlan(db, c, refs)
		if e != nil {
			t.Fatal(e)
		}
		if route == "SALES" {
			if plan.Tasks[0].Assignee != "sales" || plan.Tasks[1].NodeRole != "FINANCE_DIRECTOR" || plan.Tasks[1].Assignee != "local-FINANCE_DIRECTOR" {
				t.Fatalf("销售线路来源或顺序错误:%+v", plan.Tasks)
			}
		} else {
			if len(plan.Tasks) != 4 || plan.Tasks[0].Assignee != "pm" || plan.Tasks[1].Assignee != "tech1" || plan.Tasks[2].Assignee != "tech2" || plan.Tasks[1].Node != plan.Tasks[2].Node {
				t.Fatalf("技术总监未使用权威ALL来源:%+v", plan.Tasks)
			}
		}
	}
}

// TestRoleAlignmentOnlyTransferParentEstablishesSubstitution 防止普通资格证明被误当成不可变转交依据。
// 参数：t 为隔离数据库上下文。
// 返回值：无。
func TestRoleAlignmentOnlyTransferParentEstablishesSubstitution(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	frozen, _ := encode(domain.Task{Assignee: "lead"})
	row := store.Task{ID: "task", TenantID: "t", ClaimID: "c", Version: 2, Assignee: "lead", ContentJSON: frozen}
	if e := db.Create(&store.Action{ID: "action", TenantID: "t", ClaimID: "c", TaskID: "task", Action: "APPROVE"}).Error; e != nil {
		t.Fatal(e)
	}
	if e := db.Create(&store.ActionDetail{ID: "detail", TenantID: "t", ClaimID: "c", TaskID: "task", ActionID: "action", ToSubjectID: "lead", ContentJSON: `{"task_version_after":2}`}).Error; e != nil {
		t.Fatal(e)
	}
	substitute, e := hasTransferAuthority(db, row)
	if e != nil || substitute {
		t.Fatalf("普通审批证明变成转交依据:%v %v", substitute, e)
	}
}
