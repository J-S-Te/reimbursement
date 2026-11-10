package application

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"time"
)

// preflightTransfer 在事务外核实转交或加签的当前资格并绑定任务版本。
// 参数：ctx 为上下文，db 为数据库，cc 为可信命令，payload 为动作请求。
// 返回值：无效任务、目标或人员目录不可用错误。
func (s *Service) preflightTransfer(ctx context.Context, db *gorm.DB, cc *commandContext, payload json.RawMessage) error {
	var input ApprovalInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return e
	}
	if input.Action != "TRANSFER" && input.Action != "ADDSIGN" {
		if e := s.preflightBusinessAction(ctx, db, cc, input); e != nil {
			return e
		}
		return s.preflightFinanceRecheck(ctx, db, cc, input)
	}
	if e := require(cc.actor, "expense.approval.transfer"); e != nil {
		return e
	}
	if input.Action == "ADDSIGN" {
		current := input
		current.Action = "APPROVE"
		if e := s.preflightBusinessAction(ctx, db, cc, current); e != nil {
			return e
		}
	}
	var task store.Task
	if e := db.Where("tenant_id = ? AND id = ? AND assignee = ?", cc.actor.TenantID, cc.resourceID, cc.actor.SubjectID).First(&task).Error; e != nil {
		return domain.Fail("RESOURCE_NOT_FOUND", "当前审批任务不存在或无权转交、加签", 404)
	}
	c, _, e := loadClaim(db, cc.actor.TenantID, task.ClaimID, false)
	if e != nil {
		return e
	}
	if e = verifyVersion(task.Version, input.TaskVersion); e != nil {
		return e
	}
	if e = verifyVersion(c.Version, input.ExpectedVersion); e != nil {
		return e
	}
	if task.Status != "PENDING" || c.Status != "IN_APPROVAL" || task.Round != input.Round || task.Round != c.CurrentRound {
		return domain.Fail("STALE_APPROVAL_TASK", "仅当前有效待办可以转交或加签", 409)
	}
	if input.TargetSubjectID == "" || input.TargetSubjectID == c.Applicant || input.TargetSubjectID == cc.actor.SubjectID || input.Reason == "" {
		if input.Action == "ADDSIGN" {
			return domain.Fail("INVALID_ADDSIGN", "加签必须有原因和独立目标受理人", 422)
		}
		return domain.Fail("INVALID_TRANSFER", "转交必须有原因和独立目标受理人", 422)
	}
	var frozen domain.Task
	if e = json.Unmarshal([]byte(task.ContentJSON), &frozen); e != nil {
		return e
	}
	if e = require(cc.actor, frozen.RequiredPermission); e != nil {
		return e
	}
	if _, e = taskTransferAssignments(db, c, frozen, input.TargetSubjectID); e != nil {
		return e
	}
	if s.deps.Personnel == nil {
		return domain.Fail("PERSONNEL_DIRECTORY_UNAVAILABLE", "人员当前任职与审批能力目录未配置", 503)
	}
	if e = s.deps.Personnel.VerifyApprover(ctx, cc.actor, input.TargetSubjectID, frozen.RequiredPermission); e != nil {
		return e
	}
	if frozen.NodeCode == "ACCOUNTANT" || frozen.NodeCode == "FINANCE_DIRECTOR" {
		cc.transferTenantAll, e = s.verifyTargetFinanceScope(ctx, db, cc.actor, input.TargetSubjectID, c.OrgID)
		if e != nil {
			return e
		}
	}
	cc.transfer = &domain.TransferQualification{TargetSubjectID: input.TargetSubjectID, RequiredPermission: frozen.RequiredPermission, VerifiedAt: time.Now().UTC(), TaskID: task.ID, TaskVersion: task.Version, ClaimVersion: c.Version, Round: c.CurrentRound, Source: "personnel.verify_approver", TenantAll: cc.transferTenantAll}
	return nil
}

// transferAssignments 读取目标当前有效本地映射，目录能力不能扩大节点组织和分摊范围。
// 参数：tx 为数据库，c 为单据，role 为固定节点，target 为目标主体。
// 返回值：当前本地配置证据或拒绝。
func transferAssignments(tx *gorm.DB, c domain.Claim, role, target string) ([]domain.Assignment, error) {
	rows, e := configs(tx, c.TenantID, "assignments", true)
	if e != nil {
		return nil, e
	}
	day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	evidence := []domain.Assignment{}
	for _, row := range rows {
		var a domain.Assignment
		if e = json.Unmarshal([]byte(row.ContentJSON), &a); e != nil {
			return nil, e
		}
		if a.SubjectID == target && a.OrgID == c.OrgID && a.Role == role && a.ProjectID == "" && day >= a.ValidFrom && (a.ValidUntil == "" || day <= a.ValidUntil) {
			evidence = append(evidence, a)
		}
	}
	if len(evidence) == 0 {
		return nil, domain.Fail("TRANSFER_TARGET_INVALID", "目标不在当前组织节点的有效替代负责人映射中", 422)
	}
	return evidence, nil
}

// verifyTransferEvidence 检查事务外目录证明与当前任务完全一致且未过期。
// 参数：cc 为命令，task/dto/c 为重新锁定的当前状态，target 为请求目标。
// 返回值：证据失效时拒绝，要求重新发起新命令校验。
func verifyTransferEvidence(cc *commandContext, task store.Task, dto domain.Task, c domain.Claim, target string) error {
	proof := cc.transfer
	if proof == nil || proof.TargetSubjectID != target || proof.RequiredPermission != dto.RequiredPermission || proof.TaskID != task.ID || proof.TaskVersion != task.Version || proof.ClaimVersion != c.Version || proof.Round != c.CurrentRound || time.Since(proof.VerifiedAt) < 0 || time.Since(proof.VerifiedAt) > time.Minute {
		return domain.Fail("APPROVER_QUALIFICATION_STALE", "人员资格证明缺失或已失效，请刷新后重试", 409)
	}
	return nil
}

// approvalHistory 只向原责任链参与人或完整授权审计人员展示当前任务的不可变历史。
// 参数：db 为数据库，a 为当前身份，taskID 为任务标识。
// 返回值：结构化逐跳历史；旧版未保存的证据明确标记为遗留缺失。
func (s *Service) approvalHistory(db *gorm.DB, a domain.Actor, taskID string) (any, error) {
	if !a.Has("expense.claim.read") || (!a.Has("expense.approval.act") && !a.Has("expense.audit.read") && !a.Has("expense.approval.recover")) {
		return nil, domain.Fail("FORBIDDEN", "无审批历史读取权限", 403)
	}
	var task store.Task
	if e := db.Where("tenant_id = ? AND id = ?", a.TenantID, taskID).First(&task).Error; e != nil {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "审批任务不存在", 404)
	}
	recoveryAllowed := false
	if a.Has("expense.approval.recover") {
		c, _, e := loadClaim(db, a.TenantID, task.ClaimID, false)
		if e != nil {
			return nil, e
		}
		e = requireApprovalRecoveryScope(db, a, c)
		recoveryAllowed = e == nil
		if e != nil && !a.Has("expense.approval.act") && !a.Has("expense.audit.read") {
			return nil, e
		}
	}
	allowed := recoveryAllowed || task.Assignee == a.SubjectID
	var count int64
	if e := db.Model(&store.ActionDetail{}).Where("tenant_id = ? AND task_id = ? AND (from_subject_id = ? OR to_subject_id = ?)", a.TenantID, taskID, a.SubjectID, a.SubjectID).Count(&count).Error; e != nil {
		return nil, e
	}
	allowed = allowed || count > 0
	if !allowed {
		if e := db.Model(&store.Action{}).Where("tenant_id = ? AND task_id = ? AND actor = ?", a.TenantID, taskID, a.SubjectID).Count(&count).Error; e != nil {
			return nil, e
		}
		allowed = count > 0
	}
	if !allowed && a.Has("expense.audit.read") {
		c, _, e := loadClaim(db, a.TenantID, task.ClaimID, false)
		if e != nil {
			return nil, e
		}
		allowed, e = s.fullRead(db, a, c)
		if e != nil {
			return nil, e
		}
	}
	if !allowed {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "没有此任务历史的访问范围", 404)
	}
	var actions []store.Action
	if e := db.Where("tenant_id = ? AND task_id = ?", a.TenantID, taskID).Order("created_at,id").Limit(1001).Find(&actions).Error; e != nil {
		return nil, e
	}
	if len(actions) > 1000 {
		return nil, domain.Fail("HISTORY_LIMIT", "审批历史超过单页受控上限，请审计导出", 422)
	}
	var details []store.ActionDetail
	if e := db.Where("tenant_id = ? AND task_id = ?", a.TenantID, taskID).Find(&details).Error; e != nil {
		return nil, e
	}
	byAction := map[string]store.ActionDetail{}
	for _, detail := range details {
		byAction[detail.ActionID] = detail
	}
	items := []any{}
	for _, action := range actions {
		view := map[string]any{"id": action.ID, "task_id": taskID, "actor_subject_id": action.Actor, "action": action.Action, "reason": action.Reason, "round": action.Round, "created_at": action.CreatedAt}
		if detail, ok := byAction[action.ID]; ok {
			view["from_subject_id"] = detail.FromSubjectID
			view["to_subject_id"] = detail.ToSubjectID
			var proof map[string]any
			if e := json.Unmarshal([]byte(detail.ContentJSON), &proof); e != nil {
				return nil, e
			}
			for key, value := range proof {
				view[key] = value
			}
		} else if action.Action == "TRANSFER" {
			view["qualification_evidence_status"] = "LEGACY_NOT_RECORDED"
		}
		items = append(items, view)
	}
	result := map[string]any{"items": items, "total": len(items)}
	origin, e := addsignHistoryOrigin(db, task)
	if e != nil {
		return nil, e
	}
	if origin != nil {
		result["addsign_origin"] = origin
	}
	return result, nil
}
