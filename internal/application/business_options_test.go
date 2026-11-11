package application

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

type selectionDirectoryFixture struct {
	recoveryDirectory
	people map[string]domain.PersonOption
	lookup []string
}

// LookupPerson 记录精确候选查询，模拟只有已授权当前人员才可命中。
// 参数：ctx/actor 为调用身份；target 为后端候选；permission 为节点能力。
// 返回值：当前安全人员资料及是否命中。
func (f *selectionDirectoryFixture) LookupPerson(ctx context.Context, actor domain.Actor, target, permission string) (domain.PersonOption, bool, error) {
	f.lookup = append(f.lookup, target)
	if permission != "" {
		if err := f.VerifyApprover(ctx, actor, target, permission); err != nil {
			return domain.PersonOption{}, false, nil
		}
	}
	person, found := f.people[target]
	return person, found, nil
}

// SearchPeople 模拟关键词目录，范围裁剪由真实业务服务执行。
// 参数：ctx/actor 为调用身份；keyword 为姓名；page/size 为受控分页。
// 返回值：测试目录安全资料。
func (f *selectionDirectoryFixture) SearchPeople(ctx context.Context, actor domain.Actor, keyword string, page, size int) (domain.PersonOptionsPage, error) {
	result := domain.PersonOptionsPage{Items: []domain.PersonOption{}, Total: int64(len(f.people))}
	for _, person := range f.people {
		result.Items = append(result.Items, person)
	}
	return result, nil
}

// TestPersonnelTransferOptionsStayInsideTaskScope 验证转交候选不是全租户人员目录。
// 参数：t 为测试上下文。
// 返回值：无；跨组织目标、他人任务或无权限访问被放行时失败。
func TestPersonnelTransferOptionsStayInsideTaskScope(t *testing.T) {
	s, db, _, task, _ := approvalRecoveryFixture(t)
	directory := &selectionDirectoryFixture{people: map[string]domain.PersonOption{
		"lead2": {ID: "lead2", Name: "李负责人"},
		"lead3": {ID: "lead3", Name: "跨组织负责人"},
	}}
	s.deps.Personnel = directory
	seedPolicy(t, db, "assignments", domain.Assignment{
		ID: "foreign-target", SubjectID: "lead3", OrgID: "foreign", Role: "PROJECT_EXPENSE_OWNER",
		ProjectID: "p1", ValidFrom: "2026-01-01",
	})
	actor := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.approval.act", "expense.approval.transfer"}}
	raw, err := s.PersonnelOptions(context.Background(), actor, "transfer", task.ID, "", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Items []domain.NameOption `json:"items"`
		Total int                 `json:"total"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "lead2" || result.Total != 1 ||
		len(directory.lookup) != 1 || directory.lookup[0] != "lead2" {
		t.Fatalf("转交选项超出任务范围: %s queries=%v", raw, directory.lookup)
	}
	actor.SubjectID = "lead3"
	if _, err = s.PersonnelOptions(context.Background(), actor, "transfer", task.ID, "", 1, 20); err == nil {
		t.Fatal("他人任务候选不得读取")
	}
	actor.SubjectID, actor.Permissions = "lead", []string{"expense.approval.act"}
	if _, err = s.PersonnelOptions(context.Background(), actor, "transfer", task.ID, "", 1, 20); err == nil {
		t.Fatal("无转交权限不得读取")
	}
}

// TestPersonnelPolicyOptionsRequireExplicitOrganizationScope 验证政策首配只提供授权组织内任职人员。
// 参数：t 为测试上下文。
// 返回值：无；缺范围或跨组织人员被放行时失败。
func TestPersonnelPolicyOptionsRequireExplicitOrganizationScope(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	s.deps.Personnel = &selectionDirectoryFixture{people: map[string]domain.PersonOption{
		"app":     {ID: "app", Name: "张申请", Organizations: []domain.OrganizationOption{{ID: "org", Name: "本部"}}},
		"foreign": {ID: "foreign", Name: "张其它", Organizations: []domain.OrganizationOption{{ID: "foreign", Name: "其它部"}}},
	}}
	actor := domain.Actor{TenantID: "t", SubjectID: "policy", Permissions: []string{"expense.config.manage"}}
	if _, err := s.PersonnelOptions(context.Background(), actor, "policy", "", "张", 1, 20); err == nil {
		t.Fatal("配置管理能力不能替代明确组织范围")
	}
	seedPolicy(t, db, "scopes", domain.Scope{ID: "policy-scope", SubjectID: "policy", OrgIDs: []string{"org"}})
	raw, err := s.PersonnelOptions(context.Background(), actor, "policy", "", "张", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Items []domain.NameOption `json:"items"`
		Total int                 `json:"total"`
	}
	json.Unmarshal(raw, &result)
	if len(result.Items) != 1 || result.Items[0].ID != "app" || result.Total != 1 {
		t.Fatalf("政策选项泄漏其它组织: %s", raw)
	}
}

// TestDocumentOptionsOnlyListOwnReadyPurpose 验证原件选择按本人、用途、就绪和字面关键词分页。
// 参数：t 为测试上下文。
// 返回值：无；跨用户、未就绪或其它用途文件被返回时失败。
func TestDocumentOptionsOnlyListOwnReadyPurpose(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	for _, row := range []store.Document{
		{ID: "own", TenantID: "t", Owner: "app", FileID: "own-file", Status: "READY", ContentJSON: `{"purpose":"reimbursement.invoice.original","original_name":"机票%原件.pdf"}`},
		{ID: "foreign", TenantID: "t", Owner: "other", FileID: "foreign-file", Status: "READY", ContentJSON: `{"purpose":"reimbursement.invoice.original","original_name":"机票%原件.pdf"}`},
		{ID: "pending", TenantID: "t", Owner: "app", FileID: "pending-file", Status: "UPLOAD_PENDING", ContentJSON: `{"purpose":"reimbursement.invoice.original","original_name":"机票%原件.pdf"}`},
		{ID: "evidence", TenantID: "t", Owner: "app", FileID: "evidence-file", Status: "READY", ContentJSON: `{"purpose":"reimbursement.claim.evidence","original_name":"机票%原件.pdf"}`},
		{ID: "wildcard", TenantID: "t", Owner: "app", FileID: "wildcard-file", Status: "READY", ContentJSON: `{"purpose":"reimbursement.invoice.original","original_name":"机票原件.pdf"}`},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload"}}
	raw, err := s.DocumentOptions(context.Background(), actor, "reimbursement.invoice.original", "%", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	json.Unmarshal(raw, &result)
	if result.Total != 1 || len(result.Items) != 1 || result.Items[0]["id"] != "own" ||
		result.Items[0]["name"] != "机票%原件.pdf" {
		t.Fatalf("原件选项越过边界: %s", raw)
	}
	actor.Permissions = nil
	if _, err = s.DocumentOptions(context.Background(), actor, "reimbursement.invoice.original", "", 1, 20); err == nil {
		t.Fatal("无上传能力不得列出原件选项")
	}
}

// TestPersonnelApplicantOptionsOnlyExposeReadableClaims 验证筛选姓名来自当前可读单据而非全租户。
// 参数：t 为测试上下文。
// 返回值：无；普通用户看到他人申请人或目录异常被忽略时失败。
func TestPersonnelApplicantOptionsOnlyExposeReadableClaims(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	directory := &selectionDirectoryFixture{people: map[string]domain.PersonOption{
		"app": {ID: "app", Name: "张申请"}, "other": {ID: "other", Name: "张其它"},
	}}
	s.deps.Personnel = directory
	for _, row := range []store.Claim{
		{ID: "own-claim", TenantID: "t", Applicant: "app", OrgID: "org"},
		{ID: "other-claim", TenantID: "t", Applicant: "other", OrgID: "org"},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	for _, keyword := range []string{"", "张"} {
		raw, err := s.PersonnelOptions(context.Background(), actor, "applicant", "", keyword, 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Items []domain.NameOption `json:"items"`
			Total int                 `json:"total"`
		}
		json.Unmarshal(raw, &result)
		if len(result.Items) != 1 || result.Items[0].ID != "app" || result.Total != 1 {
			t.Fatalf("申请人筛选扩大读取范围: %s", raw)
		}
	}
}

// TestPersonnelRecoveryOptionsIncludeCurrentAuthority 验证恢复可选择真实换任岗位及完整替代映射。
// 参数：t 为测试上下文。
// 返回值：无；合法新岗位缺失、无关人员出现或原资格仍有效时放行则失败。
func TestPersonnelRecoveryOptionsIncludeCurrentAuthority(t *testing.T) {
	s, _, projects, task, actor := approvalRecoveryFixture(t)
	directory := &selectionDirectoryFixture{
		recoveryDirectory: recoveryDirectory{failed: map[string]error{"lead": domain.Fail("APPROVER_INELIGIBLE", "已离职", 422)}},
		people: map[string]domain.PersonOption{
			"lead2": {ID: "lead2", Name: "受控替代人"}, "lead3": {ID: "lead3", Name: "新项目负责人"},
			"other": {ID: "other", Name: "无关人员"},
		},
	}
	s.deps.Personnel = directory
	projects.references = []domain.ProjectReference{{ProjectID: "p1", ServiceItemID: "s", Eligible: true,
		RequiredApproverRefs: []domain.ApproverRef{{SubjectID: "lead3", NodeRole: "PROJECT_EXPENSE_OWNER", SourceField: "project_manager_id", SourceVersion: 2}},
	}}
	raw, err := s.PersonnelOptions(context.Background(), actor, "recovery", task.ID, "", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Items []domain.NameOption `json:"items"`
		Total int                 `json:"total"`
	}
	json.Unmarshal(raw, &result)
	if result.Total != 2 || len(result.Items) != 2 {
		t.Fatalf("恢复候选范围错误: %s", raw)
	}
	for _, item := range result.Items {
		if item.ID != "lead2" && item.ID != "lead3" {
			t.Fatalf("不相关恢复候选: %s", raw)
		}
	}
	directory.failed = nil
	projects.references = nil
	if _, err = s.PersonnelOptions(context.Background(), actor, "recovery", task.ID, "", 1, 20); err == nil {
		t.Fatal("原人资格仍有效时不应生成恢复候选")
	}
}
