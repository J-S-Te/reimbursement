package application

import (
	"context"
	"encoding/json"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// QueryApprovalRecovery 读取明确组织范围内当前轮次待办，不改变普通审批任务的受理人边界。
// 参数：ctx 为请求边界，a 为当前身份，id 为可选任务标识，params 为有界分页与单据筛选。
// 返回值：待核验恢复列表或详情；不声明原人已经失职，提交动作时重新实时核验。
func (s *Service) QueryApprovalRecovery(ctx context.Context, a domain.Actor, id string, params map[string]string) (json.RawMessage, error) {
	if e := require(a, "expense.claim.read"); e != nil {
		return nil, e
	}
	if e := require(a, "expense.approval.recover"); e != nil {
		return nil, e
	}
	db := s.db.WithContext(ctx)
	q, e := approvalRecoveryQuery(db, a, id, params)
	if e != nil {
		return nil, e
	}
	var total int64
	if e = q.Count(&total).Error; e != nil {
		return nil, e
	}
	page, size, offset := pageArgs(params)
	var rows []store.Task
	if e = q.Select("task.*").Order("task.created_at DESC, task.id DESC").Offset(offset).Limit(size).Find(&rows).Error; e != nil {
		return nil, e
	}
	items := []any{}
	for _, row := range rows {
		item, err := s.approvalRecoveryItem(db, a, row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if id != "" {
		if len(items) != 1 {
			return nil, domain.Fail("RESOURCE_NOT_FOUND", "审批恢复任务不存在或无权访问", 404)
		}
		return json.Marshal(items[0])
	}
	return json.Marshal(map[string]any{"items": items, "total": total, "page": page, "page_size": size})
}

// approvalRecoveryQuery 在数据库分页前过滤租户、组织、申请人、轮次及当前待办状态。
// 参数：db 为数据库，a 为当前身份，id 为可选任务标识，params 为可选单据筛选。
// 返回值：范围受控的查询；配置解析失败时返回错误。
func approvalRecoveryQuery(db *gorm.DB, a domain.Actor, id string, params map[string]string) (*gorm.DB, error) {
	orgs, all, e := scopeOrganizations(db, a)
	if e != nil {
		return nil, e
	}
	q := db.Table("exp_approval_task AS task").Joins("JOIN exp_claim AS claim ON claim.id = task.claim_id AND claim.tenant_id = task.tenant_id").Where("task.tenant_id = ? AND task.status = ? AND claim.status = ? AND claim.applicant <> ?", a.TenantID, "PENDING", "IN_APPROVAL", a.SubjectID).Where("task.round = JSON_EXTRACT(claim.content_json, '$.current_round')")
	if !all {
		if len(orgs) == 0 {
			q = q.Where("1 = 0")
		} else {
			q = q.Where("claim.org_id IN ?", orgs)
		}
	}
	if id != "" {
		q = q.Where("task.id = ?", id)
	}
	if claim := params["claim_id"]; claim != "" {
		q = q.Where("task.claim_id = ?", claim)
	}
	return q, nil
}

// approvalRecoveryItem 返回明确恢复范围内的任务版本和冻结审批快照。
// 参数：db 为数据库，a 为当前恢复身份，row 为当前轮次待办。
// 返回值：当前责任人与版本、原冻结内容和待核验提示；读取异常返回错误。
func (s *Service) approvalRecoveryItem(db *gorm.DB, a domain.Actor, row store.Task) (any, error) {
	c, _, e := loadClaim(db, a.TenantID, row.ClaimID, false)
	if e != nil {
		return nil, e
	}
	if e = requireApprovalRecoveryScope(db, a, c); e != nil {
		return nil, e
	}
	if row.Round != c.CurrentRound || c.Status != "IN_APPROVAL" {
		return nil, domain.Fail("STALE_APPROVAL_TASK", "审批任务已改变，请刷新恢复列表", 409)
	}
	var task domain.Task
	if e = json.Unmarshal([]byte(row.ContentJSON), &task); e != nil {
		return nil, e
	}
	frozenAssignee := task.Assignee
	task.Assignee, task.Version, task.Status = row.Assignee, row.Version, row.Status
	raw, e := json.Marshal(task)
	if e != nil {
		return nil, e
	}
	var item map[string]any
	if e = json.Unmarshal(raw, &item); e != nil {
		return nil, e
	}
	var snapshot store.Snapshot
	if e = db.Where("tenant_id = ? AND claim_id = ? AND round = ?", a.TenantID, c.ID, row.Round).First(&snapshot).Error; e != nil {
		return nil, e
	}
	var content struct {
		Claim domain.Claim `json:"claim"`
	}
	if e = json.Unmarshal([]byte(snapshot.ContentJSON), &content); e != nil {
		return nil, e
	}
	item["snapshot"], e = s.claimView(db, a, content.Claim)
	item["claim_version"], item["org_id"], item["claim_title"] = c.Version, c.OrgID, c.Title
	item["frozen_assignee"] = frozenAssignee
	item["recovery_qualification_status"] = "REQUIRES_CURRENT_VERIFICATION"
	item["allowed_actions"] = []string{"RECOVER"}
	return item, e
}
