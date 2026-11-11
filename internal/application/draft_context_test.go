package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

type draftDirectoryFixture struct {
	workflowPersonnel
	person domain.PersonOption
	err    error
	calls  int
}

// CurrentApplicant 为财务范围专项夹具提供与原业务夹具一致的明确本人任职。
// 参数：ctx 为测试边界；actor 为夹具当前身份。
// 返回值：测试部门的安全本人资料。
func (roleAlignmentDirectory) CurrentApplicant(ctx context.Context, actor domain.Actor) (domain.PersonOption, error) {
	return (workflowPersonnel{}).CurrentApplicant(ctx, actor)
}

// TestDraftSaveAndSubmitRejectFormerOrganization 验证保存和提交均复核最新任职组织。
// 参数：t 为测试上下文。
// 返回值：无；本地旧资料或客户端组织可以代替当前任职时失败。
func TestDraftSaveAndSubmitRejectFormerOrganization(t *testing.T) {
	for _, operation := range []string{"claim.create", "claim.submit"} {
		t.Run(operation, func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			actor, id := submissionFixture(t, db, true)
			actor.Permissions = append(actor.Permissions, "expense.claim.create")
			s.deps.Personnel = &draftDirectoryFixture{person: domain.PersonOption{
				ID: "app", Name: "张申请", Organizations: []domain.OrganizationOption{
					{ID: "new-org", Name: "新部门", IsPrimary: true},
				},
			}}
			payload := json.RawMessage(`{"command_id":"former-org","expected_version":1,"org_id":"org","route_type":"TECHNICAL"}`)
			_, err := s.Execute(context.Background(), actor, operation, id, payload)
			var failure *domain.Problem
			if !errors.As(err, &failure) || failure.Code != "EMPLOYEE_ORGANIZATION_CHANGED" {
				t.Fatalf("旧组织未被拒绝: %v", err)
			}
		})
	}
}

// CurrentApplicant 返回测试指定的当前任职，不以本地费用配置替代平台事实。
// 参数：ctx 为调用边界；actor 为本人身份。
// 返回值：精确本人资料或模拟目录故障。
func (f *draftDirectoryFixture) CurrentApplicant(ctx context.Context, actor domain.Actor) (domain.PersonOption, error) {
	f.calls++
	return f.person, f.err
}

// TestDraftOptionsIntersectCurrentAppointments 验证草稿组织来自当前任职与费用档案交集。
// 参数：t 为测试上下文。
// 返回值：无；过期组织、他人资料或任意线路进入默认上下文时失败。
func TestDraftOptionsIntersectCurrentAppointments(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	directory := &draftDirectoryFixture{person: domain.PersonOption{
		ID: "app", Name: "张申请", Organizations: []domain.OrganizationOption{
			{ID: "sales", Name: "销售一部", IsPrimary: true},
			{ID: "technical", Name: "技术服务部"},
		},
	}}
	s.deps.Personnel = directory
	for _, profile := range []domain.EmployeeProfile{
		{ID: "sales-profile", SubjectID: "app", OrgID: "sales", RouteType: "SALES", Grade: "P2", Active: true},
		{ID: "technical-profile", SubjectID: "app", OrgID: "technical", RouteType: "TECHNICAL", Grade: "P3", Active: true},
		{ID: "old-profile", SubjectID: "app", OrgID: "old", RouteType: "TECHNICAL", Grade: "P9", Active: true},
		{ID: "other-profile", SubjectID: "other", OrgID: "sales", RouteType: "SALES", Grade: "P9", Active: true},
	} {
		seedPolicy(t, db, "employee_profiles", profile)
	}
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.create"}}
	raw, err := s.Query(context.Background(), actor, "claim-options", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Applicant     domain.PersonOption         `json:"applicant"`
		Organizations []domain.OrganizationOption `json:"organizations"`
		Profiles      []domain.EmployeeProfile    `json:"employee_profiles"`
		Context       domain.DraftContext         `json:"draft_context"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if directory.calls != 1 || result.Applicant.Name != "张申请" || len(result.Organizations) != 2 ||
		len(result.Profiles) != 2 || result.Context.Status != "READY" ||
		result.Context.OrgID != "sales" || result.Context.RouteType != "SALES" ||
		result.Context.EmployeeProfileID != "sales-profile" {
		t.Fatalf("未按真实本人任职导出上下文: %s", raw)
	}
}

// TestDraftOptionsFailClosedOnDirectoryLoss 验证目录故障不能回退为本地任意组织。
// 参数：t 为测试上下文。
// 返回值：无；目录缺失或故障仍返回选项时失败。
func TestDraftOptionsFailClosedOnDirectoryLoss(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "employee_profiles", domain.EmployeeProfile{
		ID: "local", SubjectID: "app", OrgID: "stale", RouteType: "TECHNICAL", Active: true,
	})
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.create"}}
	s.deps.Personnel = &draftDirectoryFixture{err: domain.Fail("PERSONNEL_UNAVAILABLE", "目录故障", 503)}
	if _, err := s.Query(context.Background(), actor, "claim-options", "", nil); err == nil {
		t.Fatal("目录故障不得使用陈旧本地组织")
	}
	s.deps.Personnel = &personnelFixture{}
	if _, err := s.Query(context.Background(), actor, "claim-options", "", nil); err == nil {
		t.Fatal("未接入当前任职接口不得退化为任意组织")
	}
}

// TestDraftContextExplainsMissingAndAmbiguousConfiguration 验证缺任职或费用资料时不伪造线路。
// 参数：t 为测试上下文。
// 返回值：无；缺配置或多份有效资料被静默选择时失败。
func TestDraftContextExplainsMissingAndAmbiguousConfiguration(t *testing.T) {
	person := domain.PersonOption{ID: "app", Name: "张申请"}
	if result := selectDraftContext(person, nil); result.Status != "CURRENT_APPOINTMENT_REQUIRED" || result.RouteType != "" {
		t.Fatalf("无任职不应派生组织线路: %+v", result)
	}
	person.Organizations = []domain.OrganizationOption{{ID: "org", Name: "本人部门", IsPrimary: true}}
	if result := selectDraftContext(person, nil); result.Status != "PROFILE_REQUIRED" || result.RouteType != "" || result.OrgID != "org" {
		t.Fatalf("缺费用资料不应推造职级线路: %+v", result)
	}
	profile := domain.EmployeeProfile{ID: "profile", OrgID: "org", Grade: "G1", RouteType: "TECHNICAL"}
	if result := selectDraftContext(person, []domain.EmployeeProfile{profile, profile}); result.Status != "PROFILE_AMBIGUOUS" || result.RouteType != "" {
		t.Fatalf("多份有效资料不应静默选取: %+v", result)
	}
}

// TestDraftAppointmentProofRejectsConfigurationChange 验证任职预检与提交事务之间的费用配置变化被拒绝。
// 参数：t 为测试上下文。
// 返回值：无；过期证明或资料变化继续持久化时失败。
func TestDraftAppointmentProofRejectsConfigurationChange(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	profile := domain.EmployeeProfile{ID: "profile", SubjectID: "app", OrgID: "org", Grade: "G1", RouteType: "TECHNICAL", Active: true, Version: 1}
	seedPolicy(t, db, "employee_profiles", profile)
	cc := commandContext{actor: domain.Actor{TenantID: "t", SubjectID: "app"}, operation: "claim.create", hash: "hash"}
	if err := s.preflightDraftAppointment(context.Background(), db, &cc, json.RawMessage(`{"org_id":"org","route_type":"TECHNICAL"}`)); err != nil {
		t.Fatal(err)
	}
	c := domain.Claim{TenantID: "t", Applicant: "app", OrgID: "org", RouteType: "TECHNICAL"}
	if err := verifyDraftAppointment(db, &cc, c); err != nil {
		t.Fatal(err)
	}
	cc.appointment.VerifiedAt = time.Now().Add(-2 * time.Minute)
	if err := verifyDraftAppointment(db, &cc, c); err == nil {
		t.Fatal("过期任职证明未拒绝")
	}
	cc.appointment.VerifiedAt = time.Now()
	profile.Version = 2
	raw, _ := json.Marshal(profile)
	if err := db.Model(&store.Config{}).Where("id = ?", profile.ID).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	if err := verifyDraftAppointment(db, &cc, c); err == nil {
		t.Fatal("费用资料版本改变未拒绝")
	}
}
