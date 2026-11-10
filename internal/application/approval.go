package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
)

type ApprovalInput struct {
	CommandInput
	Round           int    `json:"round"`
	TaskVersion     int64  `json:"task_version"`
	Action          string `json:"action"`
	Reason          string `json:"reason"`
	TargetSubjectID string `json:"target_subject_id"`
}

// approval 将单任务动作、ALL节点推进和单据状态变更原子落库。
// 参数：tx 为事务，cc 为当前受理人命令，payload 为绑定轮次及版本的动作。
// 返回值：最小动作回执、单据标识与错误。
func (s *Service) approval(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input ApprovalInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	var lookup store.Task
	if e := tx.Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&lookup).Error; e != nil {
		return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "任务不存在或无权访问", 404)
	}
	c, _, e := loadClaim(tx, cc.actor.TenantID, lookup.ClaimID, true)
	if e != nil {
		return nil, "", e
	}
	var task store.Task
	if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&task).Error; e != nil {
		return nil, "", e
	}
	var dto domain.Task
	if e = json.Unmarshal([]byte(task.ContentJSON), &dto); e != nil {
		return nil, "", e
	}
	if e = financeTaskIdentity(task, dto); e != nil {
		return nil, "", e
	}
	if task.Assignee != cc.actor.SubjectID || cc.actor.SubjectID == c.Applicant {
		return nil, "", domain.Fail("FORBIDDEN", "只有独立的当前受理人可以办理任务", 403)
	}
	if e = require(cc.actor, dto.RequiredPermission); e != nil {
		return nil, "", e
	}
	if dto.NodeCode == "FINANCE_DIRECTOR" {
		if e = requireFinancial(tx, cc.actor, c, "expense.approval.act"); e != nil {
			return nil, "", e
		}
	}
	if dto.NodeCode == "ACCOUNTANT" {
		if e = requireFinancial(tx, cc.actor, c, "expense.finance.review"); e != nil {
			return nil, "", e
		}
	}
	if e = verifyVersion(c.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if e = verifyVersion(task.Version, input.TaskVersion); e != nil {
		return nil, "", e
	}
	if input.Round != task.Round || task.Round != c.CurrentRound || task.Status != "PENDING" || c.Status != "IN_APPROVAL" {
		return nil, "", domain.Fail("STALE_APPROVAL_TASK", "任务已过期或尚未轮到此节点", 409)
	}
	if input.Action == "APPROVE" || input.Action == "RETURN" || input.Action == "ADDSIGN" {
		if e = verifyBusinessActionProof(tx, cc, task, dto, c); e != nil {
			return nil, "", e
		}
	}
	if input.Action == "ADDSIGN" {
		return s.addApprovalSigner(tx, cc, c, task, dto, input)
	}
	var recheck *financeRecheckResult
	var complianceCodes []string
	if input.Action == "APPROVE" && dto.NodeCode == "ACCOUNTANT" {
		recheck, e = financeRecheck(tx, cc, c, task)
		if e != nil {
			return nil, "", e
		}
		if len(recheck.Issues) > 0 {
			input.Action = "RETURN"
			complianceCodes, input.Reason = recheck.complianceSummary()
		}
	}
	autoReturned := len(complianceCodes) > 0
	previous := c.Version
	previousAssignee := task.Assignee
	var assignmentEvidence []domain.Assignment
	switch input.Action {
	case "APPROVE":
		task.Status = "APPROVED"
	case "RETURN":
		if strings.TrimSpace(input.Reason) == "" {
			return nil, "", domain.Fail("REASON_REQUIRED", "退回需要填写原因", 422)
		}
		task.Status = "RETURNED"
		c.Status = "RETURNED"
	case "TRANSFER":
		if e = require(cc.actor, "expense.approval.transfer"); e != nil {
			return nil, "", e
		}
		if strings.TrimSpace(input.Reason) == "" || input.TargetSubjectID == "" || input.TargetSubjectID == c.Applicant || input.TargetSubjectID == task.Assignee {
			return nil, "", domain.Fail("INVALID_TRANSFER", "转交必须有原因和独立目标受理人", 422)
		}
		if e = verifyTransferEvidence(cc, task, dto, c, input.TargetSubjectID); e != nil {
			return nil, "", e
		}
		assignmentEvidence, e = taskTransferAssignments(tx, c, dto, input.TargetSubjectID)
		if e != nil {
			return nil, "", e
		}
		if dto.NodeCode == "ACCOUNTANT" || dto.NodeCode == "FINANCE_DIRECTOR" {
			targetActor := domain.Actor{TenantID: c.TenantID, SubjectID: input.TargetSubjectID}
			if cc.transfer.TenantAll {
				targetActor.Permissions = []string{"expense.scope.tenant_all"}
			}
			ok, err := financeScope(tx, targetActor, c.OrgID)
			if err != nil {
				return nil, "", err
			}
			if !ok {
				return nil, "", domain.Fail("TRANSFER_TARGET_INVALID", "财务目标当前范围不足", 403)
			}
		}
		task.Assignee = input.TargetSubjectID
	default:
		return nil, "", domain.Fail("INVALID_APPROVAL_ACTION", "仅支持通过、退回、受控转交或加签", 422)
	}
	task.Version++
	if e = tx.Model(&store.Task{}).Where("id = ? AND tenant_id = ? AND version = ?", task.ID, c.TenantID, input.TaskVersion).Updates(map[string]any{"status": task.Status, "assignee": task.Assignee, "version": task.Version}).Error; e != nil {
		return nil, "", e
	}
	id, e := newID()
	if e != nil {
		return nil, "", e
	}
	if e = tx.Create(&store.Action{ID: id, TenantID: c.TenantID, ClaimID: c.ID, TaskID: task.ID, Actor: cc.actor.SubjectID, Action: input.Action, Reason: input.Reason, Round: c.CurrentRound}).Error; e != nil {
		return nil, "", e
	}
	if (input.Action == "APPROVE" || input.Action == "RETURN") && cc.business != nil {
		detailID, err := newID()
		if err != nil {
			return nil, "", err
		}
		content, err := encode(map[string]any{"business_qualification_evidence": cc.business, "round": c.CurrentRound, "node_role": dto.NodeRole, "task_version_before": input.TaskVersion, "task_version_after": task.Version})
		if err != nil {
			return nil, "", err
		}
		if err = tx.Create(&store.ActionDetail{ID: detailID, TenantID: c.TenantID, ActionID: id, TaskID: task.ID, ClaimID: c.ID, FromSubjectID: previousAssignee, ToSubjectID: task.Assignee, Reason: input.Reason, ContentJSON: content}).Error; err != nil {
			return nil, "", err
		}
	}
	if autoReturned {
		if e = appendFinanceReturnEvidence(tx, cc, c, task, id, input.Reason, complianceCodes, recheck); e != nil {
			return nil, "", e
		}
	}
	if input.Action == "TRANSFER" {
		detailID, err := newID()
		if err != nil {
			return nil, "", err
		}
		evidence, err := encode(map[string]any{"qualification_evidence": cc.transfer, "assignment_evidence": assignmentEvidence, "round": c.CurrentRound, "node_code": task.NodeCode, "allocation_keys": dto.AllocationKeys, "task_version_before": input.TaskVersion, "task_version_after": task.Version})
		if err != nil {
			return nil, "", err
		}
		if err = tx.Create(&store.ActionDetail{ID: detailID, TenantID: c.TenantID, ActionID: id, TaskID: task.ID, ClaimID: c.ID, FromSubjectID: previousAssignee, ToSubjectID: task.Assignee, Reason: input.Reason, ContentJSON: evidence}).Error; err != nil {
			return nil, "", err
		}
		if err = appendOutbox(tx, c.TenantID, "audit", c.ID, map[string]any{"actor_subject_id": cc.actor.SubjectID, "request_id": cc.actor.RequestID, "action": "approval.transfer", "resource_id": c.ID, "command_id": cc.input.CommandID, "action_id": id, "from_subject_id": previousAssignee, "to_subject_id": task.Assignee, "qualification_evidence": cc.transfer}); err != nil {
			return nil, "", err
		}
	}
	if input.Action == "RETURN" {
		if e = cancelTasks(tx, c); e != nil {
			return nil, "", e
		}
		if autoReturned {
			if e = releaseInvoices(tx, c); e != nil {
				return nil, "", e
			}
		}

	} else if input.Action == "APPROVE" {
		if e = advanceNode(tx, &c, task.Node); e != nil {
			return nil, "", e
		}
	} else {
		if e = appendOutbox(tx, c.TenantID, "notification", c.ID, map[string]any{"task_id": task.ID, "assignee_subject_id": task.Assignee, "round": c.CurrentRound, "claim_id": c.ID}); e != nil {
			return nil, "", e
		}
	}
	c.Version++
	if e = saveClaim(tx, &c, previous); e != nil {
		return nil, "", e
	}
	response := map[string]any{"task_id": task.ID, "task_version": task.Version, "claim_id": c.ID, "claim_version": c.Version, "round": c.CurrentRound, "org_id": c.OrgID, "status": c.Status, "action": input.Action, "from_subject_id": previousAssignee, "to_subject_id": task.Assignee}
	if autoReturned {
		response["auto_returned"] = true
		response["actual_action"] = input.Action
		response["compliance_codes"] = complianceCodes
		response["reason"] = input.Reason
	}
	return response, c.ID, nil
}

// advanceNode 仅在当前节点所有必办任务通过后激活下一节点。
// 参数：tx 为事务，c 为单据，node 为刚处理的节点序号。
// 返回值：账本读取或通知写入错误。
func advanceNode(tx *gorm.DB, c *domain.Claim, node int) error {
	var pending int64
	if e := tx.Model(&store.Task{}).Where("tenant_id = ? AND claim_id = ? AND round = ? AND node = ? AND status <> ?", c.TenantID, c.ID, c.CurrentRound, node, "APPROVED").Count(&pending).Error; e != nil {
		return e
	}
	if pending > 0 {
		return nil
	}
	var next []store.Task
	if e := tx.Where("tenant_id = ? AND claim_id = ? AND round = ? AND node = ? AND status = ?", c.TenantID, c.ID, c.CurrentRound, node+1, "WAITING").Find(&next).Error; e != nil {
		return e
	}
	if len(next) == 0 {
		c.Status = "APPROVED_PENDING_PAYMENT"
		return nil
	}
	for _, task := range next {
		if e := tx.Model(&store.Task{}).Where("id = ? AND status = ?", task.ID, "WAITING").Updates(map[string]any{"status": "PENDING", "version": gorm.Expr("version + 1")}).Error; e != nil {
			return e
		}
		if e := appendOutbox(tx, c.TenantID, "notification", c.ID, map[string]any{"task_id": task.ID, "assignee_subject_id": task.Assignee, "round": c.CurrentRound, "claim_id": c.ID}); e != nil {
			return e
		}
	}
	return nil
}
