package application

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// taskPersonnelOptions 根据当前任务责任范围提供转交或受控恢复候选。
// 参数：ctx/db/a 为可信边界；purpose 为转交或恢复；taskID 为当前任务。
// 返回值：当前可用目标姓名；不会以租户目录替代映射或项目职责校验。
func (s *Service) taskPersonnelOptions(ctx context.Context, db *gorm.DB, a domain.Actor, purpose, taskID string) ([]domain.NameOption, error) {
	c, row, task, err := s.loadPersonnelOptionTask(db, a, purpose, taskID)
	if err != nil {
		return nil, err
	}
	directory, ok := s.deps.Personnel.(selectionDirectory)
	if !ok {
		return nil, domain.Fail("PERSONNEL_UNAVAILABLE", "人员姓名服务未配置", 503)
	}
	if purpose == "recovery" {
		input := ApprovalInput{TaskVersion: row.Version, Round: row.Round}
		input.ExpectedVersion = c.Version
		if _, err = s.approvalRecoveryLoss(ctx, db, &commandContext{actor: a, resourceID: row.ID}, input, c, row, task); err != nil {
			return nil, err
		}
	}
	ids, err := s.taskPersonnelCandidateIDs(ctx, db, a, purpose, c, row, task)
	if err != nil {
		return nil, err
	}
	if len(ids) > 100 {
		return nil, domain.Fail("OPTIONS_LIMIT", "任务候选超过受控上限，请管理员整理映射", 422)
	}
	items := []domain.NameOption{}
	for _, id := range mapKeys(ids) {
		person, found, err := directory.LookupPerson(ctx, a, id, task.RequiredPermission)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if person.ID != id || person.Name == "" {
			return nil, domain.Fail("PERSONNEL_UNAVAILABLE", "人员目录返回不一致", 503)
		}
		if task.NodeCode == "ACCOUNTANT" || task.NodeCode == "FINANCE_DIRECTOR" {
			if _, err = s.verifyTargetFinanceScope(ctx, db, a, id, c.OrgID); err != nil {
				var failure *domain.Problem
				if errors.As(err, &failure) && failure.Code == "APPROVER_FINANCE_SCOPE_MISSING" {
					continue
				}
				return nil, err
			}
		}
		items = append(items, domain.NameOption{ID: id, Name: person.Name})
	}
	return items, nil
}

// loadPersonnelOptionTask 在解析候选姓名前检查具体动作、当前任务和单据范围。
// 参数：db/a 为查询身份；purpose 为动作；taskID 为任务标识。
// 返回值：当前单据、任务和冻结责任；无权或状态失效时拒绝。
func (s *Service) loadPersonnelOptionTask(db *gorm.DB, a domain.Actor, purpose, taskID string) (domain.Claim, store.Task, domain.Task, error) {
	var row store.Task
	var task domain.Task
	var c domain.Claim
	permission := "expense.approval.transfer"
	if purpose == "recovery" {
		permission = "expense.approval.recover"
	}
	if err := require(a, permission); err != nil {
		return c, row, task, err
	}
	q := db.Where("tenant_id = ? AND id = ?", a.TenantID, taskID)
	if purpose == "transfer" {
		q = q.Where("assignee = ?", a.SubjectID)
	}
	if err := q.First(&row).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return c, row, task, err
		}
		return c, row, task, domain.Fail("RESOURCE_NOT_FOUND", "当前审批任务不存在或无权操作", 404)
	}
	c, _, err := loadClaim(db, a.TenantID, row.ClaimID, false)
	if err != nil {
		return c, row, task, err
	}
	if purpose == "recovery" {
		err = requireApprovalRecoveryScope(db, a, c)
	} else {
		err = require(a, "expense.approval.act")
	}
	if err != nil {
		return c, row, task, err
	}
	if row.Status != "PENDING" || c.Status != "IN_APPROVAL" || row.Round != c.CurrentRound {
		return c, row, task, domain.Fail("STALE_APPROVAL_TASK", "仅当前轮次有效待办可选择受理人", 409)
	}
	if err = json.Unmarshal([]byte(row.ContentJSON), &task); err != nil {
		return c, row, task, err
	}
	if task.RequiredPermission == "" {
		return c, row, task, domain.Fail("TASK_CONFIGURATION_INVALID", "任务审批能力不完整", 422)
	}
	if purpose == "transfer" {
		err = require(a, task.RequiredPermission)
	}
	return c, row, task, err
}

// taskPersonnelCandidateIDs 仅从完整有效替代映射及恢复时当前权威岗位提取候选。
// 参数：ctx/db/a 为查询身份；purpose 为动作；c/row/task 为当前责任范围。
// 返回值：排除申请人、当前受理人和操作者的精确候选集合。
func (s *Service) taskPersonnelCandidateIDs(ctx context.Context, db *gorm.DB, a domain.Actor, purpose string, c domain.Claim, row store.Task, task domain.Task) (map[string]bool, error) {
	rows, err := configs(db, a.TenantID, "assignments", true)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	checked := map[string]bool{}
	day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	for _, config := range rows {
		var assignment domain.Assignment
		if err = json.Unmarshal([]byte(config.ContentJSON), &assignment); err != nil {
			return nil, err
		}
		if assignment.OrgID != c.OrgID || (assignment.Role != task.NodeRole && assignment.Role != task.NodeCode) ||
			day < assignment.ValidFrom || (assignment.ValidUntil != "" && day > assignment.ValidUntil) {
			continue
		}
		if checked[assignment.SubjectID] {
			continue
		}
		checked[assignment.SubjectID] = true
		if len(checked) > 100 {
			return nil, domain.Fail("OPTIONS_LIMIT", "任务候选超过受控上限，请管理员整理映射", 422)
		}
		if _, err = taskTransferAssignments(db, c, task, assignment.SubjectID); err != nil {
			var failure *domain.Problem
			if errors.As(err, &failure) && failure.Code == "TRANSFER_TARGET_INVALID" {
				continue
			}
			return nil, err
		}
		ids[assignment.SubjectID] = true
	}
	if purpose == "recovery" && isProjectRole(task.NodeRole) {
		if err = s.addCurrentPersonnelAuthority(ctx, a, c, task, ids); err != nil {
			return nil, err
		}
	}
	delete(ids, "")
	delete(ids, c.Applicant)
	delete(ids, row.Assignee)
	delete(ids, a.SubjectID)
	return ids, nil
}

// addCurrentPersonnelAuthority 为恢复选择增加覆盖全部当前任务分摊的真实项目岗位。
// 参数：ctx/a 为调用身份；c/task 为责任范围；ids 为已限定候选集合。
// 返回值：项目来源、范围或岗位事实错误；不将目录故障降级为本地映射成功。
func (s *Service) addCurrentPersonnelAuthority(ctx context.Context, a domain.Actor, c domain.Claim, task domain.Task, ids map[string]bool) error {
	if s.deps.Projects == nil {
		return domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "当前项目岗位服务未配置", 503)
	}
	refs, err := s.deps.Projects.Validate(ctx, domain.Actor{TenantID: c.TenantID, SubjectID: c.Applicant}, c.ID, c.Version, claimReferences(c))
	if err != nil {
		return err
	}
	keys, err := currentTaskRoleKeys(c, refs, task)
	if err != nil {
		var failure *domain.Problem
		if !errors.As(err, &failure) || failure.Code != "APPROVER_MISSING" {
			return err
		}
	}
	for subject, coverage := range keys {
		covered := map[string]bool{}
		for _, key := range coverage {
			covered[key] = true
		}
		complete := len(task.AllocationKeys) > 0
		for _, key := range task.AllocationKeys {
			complete = complete && covered[key]
		}
		if complete {
			ids[subject] = true
		}
	}
	return nil
}
