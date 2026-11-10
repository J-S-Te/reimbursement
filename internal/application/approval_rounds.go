package application

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// ApprovalRounds 返回当前范围内不可变审批轮次及责任来源，不用当前岗位覆盖旧事实。
// 参数：ctx 为请求边界，a 为可信主体，claimID 为单据，params 为有界分页。
// 返回值：轮次、冻结快照和责任链；部分受理人只看到本任务覆盖内容。
func (s *Service) ApprovalRounds(ctx context.Context, a domain.Actor, claimID string, params map[string]string) (any, error) {
	db := s.db.WithContext(ctx)
	c, _, err := loadClaim(db, a.TenantID, claimID, false)
	if err != nil {
		return nil, err
	}
	if err = s.canRead(db, a, c); err != nil {
		return nil, err
	}
	full, err := s.fullRead(db, a, c)
	if err != nil {
		return nil, err
	}
	q := db.Model(&store.Snapshot{}).Where("tenant_id = ? AND claim_id = ?", a.TenantID, claimID)
	if !full {
		q = q.Where("round IN (?)", db.Model(&store.Task{}).Select("round").Where("tenant_id = ? AND claim_id = ? AND assignee = ?", a.TenantID, claimID, a.SubjectID))
	}
	var total int64
	if err = q.Count(&total).Error; err != nil {
		return nil, err
	}
	page, size, offset := pageArgs(params)
	var rows []store.Snapshot
	if err = q.Order("round DESC").Limit(size).Offset(offset).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := []any{}
	for _, row := range rows {
		value, e := s.approvalRoundView(db, a, row, full)
		if e != nil {
			return nil, e
		}
		items = append(items, value)
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}, nil
}

// approvalRoundView 投影一个历史轮次并按当前受理范围裁剪任务、来源与动作。
// 参数：db 为数据库，a 为当前主体，row 为不可变轮次，full 表示当前整单权限。
// 返回值：经过范围过滤的责任视图或持久内容错误。
func (s *Service) approvalRoundView(db *gorm.DB, a domain.Actor, row store.Snapshot, full bool) (any, error) {
	var frozen approvalSnapshot
	if err := json.Unmarshal([]byte(row.ContentJSON), &frozen); err != nil {
		return nil, err
	}
	view, err := s.claimView(db, a, frozen.Claim)
	if err != nil {
		return nil, err
	}
	q := db.Where("tenant_id = ? AND claim_id = ? AND round = ?", a.TenantID, row.ClaimID, row.Round)
	if !full {
		q = q.Where("assignee = ?", a.SubjectID)
	}
	var tasks []store.Task
	if err = q.Order("node,id").Find(&tasks).Error; err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	projects := map[string]bool{}
	for _, task := range tasks {
		allowed[task.ID] = true
		var content domain.Task
		if json.Unmarshal([]byte(task.ContentJSON), &content) != nil {
			return nil, domain.Fail("APPROVAL_HISTORY_INVALID", "历史责任数据不可读", 409)
		}
		for _, key := range content.AllocationKeys {
			projects[key] = true
		}
	}
	frozenTasks := []domain.Task{}
	for _, task := range frozen.Approval.Tasks {
		if full || allowed[task.ID] {
			frozenTasks = append(frozenTasks, task)
		}
	}
	refs := []domain.ProjectReference{}
	for _, ref := range frozen.Approval.References {
		if full || roundReferenceCovered(frozen.Claim, projects, ref) {
			refs = append(refs, ref)
		}
	}
	actions := []store.Action{}
	aq := db.Where("tenant_id = ? AND claim_id = ? AND round = ?", a.TenantID, row.ClaimID, row.Round)
	if !full {
		ids := []string{}
		for id := range allowed {
			ids = append(ids, id)
		}
		aq = aq.Where("task_id IN ?", ids)
	}
	if err = aq.Order("created_at,id").Limit(2001).Find(&actions).Error; err != nil {
		return nil, err
	}
	if len(actions) > 2000 {
		return nil, domain.Fail("APPROVAL_HISTORY_LIMIT", "审批历史超过分页边界", 422)
	}
	return map[string]any{"round": row.Round, "content_hash": row.ContentHash, "created_at": row.CreatedAt, "snapshot": view, "frozen_tasks": frozenTasks, "current_tasks": tasks, "references": refs, "actions": actions, "rule_version": frozen.Approval.RuleVersion}, nil
}

// roundReferenceCovered 仅选择当前任务实际覆盖的项目服务项引用。
// 参数：c 为冻结单据，keys 为受理分摊键，ref 为一个权威引用。
// 返回值：是否允许展示此引用的责任来源。
func roundReferenceCovered(c domain.Claim, keys map[string]bool, ref domain.ProjectReference) bool {
	if keys["*"] {
		return true
	}
	for _, item := range c.Items {
		if keys[item.ID+"/"+ref.ProjectID+"/"+ref.ServiceItemID] {
			return true
		}
	}
	return false
}
