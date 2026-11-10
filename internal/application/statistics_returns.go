package application

import (
	"encoding/json"
	"math/big"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type statisticsReturnFact struct {
	ClaimID   string
	Submitted bool
	Returned  bool
}

// collectStatisticsReturns 在原一致性事务内批量统计真实提交与当前可见历史退回。
// 参数：tx为只读事务，a为可信主体，rows为至多100单，totals为跨批聚合器。
// 返回值：权限配置、内容编码或事实查询错误。
func collectStatisticsReturns(tx *gorm.DB, a domain.Actor, rows []store.Claim, totals *statisticsTotals) error {
	ids, fullIDs, err := statisticsReturnClaimIDs(tx, a, rows)
	if err != nil {
		return err
	}
	submitted := tx.Model(&store.Snapshot{}).Select("1").Where(
		"tenant_id = ? AND claim_id = stats_claim.id", a.TenantID)
	returned := tx.Table((store.Action{}).TableName()+" AS stats_action").Select("1").Where(
		"stats_action.tenant_id = ? AND stats_action.claim_id = stats_claim.id "+
			"AND stats_action.action = ?", a.TenantID, "RETURN")
	if a.Has("expense.approval.act") {
		// 与审批历史权限一致：局部读者只能获知本人历史任务上的动作。
		ownTask := tx.Table((store.Task{}).TableName()+" AS stats_task").Select("1").Where(
			"stats_task.tenant_id = ? AND stats_task.id = stats_action.task_id "+
				"AND stats_task.claim_id = stats_action.claim_id "+
				"AND stats_task.round = stats_action.round AND stats_task.assignee = ?",
			a.TenantID, a.SubjectID)
		returned = returned.Where("stats_action.claim_id IN ? OR EXISTS (?)", fullIDs, ownTask)
	} else {
		returned = returned.Where("stats_action.claim_id IN ?", fullIDs)
	}
	var facts []statisticsReturnFact
	err = tx.Table((store.Claim{}).TableName()+" AS stats_claim").Select(
		"stats_claim.id AS claim_id, EXISTS (?) AS submitted, EXISTS (?) AS returned", submitted, returned).
		Where("stats_claim.tenant_id = ? AND stats_claim.id IN ?", a.TenantID, ids).Limit(100).Scan(&facts).Error
	if err != nil {
		return err
	}
	for _, fact := range facts {
		if fact.Submitted {
			totals.Submitted++
			if fact.Returned {
				totals.Returned++
			}
		}
	}
	return nil
}

// statisticsReturnClaimIDs 复用当前整单权限条件，为批量事实查询区分完整和历史任务范围。
// 参数：tx为只读事务，a为当前主体，rows为当前有界单据批次。
// 返回值：全部单据标识、允许整单读的标识以及范围或内容错误。
func statisticsReturnClaimIDs(tx *gorm.DB, a domain.Actor, rows []store.Claim) ([]string, []string, error) {
	orgs, all, err := scopeOrganizations(tx, a)
	if err != nil {
		return nil, nil, err
	}
	organizations := map[string]bool{}
	for _, org := range orgs {
		organizations[org] = true
	}
	ids, fullIDs := make([]string, 0, len(rows)), make([]string, 0, len(rows))
	for _, row := range rows {
		var claim domain.Claim
		if err = json.Unmarshal([]byte(row.ContentJSON), &claim); err != nil {
			return nil, nil, err
		}
		ids = append(ids, row.ID)
		full := claim.Applicant == a.SubjectID || hasFinancialReadAbility(a) && (all || organizations[claim.OrgID])
		if claim.TenantID == a.TenantID && a.Has("expense.claim.read") && full {
			fullIDs = append(fullIDs, row.ID)
		}
	}
	return ids, fullIDs, nil
}

// statisticsReturnRate 用整数计算万分比，避免浮点误差和计数乘法溢出。
// 参数：submitted为去重已提交单据数，returned为其中可见真实退回单据数。
// 返回值：向下取整的0至10000基点；零分母时返回nil以明确尚无统计基础。
func statisticsReturnRate(submitted, returned int) *int {
	if submitted == 0 {
		return nil
	}
	rate := new(big.Int).Mul(big.NewInt(int64(returned)), big.NewInt(10000))
	rate.Quo(rate, big.NewInt(int64(submitted)))
	value := int(rate.Int64())
	return &value
}
