package application

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"time"
)

type businessActionProof struct {
	TaskID       string    `json:"task_id"`
	TaskVersion  int64     `json:"task_version"`
	ClaimVersion int64     `json:"claim_version"`
	Subject      string    `json:"subject_id"`
	Role         string    `json:"node_role"`
	KeysHash     string    `json:"allocation_keys_hash"`
	SourceHash   string    `json:"current_source_hash,omitempty"`
	VerifiedAt   time.Time `json:"verified_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Substitute   bool      `json:"controlled_substitute"`
}

// preflightBusinessAction 事务外复核当前业务岗位，转交或加签替代人仍核验当前映射。
// 参数：ctx 为取消边界，db 为数据库，cc 为当前命令，input 为已绑定版本动作。
// 返回值：岗位失效、依赖故障或任务冲突错误。
func (s *Service) preflightBusinessAction(ctx context.Context, db *gorm.DB, cc *commandContext, input ApprovalInput) error {
	if input.Action != "APPROVE" && input.Action != "RETURN" {
		return nil
	}
	var row store.Task
	if e := db.Where("tenant_id = ? AND id = ? AND assignee = ?", cc.actor.TenantID, cc.resourceID, cc.actor.SubjectID).First(&row).Error; e != nil {
		return domain.Fail("RESOURCE_NOT_FOUND", "当前审批任务不存在", 404)
	}
	var task domain.Task
	if e := json.Unmarshal([]byte(row.ContentJSON), &task); e != nil {
		return e
	}
	if !isProjectRole(task.NodeRole) {
		return nil
	}
	c, _, e := loadClaim(db, cc.actor.TenantID, row.ClaimID, false)
	if e != nil {
		return e
	}
	if row.Version != input.TaskVersion || c.Version != input.ExpectedVersion || row.Round != input.Round || c.CurrentRound != row.Round || row.Status != "PENDING" || c.Status != "IN_APPROVAL" {
		return domain.Fail("STALE_APPROVAL_TASK", "任务版本或状态已改变", 409)
	}
	if s.deps.Personnel == nil {
		return domain.Fail("PERSONNEL_DIRECTORY_UNAVAILABLE", "当前人员目录未配置", 503)
	}
	if e = s.deps.Personnel.VerifyApprover(ctx, cc.actor, row.Assignee, task.RequiredPermission); e != nil {
		return e
	}
	substitute, e := hasTransferAuthority(db, row)
	if e != nil {
		return e
	}
	sourceHash := ""
	expiresAt := time.Now().UTC().Add(time.Minute)
	if substitute {
		if _, e = taskTransferAssignments(db, c, task, row.Assignee); e != nil {
			return e
		}
	} else {
		if s.deps.Projects == nil {
			return domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "当前项目岗位服务未配置", 503)
		}
		refs, err := s.deps.Projects.Validate(ctx, domain.Actor{TenantID: c.TenantID, SubjectID: c.Applicant}, c.ID, c.Version, claimReferences(c))
		if err != nil {
			return err
		}
		if _, err = referenceShape(c, refs); err != nil {
			return err
		}
		sourceHash, err = digest(refs)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if ref.ValidUntil.Before(expiresAt) {
				expiresAt = ref.ValidUntil
			}
		}
		keys, err := currentTaskRoleKeys(c, refs, task)
		if err != nil {
			return err
		}
		allowed := map[string]bool{}
		for _, key := range keys[row.Assignee] {
			allowed[key] = true
		}
		for _, key := range task.AllocationKeys {
			if !allowed[key] {
				return domain.Fail("APPROVER_AUTHORITY_CHANGED", "当前项目岗位已变更，请受控转交或重开重审", 422)
			}
		}
	}
	hash, e := digest(task.AllocationKeys)
	if e != nil {
		return e
	}
	cc.business = &businessActionProof{TaskID: row.ID, TaskVersion: row.Version, ClaimVersion: c.Version, Subject: row.Assignee, Role: task.NodeRole, KeysHash: hash, SourceHash: sourceHash, VerifiedAt: time.Now().UTC(), ExpiresAt: expiresAt, Substitute: substitute}
	return nil
}

// isProjectRole 识别必须复核项目权威来源的业务岗位。
// 参数：role 为冻结任务岗位。
// 返回值：是否为项目业务岗位。
func isProjectRole(role string) bool {
	return role == "PROJECT_EXPENSE_OWNER" || role == "TECHNICAL_DIRECTOR" || role == "SALES_DIRECTOR"
}

// hasTransferAuthority 区分真实转交与加签来源，不能只信任可变受理人字段。
// 参数：db 为数据库，task 为当前锁前任务。
// 返回值：是否有真实受控替代来源；转交或加签链断裂时拒绝。
func hasTransferAuthority(db *gorm.DB, task store.Task) (bool, error) {
	var rows []store.ActionDetail
	if e := db.Where("tenant_id = ? AND task_id = ?", task.TenantID, task.ID).Where("action_id IN (?)", db.Model(&store.Action{}).Select("id").Where("tenant_id = ? AND task_id = ? AND claim_id = ? AND action = ?", task.TenantID, task.ID, task.ClaimID, "TRANSFER")).Limit(1001).Find(&rows).Error; e != nil {
		return false, e
	}
	if len(rows) > 1000 {
		return false, domain.Fail("HISTORY_LIMIT", "转交历史超过受控上限", 422)
	}
	var latest *store.ActionDetail
	var version int64
	for i := range rows {
		var data struct {
			Version int64 `json:"task_version_after"`
		}
		if e := json.Unmarshal([]byte(rows[i].ContentJSON), &data); e != nil {
			return false, e
		}
		if data.Version > version {
			latest = &rows[i]
			version = data.Version
		}
	}
	if latest != nil {
		if latest.ToSubjectID != task.Assignee || version > task.Version {
			return false, domain.Fail("APPROVER_AUTHORITY_CHANGED", "当前受理人与转交历史不一致", 409)
		}
		var recovery struct {
			Mode string `json:"recovery_target_mode"`
		}
		if e := json.Unmarshal([]byte(latest.ContentJSON), &recovery); e != nil {
			return false, e
		}
		// 权威岗位接管仍逐次核查当前项目职责，不强制伪造本地替代映射。
		if recovery.Mode == "CURRENT_AUTHORITY" {
			return false, nil
		}
		return true, nil
	}
	var dto domain.Task
	if e := json.Unmarshal([]byte(task.ContentJSON), &dto); e != nil {
		return false, e
	}
	if dto.Assignee != task.Assignee {
		return false, domain.Fail("APPROVER_AUTHORITY_CHANGED", "当前受理人缺少转交依据", 409)
	}
	return hasAddsignAuthority(db, task, dto)
}

// verifyBusinessActionProof 在锁定版本后验证事务外岗位证明未过期，替代映射再次本地复核。
// 参数：tx 为事务，cc 为命令，row/task/c 为重新锁定状态。
// 返回值：岗位证明或本地替代映射失效错误。
func verifyBusinessActionProof(tx *gorm.DB, cc *commandContext, row store.Task, task domain.Task, c domain.Claim) error {
	if !isProjectRole(task.NodeRole) {
		return nil
	}
	p := cc.business
	hash, e := digest(task.AllocationKeys)
	if e != nil {
		return e
	}
	if p == nil || p.TaskID != row.ID || p.TaskVersion != row.Version || p.ClaimVersion != c.Version || p.Subject != row.Assignee || p.Role != task.NodeRole || p.KeysHash != hash || time.Since(p.VerifiedAt) < 0 || time.Since(p.VerifiedAt) > time.Minute || !p.ExpiresAt.After(time.Now().UTC()) {
		return domain.Fail("APPROVER_QUALIFICATION_STALE", "当前岗位证明缺失或过期", 409)
	}
	if p.Substitute {
		_, e = taskTransferAssignments(tx, c, task, row.Assignee)
	}
	return e
}
