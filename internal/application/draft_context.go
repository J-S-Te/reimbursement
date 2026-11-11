package application

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	"gorm.io/gorm"
)

type applicantDirectory interface {
	CurrentApplicant(context.Context, domain.Actor) (domain.PersonOption, error)
}

type draftAppointmentProof struct {
	TenantID, SubjectID, OrgID, RouteType, ProfileID, CommandHash string
	ProfileVersion                                                int64
	VerifiedAt                                                    time.Time
}

// preflightDraftAppointment 在事务外验证草稿本人当前任职与已发布费用线路。
// 参数：ctx/db 为查询边界；cc 为命令身份版本；payload 为草稿内容。
// 返回值：当前组织与费用资料证明；目录故障、旧任职或线路不匹配时拒绝。
func (s *Service) preflightDraftAppointment(ctx context.Context, db *gorm.DB, cc *commandContext, payload json.RawMessage) error {
	var c domain.Claim
	if cc.operation == "claim.submit" {
		loaded, _, err := loadClaim(db, cc.actor.TenantID, cc.resourceID, false)
		if err != nil {
			return err
		}
		if loaded.Applicant != cc.actor.SubjectID {
			return domain.Fail("FORBIDDEN", "只能提交本人单据", 403)
		}
		if err = verifyVersion(loaded.Version, cc.input.ExpectedVersion); err != nil {
			return err
		}
		c = loaded
	} else if err := json.Unmarshal(payload, &c); err != nil {
		return domain.Fail("INVALID_REQUEST", "草稿字段不正确", 422)
	}
	if s.deps.Personnel == nil {
		return domain.Fail("PERSONNEL_DIRECTORY_UNAVAILABLE", "当前人员任职目录未配置", 503)
	}
	person, err := s.currentDraftApplicant(ctx, cc.actor)
	if err != nil {
		return err
	}
	proof := &draftAppointmentProof{
		TenantID: cc.actor.TenantID, SubjectID: cc.actor.SubjectID, OrgID: c.OrgID,
		CommandHash: cc.hash, VerifiedAt: time.Now().UTC(),
	}
	// 未填写组织的未完成草稿仍可暂存；提交与所有已填组织必须具有当前任职。
	if c.OrgID == "" && cc.operation != "claim.submit" {
		cc.appointment = proof
		return nil
	}
	member := false
	for _, org := range person.Organizations {
		member = member || org.ID == c.OrgID
	}
	if !member {
		return domain.Fail("EMPLOYEE_ORGANIZATION_CHANGED", "当前部门不属于本人有效任职，请刷新后重新选择", 422)
	}
	c.TenantID, c.Applicant = cc.actor.TenantID, cc.actor.SubjectID
	profile, err := employeeProfile(db, c)
	if err != nil {
		return err
	}
	if c.RouteType != "" && c.RouteType != profile.RouteType {
		return domain.Fail("ROUTE_TYPE_MISMATCH", "费用线路与已审核人员资料不一致", 422)
	}
	proof.ProfileID, proof.ProfileVersion, proof.RouteType = profile.ID, profile.Version, profile.RouteType
	cc.appointment = proof
	return nil
}

// verifyDraftAppointment 在持锁事务内复核事务外任职证明及当前本地费用配置版本。
// 参数：tx 为当前事务；cc 为同一命令证明；c 为待保存或提交单据。
// 返回值：证明缺失、过期或本地资料变化时拒绝；不在事务内调用远端目录。
func verifyDraftAppointment(tx *gorm.DB, cc *commandContext, c domain.Claim) error {
	p := cc.appointment
	if p == nil || p.TenantID != cc.actor.TenantID || p.SubjectID != cc.actor.SubjectID ||
		p.CommandHash != cc.hash || p.OrgID != c.OrgID || time.Since(p.VerifiedAt) < 0 ||
		time.Since(p.VerifiedAt) > time.Minute {
		return domain.Fail("EMPLOYEE_APPOINTMENT_STALE", "当前任职证明已失效，请刷新重试", 409)
	}
	if c.OrgID == "" && cc.operation != "claim.submit" {
		return nil
	}
	profile, err := employeeProfile(tx, c)
	if err != nil {
		return err
	}
	if profile.ID != p.ProfileID || profile.Version != p.ProfileVersion ||
		profile.RouteType != p.RouteType || c.RouteType != p.RouteType {
		return domain.Fail("EMPLOYEE_PROFILE_CHANGED", "费用资料或线路已改变，请刷新后重新保存", 409)
	}
	return nil
}

// currentDraftApplicant 只读取当前平台任职，不从财务范围或费用档案推造组织。
// 参数：ctx 为调用边界；actor 为可信本人身份。
// 返回值：本人安全姓名和组织；接口未装配或错用户时返回503。
func (s *Service) currentDraftApplicant(ctx context.Context, actor domain.Actor) (domain.PersonOption, error) {
	directory, ok := s.deps.Personnel.(applicantDirectory)
	if !ok {
		return domain.PersonOption{}, domain.Fail("PERSONNEL_UNAVAILABLE", "当前人员任职服务未配置", 503)
	}
	person, err := directory.CurrentApplicant(ctx, actor)
	if err != nil {
		return domain.PersonOption{}, err
	}
	if person.ID != actor.SubjectID {
		return domain.PersonOption{}, domain.Fail("PERSONNEL_UNAVAILABLE", "人员任职响应不一致", 503)
	}
	return person, nil
}

// selectDraftContext 以主任职组织及唯一有效费用资料决定只读部门和业务线路。
// 参数：person 为平台本人资料；profiles 为当前任职组织内已发布有效费用资料。
// 返回值：可用上下文，或明确的缺任职、缺配置、配置歧义状态。
func selectDraftContext(person domain.PersonOption, profiles []domain.EmployeeProfile) domain.DraftContext {
	result := domain.DraftContext{Status: "CURRENT_APPOINTMENT_REQUIRED", Message: "当前账号没有可用的在职组织资料，请联系管理员核对任职"}
	if len(person.Organizations) == 0 {
		return result
	}
	primary := []string{}
	for _, org := range person.Organizations {
		if org.IsPrimary || len(person.Organizations) == 1 {
			primary = append(primary, org.ID)
		}
	}
	if len(primary) != 1 {
		return domain.DraftContext{Status: "PROFILE_AMBIGUOUS", Message: "当前任职没有唯一主组织，请联系管理员确认"}
	}
	result = domain.DraftContext{Status: "PROFILE_REQUIRED", OrgID: primary[0], Message: "当前组织尚未配置有效的费用职级和业务线路"}
	matching := []domain.EmployeeProfile{}
	for _, profile := range profiles {
		if profile.OrgID == result.OrgID {
			matching = append(matching, profile)
		}
	}
	if len(matching) > 1 {
		result.Status, result.Message = "PROFILE_AMBIGUOUS", "当前组织存在多份有效费用资料，请联系管理员核对"
		return result
	}
	if len(matching) != 1 || strings.TrimSpace(matching[0].Grade) == "" ||
		(matching[0].RouteType != "SALES" && matching[0].RouteType != "TECHNICAL") {
		return result
	}
	result.Status, result.Message = "READY", ""
	result.RouteType, result.EmployeeProfileID = matching[0].RouteType, matching[0].ID
	return result
}
