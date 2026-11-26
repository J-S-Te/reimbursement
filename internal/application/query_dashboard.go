package application

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// enrichClaimQueryView 补充列表摘要，异常仅从已裁剪条目汇总。
// 参数：db 为数据库，a 为当前身份，c 为单据，value 为已授权视图。
// 返回值：带可读摘要的单据视图或读取错误。
func enrichClaimQueryView(db *gorm.DB, a domain.Actor, c domain.Claim, value any) (any, error) {
	view, ok := value.(map[string]any)
	if !ok {
		return value, nil
	}
	view["applicant_subject_id"], view["org_id"] = c.Applicant, c.OrgID
	if c.ApplicantName != "" {
		view["applicant_name"] = c.ApplicantName
	}
	view["created_at"] = c.CreatedAt
	if _, exists := view["claim_type"]; !exists {
		raw, e := json.Marshal(c)
		if e != nil {
			return nil, e
		}
		var common struct {
			ClaimType string `json:"claim_type"`
		}
		if e = json.Unmarshal(raw, &common); e != nil {
			return nil, e
		}
		view["claim_type"] = common.ClaimType
	}
	raw, e := json.Marshal(view["items"])
	if e != nil {
		return nil, e
	}
	var items []domain.Item
	if e = json.Unmarshal(raw, &items); e != nil {
		return nil, e
	}
	over, abnormal := false, false
	for _, item := range items {
		over = over || item.OverStandard
		abnormal = abnormal || item.TravelException
	}
	view["has_over_standard"], view["has_exception"] = over, abnormal
	q := db.Model(&store.Task{}).Where("tenant_id = ? AND claim_id = ? AND round = ? AND status = ?", a.TenantID, c.ID, c.CurrentRound, "PENDING")
	if view["scope_limited"] == true {
		q = q.Where("assignee = ?", a.SubjectID)
	}
	var codes []string
	if e = q.Distinct("node_code").Order("node_code").Limit(101).Pluck("node_code", &codes).Error; e != nil {
		return nil, e
	}
	if len(codes) > 100 {
		return nil, domain.Fail("APPROVAL_NODE_LIMIT", "当前审批节点超过受控上限", 422)
	}
	sort.Strings(codes)
	view["current_approval_nodes"], view["current_approval_node"] = codes, strings.Join(codes, " / ")
	return view, nil
}

// dashboardPaidMonth 统计本人当前有效付款，以北京当月的实际付款业务日为口径。
// 参数：db 为数据库，a 为可信身份，now 为当前时刻。
// 返回值：当月仍有效的已付单据数或数据库错误。
func dashboardPaidMonth(db *gorm.DB, a domain.Actor, now time.Time) (int64, error) {
	beijing := now.In(time.FixedZone("Asia/Shanghai", 8*3600))
	from := time.Date(beijing.Year(), beijing.Month(), 1, 0, 0, 0, 0, beijing.Location())
	paidDate := queryJSONText(db, "content_json", "paid_date")
	payments := db.Model(&store.Payment{}).Select("id").Where("tenant_id = ?", a.TenantID).Where(paidDate+" >= ? AND "+paidDate+" < ?", from.Format("2006-01-02"), from.AddDate(0, 1, 0).Format("2006-01-02"))
	var total int64
	e := db.Model(&store.Claim{}).Where("tenant_id = ? AND applicant = ? AND status = ? AND active_payment_id IN (?)", a.TenantID, a.SubjectID, "PAID", payments).Count(&total).Error
	return total, e
}

// dashboardPendingTasks 查询当前仍可办理的本人任务，并返回有界的前五条详情。
// 参数：db 为数据库，a 为可信身份。
// 返回值：真实待办计数、最多五个受控快照和读取错误。
func (s *Service) dashboardPendingTasks(db *gorm.DB, a domain.Actor) (int64, []any, error) {
	items := []any{}
	if !a.Has("expense.approval.act") {
		return 0, items, nil
	}
	round := queryJSONText(db, "exp_claim.content_json", "current_round")
	claims := db.Model(&store.Claim{}).Select("1").Where("exp_claim.tenant_id = exp_approval_task.tenant_id AND exp_claim.id = exp_approval_task.claim_id AND exp_claim.status = ?", "IN_APPROVAL").Where(round+" = exp_approval_task.round").Where("exp_claim.applicant <> ?", a.SubjectID)
	q := db.Model(&store.Task{}).Where("tenant_id = ? AND assignee = ? AND status = ?", a.TenantID, a.SubjectID, "PENDING").Where("EXISTS (?)", claims)
	required := queryJSONText(db, "content_json", "required_permission")
	q = q.Where(required+" IN ?", a.Permissions)
	var total int64
	if e := q.Count(&total).Error; e != nil {
		return 0, nil, e
	}
	var rows []store.Task
	if e := q.Order("created_at ASC, id ASC").Limit(5).Find(&rows).Error; e != nil {
		return 0, nil, e
	}
	for _, row := range rows {
		value, e := s.listTasks(db, a, row.ID, nil)
		if e != nil {
			return 0, nil, e
		}
		items = append(items, value)
	}
	return total, items, nil
}
