package application

import (
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// appendFinanceReturnEvidence 原子保存自动退回依据和仅申请人可见的可靠通知。
// 参数：tx/cc 为命令事务与身份，c/task 为单据任务，actionID/reason/codes/result 为真实结论。
// 返回值：证据、审计或通知写入失败，交由外层事务回滚全部事实。
func appendFinanceReturnEvidence(tx *gorm.DB, cc *commandContext,
	c domain.Claim, task store.Task, actionID, reason string,
	codes []string, result *financeRecheckResult) error {
	id, err := newID()
	if err != nil {
		return err
	}
	content, err := encode(map[string]any{
		"auto_returned": true, "requested_action": "APPROVE", "actual_action": "RETURN",
		"compliance_codes": codes, "reason": reason, "round": c.CurrentRound,
		"node_code": "ACCOUNTANT", "task_version_before": task.Version - 1,
		"task_version_after": task.Version, "claim_version_before": c.Version,
		"compliance_evidence": result,
	})
	if err != nil {
		return err
	}
	detail := store.ActionDetail{
		ID: id, TenantID: c.TenantID, ClaimID: c.ID, TaskID: task.ID, ActionID: actionID,
		FromSubjectID: task.Assignee, ToSubjectID: task.Assignee,
		Reason: reason, ContentJSON: content,
	}
	if err = tx.Create(&detail).Error; err != nil {
		return err
	}
	audit := map[string]any{
		"action": "approval.finance_auto_return", "actor_subject_id": cc.actor.SubjectID,
		"request_id": cc.actor.RequestID, "command_id": cc.input.CommandID,
		"resource_id": c.ID, "action_id": actionID, "requested_action": "APPROVE",
		"actual_action": "RETURN", "compliance_codes": codes, "reason": reason,
	}
	if err = appendOutbox(tx, c.TenantID, "audit", c.ID, audit); err != nil {
		return err
	}
	return appendOutbox(tx, c.TenantID, "notification", c.ID, map[string]any{
		"kind": "FINANCE_COMPLIANCE_RETURNED", "claim_id": c.ID, "task_id": task.ID,
		"round": c.CurrentRound, "action_id": actionID,
		"applicant_subject_id": c.Applicant, "assignee_subject_id": c.Applicant,
		"reason": reason, "compliance_codes": codes,
	})
}
