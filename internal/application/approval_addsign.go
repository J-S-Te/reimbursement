package application

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type addsignTaskContent struct {
	domain.Task
	ParentTaskID    string `json:"parent_task_id"`
	AddsignActionID string `json:"addsign_action_id"`
}

type addsignOrigin struct {
	ParentTaskID      string                        `json:"parent_task_id"`
	AddedTaskID       string                        `json:"added_task_id"`
	AddedTask         domain.Task                   `json:"added_task"`
	AddedTaskHash     string                        `json:"added_task_hash"`
	Qualification     *domain.TransferQualification `json:"qualification_evidence"`
	Assignments       []domain.Assignment           `json:"assignment_evidence"`
	Round             int                           `json:"round"`
	TaskVersionBefore int64                         `json:"task_version_before"`
	TaskVersionAfter  int64                         `json:"task_version_after"`
}

// validateAddsignTarget 重用转交等强度证明并拒绝同节点已存在的目标。
// 参数：tx为事务，cc为命令，c/task/dto为已锁定任务，input为目标及原因。
// 返回值：当前有效替代映射证据，或权限、范围、重复目标错误。
func validateAddsignTarget(tx *gorm.DB, cc *commandContext, c domain.Claim,
	task store.Task, dto domain.Task, input ApprovalInput) ([]domain.Assignment, error) {
	if err := require(cc.actor, "expense.approval.transfer"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Reason) == "" || utf8.RuneCountInString(input.Reason) > 2000 ||
		input.TargetSubjectID == "" || input.TargetSubjectID == c.Applicant ||
		input.TargetSubjectID == task.Assignee {
		return nil, domain.Fail("INVALID_ADDSIGN", "加签需要2000字符内原因和独立目标受理人", 422)
	}
	if err := verifyTransferEvidence(cc, task, dto, c, input.TargetSubjectID); err != nil {
		return nil, err
	}
	var count int64
	if err := tx.Model(&store.Task{}).Where(
		"tenant_id = ? AND claim_id = ? AND round = ? AND node = ? AND assignee = ?",
		c.TenantID, c.ID, c.CurrentRound, task.Node, input.TargetSubjectID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count != 0 {
		return nil, domain.Fail("ADDSIGN_TARGET_EXISTS", "该受理人已在本轮当前节点，不得重复加签", 409)
	}
	if err := verifyAddsignFinanceScope(tx, cc, c, dto, input.TargetSubjectID); err != nil {
		return nil, err
	}
	return taskTransferAssignments(tx, c, dto, input.TargetSubjectID)
}

// verifyAddsignFinanceScope 在事务内重核财务加签目标的组织或独立全租户范围。
// 参数：tx为事务，cc为新鲜目录证明，c/dto为单据节点，target为目标主体。
// 返回值：范围不足或读取失败时拒绝加签。
func verifyAddsignFinanceScope(tx *gorm.DB, cc *commandContext, c domain.Claim,
	dto domain.Task, target string) error {
	if dto.NodeCode != "ACCOUNTANT" && dto.NodeCode != "FINANCE_DIRECTOR" {
		return nil
	}
	a := domain.Actor{TenantID: c.TenantID, SubjectID: target}
	if cc.transfer.TenantAll {
		a.Permissions = []string{"expense.scope.tenant_all"}
	}
	ok, err := financeScope(tx, a, c.OrgID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.Fail("ADDSIGN_TARGET_INVALID", "财务加签目标当前组织范围不足", 403)
	}
	return nil
}

// createAddsignTask 建立同节点必办任务并在冻结内容中保留真实父动作坐标。
// 参数：tx为事务，c为单据，parent为冻结父任务，input为加签目标，actionID为真实动作。
// 返回值：新任务及内容摘要；创建失败返回错误。
func createAddsignTask(tx *gorm.DB, c domain.Claim, parent domain.Task,
	input ApprovalInput, actionID string) (domain.Task, string, error) {
	added := parent
	var err error
	added.ID, err = newID()
	if err != nil {
		return added, "", err
	}
	added.Assignee, added.Status, added.Version = input.TargetSubjectID, "PENDING", 1
	hash, err := digest(added)
	if err != nil {
		return added, "", err
	}
	raw, err := encode(addsignTaskContent{added, parent.ID, actionID})
	if err != nil {
		return added, "", err
	}
	err = tx.Create(&store.Task{ID: added.ID, TenantID: c.TenantID, ClaimID: c.ID,
		Round: c.CurrentRound, Node: added.Node, NodeCode: added.NodeCode,
		Assignee: added.Assignee, Status: added.Status, Version: 1, ContentJSON: raw}).Error
	return added, hash, err
}

// recordAddsignOrigin 写入真实加签动作与不可变父子任务和当前资格证据。
// 参数：tx为事务，cc为操作者，c/parent为单据任务，added/hash为新任务，input为原因，evidence为映射。
// 返回值：动作或补充证据持久化失败。
func recordAddsignOrigin(tx *gorm.DB, cc *commandContext, c domain.Claim, parent store.Task,
	added domain.Task, hash, actionID string, input ApprovalInput, evidence []domain.Assignment) error {
	if err := tx.Create(&store.Action{ID: actionID, TenantID: c.TenantID, ClaimID: c.ID,
		TaskID: parent.ID, Actor: cc.actor.SubjectID, Action: "ADDSIGN", Reason: input.Reason,
		Round: c.CurrentRound}).Error; err != nil {
		return err
	}
	raw, err := encode(addsignOrigin{parent.ID, added.ID, added, hash, cc.transfer, evidence,
		c.CurrentRound, parent.Version, parent.Version + 1})
	if err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	return tx.Create(&store.ActionDetail{ID: id, TenantID: c.TenantID, ActionID: actionID,
		TaskID: parent.ID, ClaimID: c.ID, FromSubjectID: parent.Assignee,
		ToSubjectID: added.Assignee, Reason: input.Reason, ContentJSON: raw}).Error
}

// addApprovalSigner 追加同节点ALL任务，保留原任务待办及批准内容不变。
// 参数：tx为事务，cc为已复核命令，c/task/dto为锁定状态，input为加签请求。
// 返回值：父子任务与版本回执，或原子回滚错误。
func (s *Service) addApprovalSigner(tx *gorm.DB, cc *commandContext, c domain.Claim,
	task store.Task, dto domain.Task, input ApprovalInput) (any, string, error) {
	evidence, err := validateAddsignTarget(tx, cc, c, task, dto, input)
	if err != nil {
		return nil, "", err
	}
	actionID, err := newID()
	if err != nil {
		return nil, "", err
	}
	added, hash, err := createAddsignTask(tx, c, dto, input, actionID)
	if err != nil {
		return nil, "", err
	}
	result := tx.Model(&store.Task{}).Where("tenant_id = ? AND id = ? AND version = ? AND status = ?",
		c.TenantID, task.ID, task.Version, "PENDING").Update("version", task.Version+1)
	if result.Error != nil {
		return nil, "", result.Error
	}
	if result.RowsAffected != 1 {
		return nil, "", domain.Fail("STALE_APPROVAL_TASK", "加签时任务版本已改变", 409)
	}
	if err = recordAddsignOrigin(tx, cc, c, task, added, hash, actionID, input, evidence); err != nil {
		return nil, "", err
	}
	if err = appendOutbox(tx, c.TenantID, "notification", c.ID, map[string]any{
		"task_id": added.ID, "assignee_subject_id": added.Assignee,
		"round": c.CurrentRound, "claim_id": c.ID}); err != nil {
		return nil, "", err
	}
	previous := c.Version
	c.Version++
	if err = saveClaim(tx, &c, previous); err != nil {
		return nil, "", err
	}
	return map[string]any{"task_id": task.ID, "task_version": task.Version + 1,
		"added_task_id": added.ID, "claim_id": c.ID, "claim_version": c.Version,
		"round": c.CurrentRound, "org_id": c.OrgID, "status": c.Status, "action": "ADDSIGN",
		"from_subject_id": task.Assignee, "to_subject_id": added.Assignee}, c.ID, nil
}

// hasAddsignAuthority 验证真实加签动作的父子绑定，不能把新增受理人冒充转交或原岗位。
// 参数：db为数据库，row为当前任务，dto为冻结任务内容。
// 返回值：是否有不可变加签来源；父子关系或冻结摘要不一致时拒绝。
func hasAddsignAuthority(db *gorm.DB, row store.Task, dto domain.Task) (bool, error) {
	var frozen addsignTaskContent
	if err := json.Unmarshal([]byte(row.ContentJSON), &frozen); err != nil {
		return false, err
	}
	if frozen.ParentTaskID == "" && frozen.AddsignActionID == "" {
		return false, nil
	}
	var action store.Action
	if err := db.Where("tenant_id = ? AND id = ? AND claim_id = ? AND task_id = ? AND round = ? AND action = ?",
		row.TenantID, frozen.AddsignActionID, row.ClaimID, frozen.ParentTaskID, row.Round,
		"ADDSIGN").First(&action).Error; err != nil {
		return false, domain.Fail("APPROVER_AUTHORITY_CHANGED", "加签来源动作缺失或不匹配", 409)
	}
	var detail store.ActionDetail
	if err := db.Where("tenant_id = ? AND action_id = ? AND task_id = ? AND claim_id = ?",
		row.TenantID, action.ID, action.TaskID, row.ClaimID).First(&detail).Error; err != nil {
		return false, domain.Fail("APPROVER_AUTHORITY_CHANGED", "加签不可变证据缺失", 409)
	}
	var origin addsignOrigin
	if err := json.Unmarshal([]byte(detail.ContentJSON), &origin); err != nil {
		return false, err
	}
	hash, err := digest(dto)
	if err != nil {
		return false, err
	}
	originHash, err := digest(origin.AddedTask)
	if err != nil {
		return false, err
	}
	if origin.ParentTaskID != action.TaskID || origin.AddedTaskID != row.ID ||
		originHash != hash ||
		origin.Round != row.Round || origin.AddedTaskHash != hash ||
		detail.ToSubjectID != row.Assignee || dto.Assignee != row.Assignee ||
		detail.FromSubjectID != action.Actor || origin.Qualification == nil ||
		origin.Qualification.TargetSubjectID != row.Assignee ||
		origin.Qualification.TaskID != action.TaskID || origin.Qualification.Round != row.Round ||
		origin.Qualification.RequiredPermission != dto.RequiredPermission {
		return false, domain.Fail("APPROVER_AUTHORITY_CHANGED", "加签任务与冻结父子证据不一致", 409)
	}
	return true, nil
}

// VerifyApprovalReminder 按当前人员、业务岗位或受控替代及财务范围复核催办收件资格。
// 参数：ctx为超时，tenant/taskID为可信任务坐标，taskVersion/claimVersion绑定扫描版本。
// 返回值：资格失效、状态改变或依赖故障时拒绝生成催办，不执行审批动作。
func (s *Service) VerifyApprovalReminder(ctx context.Context, tenant, taskID string,
	taskVersion, claimVersion int64) error {
	started := time.Now().UTC()
	var row store.Task
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenant, taskID).
		First(&row).Error; err != nil {
		return err
	}
	c, stored, err := loadClaim(s.db.WithContext(ctx), tenant, row.ClaimID, false)
	if err != nil {
		return err
	}
	if row.Status != "PENDING" || c.Status != "IN_APPROVAL" || row.Round != c.CurrentRound ||
		row.Version != taskVersion || c.Version != claimVersion || row.Assignee == c.Applicant {
		return domain.Fail("STALE_APPROVAL_TASK", "催办任务状态或版本已改变", 409)
	}
	var task domain.Task
	if err = json.Unmarshal([]byte(row.ContentJSON), &task); err != nil {
		return err
	}
	proof, tenantAll, err := s.verifyReminderRecipient(ctx, row, c, task)
	if err != nil {
		return err
	}
	if time.Since(started) > time.Minute {
		return domain.Fail("APPROVER_QUALIFICATION_STALE", "催办人员资格证明已过期", 409)
	}
	return s.verifyReminderAfterAuthority(ctx, row, stored, task, proof, tenantAll)
}

// verifyReminderRecipient 在不持有数据库锁时取得当前人员及业务岗位证明。
// 参数：ctx为远端边界，row/c/task为初始任务及单据内容。
// 返回值：项目岗位证明、当前独立全租户能力证明或依赖错误。
func (s *Service) verifyReminderRecipient(ctx context.Context, row store.Task,
	c domain.Claim, task domain.Task) (*commandContext, bool, error) {
	a := domain.Actor{TenantID: row.TenantID, SubjectID: row.Assignee,
		Permissions: []string{"expense.approval.act", task.RequiredPermission}}
	if isProjectRole(task.NodeRole) {
		proof := &commandContext{actor: a, resourceID: row.ID}
		err := s.preflightBusinessAction(ctx, s.db.WithContext(ctx), proof,
			ApprovalInput{Action: "APPROVE", Round: row.Round, TaskVersion: row.Version,
				CommandInput: CommandInput{ExpectedVersion: c.Version}})
		return proof, false, err
	}
	if s.deps.Personnel == nil {
		return nil, false, domain.Fail("PERSONNEL_DIRECTORY_UNAVAILABLE", "当前人员目录未配置", 503)
	}
	if err := s.deps.Personnel.VerifyApprover(ctx, a, row.Assignee, task.RequiredPermission); err != nil {
		return nil, false, err
	}
	if task.NodeCode == "ACCOUNTANT" || task.NodeCode == "FINANCE_DIRECTOR" {
		all, err := s.verifyTargetFinanceScope(ctx, s.db.WithContext(ctx), a, row.Assignee, c.OrgID)
		return nil, all, err
	}
	return nil, false, nil
}

// verifyReminderAfterAuthority 在所有远端等待结束后重新检查本地状态及范围。
// 参数：ctx为短事务边界，expected/claim为初始版本，task/proof为冻结内容及新鲜证明，tenantAll为目录结论。
// 返回值：等待期间任务、单据、财务范围或替代映射改变时拒绝；数据库失败保留错误。
func (s *Service) verifyReminderAfterAuthority(ctx context.Context, expected store.Task,
	claim store.Claim, task domain.Task, proof *commandContext, tenantAll bool) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		c, stored, err := loadClaim(tx, claim.TenantID, claim.ID, true)
		if err != nil {
			return err
		}
		var current store.Task
		if err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?",
			expected.TenantID, expected.ID).First(&current).Error; err != nil {
			return err
		}
		if stored.Version != claim.Version || stored.ContentJSON != claim.ContentJSON ||
			stored.Status != "IN_APPROVAL" || current.Status != "PENDING" ||
			current.Version != expected.Version || current.Round != expected.Round ||
			current.Round != c.CurrentRound || current.Assignee != expected.Assignee ||
			current.ClaimID != claim.ID || current.ContentJSON != expected.ContentJSON {
			return domain.Fail("STALE_APPROVAL_TASK", "人员复核期间催办任务或单据已改变", 409)
		}
		if proof != nil {
			return verifyBusinessActionProof(tx, proof, current, task, c)
		}
		if task.NodeCode != "ACCOUNTANT" && task.NodeCode != "FINANCE_DIRECTOR" {
			return nil
		}
		a := domain.Actor{TenantID: claim.TenantID, SubjectID: current.Assignee}
		if tenantAll {
			a.Permissions = []string{"expense.scope.tenant_all"}
		}
		ok, err := financeScope(tx, a, c.OrgID)
		if err != nil {
			return err
		}
		if !ok {
			return domain.Fail("APPROVER_FINANCE_SCOPE_MISSING", "催办目标当前组织范围已失效", 422)
		}
		return nil
	})
}

// addsignHistoryOrigin 向已有任务历史读取权限的调用者展示真实加签父动作。
// 参数：db为数据库，row为已完成访问控制的当前任务。
// 返回值：包含父任务、原因和冻结资格的来源；普通任务为空，来源损坏时拒绝。
func addsignHistoryOrigin(db *gorm.DB, row store.Task) (map[string]any, error) {
	var frozen addsignTaskContent
	if err := json.Unmarshal([]byte(row.ContentJSON), &frozen); err != nil {
		return nil, err
	}
	if frozen.ParentTaskID == "" && frozen.AddsignActionID == "" {
		return nil, nil
	}
	// 历史来源保留最初加签人；后续真实转交不会改写该来源，也不授予当前办理权限。
	original := row
	original.Assignee = frozen.Assignee
	if _, err := hasAddsignAuthority(db, original, frozen.Task); err != nil {
		return nil, err
	}
	var detail store.ActionDetail
	if err := db.Where("tenant_id = ? AND action_id = ? AND task_id = ? AND claim_id = ?",
		row.TenantID, frozen.AddsignActionID, frozen.ParentTaskID, row.ClaimID).
		First(&detail).Error; err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(detail.ContentJSON), &data); err != nil {
		return nil, err
	}
	data["action"] = "ADDSIGN"
	data["action_id"] = detail.ActionID
	data["reason"] = detail.Reason
	data["from_subject_id"] = detail.FromSubjectID
	data["to_subject_id"] = detail.ToSubjectID
	data["created_at"] = detail.CreatedAt
	return data, nil
}
