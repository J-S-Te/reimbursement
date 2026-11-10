package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type approvalRecoveryProof struct {
	TaskID             string    `json:"task_id"`
	OriginalSubject    string    `json:"original_subject_id"`
	TargetSubject      string    `json:"target_subject_id"`
	TaskVersion        int64     `json:"task_version"`
	ClaimVersion       int64     `json:"claim_version"`
	Round              int       `json:"round"`
	RequiredPermission string    `json:"required_permission"`
	FrozenHash         string    `json:"frozen_task_hash"`
	LossCode           string    `json:"original_qualification_loss"`
	TargetMode         string    `json:"target_mode"`
	SourceHash         string    `json:"current_source_hash,omitempty"`
	TenantAll          bool      `json:"tenant_all"`
	VerifiedAt         time.Time `json:"verified_at"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// requireApprovalRecoveryScope 同时验证独立恢复能力、明确组织范围和申请人职责分离。
// 参数：db 为数据库，a 为当前可信身份，c 为租户内待恢复单据。
// 返回值：能力、范围或自审边界不满足时拒绝。
func requireApprovalRecoveryScope(db *gorm.DB, a domain.Actor, c domain.Claim) error {
	if e := require(a, "expense.claim.read"); e != nil {
		return e
	}
	if a.SubjectID == c.Applicant {
		return domain.Fail("FORBIDDEN", "申请人不能接管本人报销审批", 403)
	}
	return requireFinancial(db, a, c, "expense.approval.recover")
}

// loadApprovalRecovery 读取当前待办并绑定单据、轮次、双版本及恢复范围。
// 参数：db 为锁前或事务数据库，cc 为恢复操作者，input 为明确的恢复目标。
// 返回值：当前单据、任务和冻结任务内容；失效时拒绝。
func loadApprovalRecovery(db *gorm.DB, cc *commandContext, input ApprovalInput) (domain.Claim, store.Task, domain.Task, error) {
	var row store.Task
	var frozen domain.Task
	var c domain.Claim
	if e := db.Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&row).Error; e != nil {
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return c, row, frozen, domain.Fail("RESOURCE_NOT_FOUND", "当前审批任务不存在", 404)
		}
		return c, row, frozen, e
	}
	c, _, e := loadClaim(db, cc.actor.TenantID, row.ClaimID, false)
	if e == nil {
		e = requireApprovalRecoveryScope(db, cc.actor, c)
	}
	if e == nil {
		e = verifyVersion(c.Version, input.ExpectedVersion)
	}
	if e == nil {
		e = verifyVersion(row.Version, input.TaskVersion)
	}
	if e != nil {
		return c, row, frozen, e
	}
	if input.Round != row.Round || row.Round != c.CurrentRound || row.Status != "PENDING" || c.Status != "IN_APPROVAL" {
		return c, row, frozen, domain.Fail("STALE_APPROVAL_TASK", "仅当前轮次有效待办可以恢复", 409)
	}
	if (input.Action != "" && input.Action != "RECOVER") || strings.TrimSpace(input.Reason) == "" || len(input.Reason)+len(cc.actor.SubjectID)+len(row.Assignee) > 1800 || input.TargetSubjectID == "" || len(input.TargetSubjectID) > 128 || input.TargetSubjectID != strings.TrimSpace(input.TargetSubjectID) || input.TargetSubjectID == row.Assignee || input.TargetSubjectID == c.Applicant || input.TargetSubjectID == cc.actor.SubjectID {
		return c, row, frozen, domain.Fail("INVALID_APPROVAL_RECOVERY", "恢复须填写原因并指定独立的不同受理人", 422)
	}
	e = json.Unmarshal([]byte(row.ContentJSON), &frozen)
	return c, row, frozen, e
}

// preflightApprovalRecovery 仅在原岗位明确失效后核实目标当前能力和完整责任范围。
// 参数：ctx 为远端取消边界，db 为数据库，cc 为独立恢复命令，payload 为版本绑定请求。
// 返回值：不需恢复、资格不明、依赖故障或版本错误时拒绝。
func (s *Service) preflightApprovalRecovery(ctx context.Context, db *gorm.DB, cc *commandContext, payload json.RawMessage) error {
	var input ApprovalInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return e
	}
	c, row, task, e := loadApprovalRecovery(db, cc, input)
	if e != nil {
		return e
	}
	if s.deps.Personnel == nil {
		return domain.Fail("PERSONNEL_DIRECTORY_UNAVAILABLE", "当前人员目录未配置", 503)
	}
	loss, e := s.approvalRecoveryLoss(ctx, db, cc, input, c, row, task)
	if e != nil {
		return e
	}
	if e = s.deps.Personnel.VerifyApprover(ctx, cc.actor, input.TargetSubjectID, task.RequiredPermission); e != nil {
		return e
	}
	proof := &approvalRecoveryProof{TaskID: row.ID, OriginalSubject: row.Assignee, TargetSubject: input.TargetSubjectID, TaskVersion: row.Version, ClaimVersion: c.Version, Round: c.CurrentRound, RequiredPermission: task.RequiredPermission, LossCode: loss, VerifiedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute)}
	proof.FrozenHash, e = digest(task)
	if e != nil {
		return e
	}
	if e = s.approvalRecoveryTarget(ctx, db, cc.actor, c, task, proof); e != nil {
		return e
	}
	if task.NodeCode == "ACCOUNTANT" || task.NodeCode == "FINANCE_DIRECTOR" {
		proof.TenantAll, e = s.verifyTargetFinanceScope(ctx, db, cc.actor, input.TargetSubjectID, c.OrgID)
		if e != nil {
			return e
		}
	}
	cc.recovery = proof
	return nil
}

// approvalRecoveryLoss 只接受明确的当前失职事实，不把网络或目录配置故障当作撤权。
// 参数：ctx/db 为调用边界，cc/input 为命令，c/row/task 为当前冻结责任。
// 返回值：明确失效代码；原人仍有效或事实不明时拒绝。
func (s *Service) approvalRecoveryLoss(ctx context.Context, db *gorm.DB, cc *commandContext, input ApprovalInput, c domain.Claim, row store.Task, task domain.Task) (string, error) {
	if _, e := hasTransferAuthority(db, row); e != nil {
		return "", e
	}
	e := s.deps.Personnel.VerifyApprover(ctx, cc.actor, row.Assignee, task.RequiredPermission)
	if e == nil && isProjectRole(task.NodeRole) {
		original := *cc
		original.actor.SubjectID = row.Assignee
		input.Action = "APPROVE"
		e = s.preflightBusinessAction(ctx, db, &original, input)
	}
	if e == nil && (task.NodeCode == "ACCOUNTANT" || task.NodeCode == "FINANCE_DIRECTOR") {
		_, e = s.verifyTargetFinanceScope(ctx, db, cc.actor, row.Assignee, c.OrgID)
	}
	if e == nil {
		return "", domain.Fail("APPROVAL_RECOVERY_NOT_REQUIRED", "原受理人仍具备当前审批资格，应由其正常办理或转交", 409)
	}
	var failure *domain.Problem
	if errors.As(e, &failure) && failure.Status == 422 {
		switch failure.Code {
		case "APPROVER_INELIGIBLE", "APPROVER_AUTHORITY_CHANGED", "APPROVER_MISSING", "TRANSFER_TARGET_INVALID", "APPROVER_FINANCE_SCOPE_MISSING":
			return failure.Code, nil
		}
	}
	return "", e
}

// approvalRecoveryTarget 优先核实当前权威岗位，否则要求当前精确替代映射。
// 参数：ctx/db 为调用边界，actor 为操作者，c/task 为冻结责任，proof 接收资格摘要。
// 返回值：缺少权威岗位和有效替代映射，或远端事实不完整时拒绝。
func (s *Service) approvalRecoveryTarget(ctx context.Context, db *gorm.DB, actor domain.Actor, c domain.Claim, task domain.Task, proof *approvalRecoveryProof) error {
	if isProjectRole(task.NodeRole) {
		if s.deps.Projects == nil {
			return domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "当前项目岗位服务未配置", 503)
		}
		refs, e := s.deps.Projects.Validate(ctx, domain.Actor{TenantID: c.TenantID, SubjectID: c.Applicant}, c.ID, c.Version, claimReferences(c))
		if e != nil {
			return e
		}
		keys, e := currentTaskRoleKeys(c, refs, task)
		var failure *domain.Problem
		if e != nil && (!errors.As(e, &failure) || failure.Code != "APPROVER_MISSING" || failure.Status != 422) {
			return e
		}
		covered := map[string]bool{}
		for _, key := range keys[proof.TargetSubject] {
			covered[key] = true
		}
		all := len(task.AllocationKeys) > 0
		for _, key := range task.AllocationKeys {
			all = all && covered[key]
		}
		if all {
			proof.TargetMode = "CURRENT_AUTHORITY"
			proof.SourceHash, e = digest(refs)
			for _, ref := range refs {
				if ref.ValidUntil.Before(proof.ExpiresAt) {
					proof.ExpiresAt = ref.ValidUntil
				}
			}
			return e
		}
	}
	if _, e := taskTransferAssignments(db, c, task, proof.TargetSubject); e != nil {
		return e
	}
	proof.TargetMode = "CONTROLLED_SUBSTITUTE"
	return nil
}

// recoverApproval 在锁定原单据和任务后仅更换当前受理人，保留冻结内容及真实转交历史。
// 参数：tx 为命令事务，cc 为绑定证明的独立恢复命令，payload 为原始恢复请求。
// 返回值：版本化恢复回执、单据标识及事务错误。
func (s *Service) recoverApproval(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input ApprovalInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	var lookup store.Task
	if e := tx.Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&lookup).Error; e != nil {
		return nil, "", e
	}
	if _, _, e := loadClaim(tx, cc.actor.TenantID, lookup.ClaimID, true); e != nil {
		return nil, "", e
	}
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&lookup).Error; e != nil {
		return nil, "", e
	}
	c, row, task, e := loadApprovalRecovery(tx, cc, input)
	if e != nil {
		return nil, "", e
	}
	assignments, e := verifyApprovalRecoveryProof(tx, cc, c, row, task, input.TargetSubjectID)
	if e != nil {
		return nil, "", e
	}
	updated := tx.Model(&store.Task{}).Where("tenant_id = ? AND id = ? AND version = ? AND assignee = ? AND status = ?", c.TenantID, row.ID, row.Version, row.Assignee, "PENDING").Updates(map[string]any{"assignee": input.TargetSubjectID, "version": row.Version + 1})
	if updated.Error != nil {
		return nil, "", updated.Error
	}
	if updated.RowsAffected != 1 {
		return nil, "", domain.Fail("VERSION_CONFLICT", "任务已改变，请刷新后恢复", 409)
	}
	if e = appendApprovalRecoveryHistory(tx, cc, c, row, task, input, assignments); e != nil {
		return nil, "", e
	}
	previous := c.Version
	c.Version++
	if e = saveClaim(tx, &c, previous); e != nil {
		return nil, "", e
	}
	if e = appendOutbox(tx, c.TenantID, "notification", c.ID, map[string]any{"task_id": row.ID, "assignee_subject_id": input.TargetSubjectID, "round": c.CurrentRound, "claim_id": c.ID}); e != nil {
		return nil, "", e
	}
	return map[string]any{"task_id": row.ID, "task_version": row.Version + 1, "claim_id": c.ID, "claim_version": c.Version, "round": c.CurrentRound, "org_id": c.OrgID, "status": c.Status, "action": "RECOVER", "from_subject_id": row.Assignee, "to_subject_id": input.TargetSubjectID}, c.ID, nil
}

// verifyApprovalRecoveryProof 事务内复核版本绑定和本地替代映射，目录证明不能长期复用。
// 参数：tx 为事务，cc 为命令，c/row/task 为锁定状态，target 为请求目标。
// 返回值：当前替代证据；证明缺失、范围变更或过期时拒绝。
func verifyApprovalRecoveryProof(tx *gorm.DB, cc *commandContext, c domain.Claim, row store.Task, task domain.Task, target string) ([]domain.Assignment, error) {
	p := cc.recovery
	hash, e := digest(task)
	if e != nil {
		return nil, e
	}
	if p == nil || p.TaskID != row.ID || p.TaskVersion != row.Version || p.ClaimVersion != c.Version || p.Round != c.CurrentRound || p.OriginalSubject != row.Assignee || p.TargetSubject != target || p.RequiredPermission != task.RequiredPermission || p.FrozenHash != hash || p.LossCode == "" || time.Since(p.VerifiedAt) < 0 || time.Since(p.VerifiedAt) > time.Minute || !p.ExpiresAt.After(time.Now().UTC()) {
		return nil, domain.Fail("APPROVER_QUALIFICATION_STALE", "恢复资格证明缺失或已失效，请刷新后重试", 409)
	}
	if task.NodeCode == "ACCOUNTANT" || task.NodeCode == "FINANCE_DIRECTOR" {
		a := domain.Actor{TenantID: c.TenantID, SubjectID: target}
		if p.TenantAll {
			a.Permissions = []string{"expense.scope.tenant_all"}
		}
		if ok, err := financeScope(tx, a, c.OrgID); err != nil || !ok {
			if err != nil {
				return nil, err
			}
			return nil, domain.Fail("APPROVER_FINANCE_SCOPE_MISSING", "恢复目标缺少当前财务范围", 422)
		}
	}
	if p.TargetMode == "CURRENT_AUTHORITY" && p.SourceHash != "" && isProjectRole(task.NodeRole) {
		return nil, nil
	}
	if p.TargetMode != "CONTROLLED_SUBSTITUTE" {
		return nil, domain.Fail("APPROVER_QUALIFICATION_STALE", "恢复目标资格证明不完整", 409)
	}
	return taskTransferAssignments(tx, c, task, target)
}

// appendApprovalRecoveryHistory 写入恢复发起人与原责任人清晰可追溯的不可变TRANSFER。
// 参数：tx 为事务，cc 为命令，c/row/task 为原冻结责任，input 为原因和目标，assignments 为替代证据。
// 返回值：历史、审计或编码失败错误。
func appendApprovalRecoveryHistory(tx *gorm.DB, cc *commandContext, c domain.Claim, row store.Task, task domain.Task, input ApprovalInput, assignments []domain.Assignment) error {
	id, e := newID()
	if e != nil {
		return e
	}
	reason := "恢复发起人:" + cc.actor.SubjectID + "；原受理人:" + row.Assignee + "；" + input.Reason
	if e = tx.Create(&store.Action{ID: id, TenantID: c.TenantID, ClaimID: c.ID, TaskID: row.ID, Actor: cc.actor.SubjectID, Action: "TRANSFER", Reason: reason, Round: c.CurrentRound}).Error; e != nil {
		return e
	}
	detailID, e := newID()
	if e != nil {
		return e
	}
	content, e := encode(map[string]any{"recovery_evidence": cc.recovery, "recovery_actor": cc.actor.SubjectID, "recovery_target_mode": cc.recovery.TargetMode, "assignment_evidence": assignments, "round": c.CurrentRound, "node_code": task.NodeCode, "allocation_keys": task.AllocationKeys, "task_version_before": row.Version, "task_version_after": row.Version + 1})
	if e != nil {
		return e
	}
	if e = tx.Create(&store.ActionDetail{ID: detailID, TenantID: c.TenantID, ActionID: id, TaskID: row.ID, ClaimID: c.ID, FromSubjectID: row.Assignee, ToSubjectID: input.TargetSubjectID, Reason: reason, ContentJSON: content}).Error; e != nil {
		return e
	}
	return appendOutbox(tx, c.TenantID, "audit", c.ID, map[string]any{"actor_subject_id": cc.actor.SubjectID, "request_id": cc.actor.RequestID, "action": "approval.recover", "resource_id": c.ID, "command_id": cc.input.CommandID, "action_id": id, "from_subject_id": row.Assignee, "to_subject_id": input.TargetSubjectID, "recovery_evidence": cc.recovery})
}
