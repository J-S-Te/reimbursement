package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"math/big"
	"sort"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type statisticsBucket struct {
	Amount big.Int
	Count  int
}

type statisticsTotals struct {
	Monthly, ByType, ByProject     map[string]*statisticsBucket
	Total, OverStandard, Exception int
	Submitted, Returned            int
}

// Statistics 按当前可见范围统计创建月归属净额和真实提交退回率。
// 参数：ctx 为请求上下文，a 为可信身份，params 为最多366日的筛选窗口。
// 返回值：金额、风险、去重提交退回计数及基点；失败返回公开业务错误。
func (s *Service) Statistics(ctx context.Context, a domain.Actor, params map[string]string) (any, error) {
	if a.TenantID == "" || a.SubjectID == "" {
		return nil, domain.Fail("SESSION_EXPIRED", "请重新登录", 401)
	}
	if e := require(a, "expense.claim.read"); e != nil {
		return nil, e
	}
	filters, e := statisticsFilters(params, time.Now())
	if e != nil {
		return nil, e
	}
	totals := statisticsTotals{Monthly: map[string]*statisticsBucket{}, ByType: map[string]*statisticsBucket{}, ByProject: map[string]*statisticsBucket{}}
	for month := time.Date(filters.From.Year(), filters.From.Month(), 1, 0, 0, 0, 0, filters.From.Location()); month.Before(filters.Until); month = month.AddDate(0, 1, 0) {
		totals.Monthly[month.Format("2006-01")] = &statisticsBucket{}
	}
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.collectStatistics(tx, a, filters, &totals)
	}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if e != nil {
		return nil, e
	}
	return map[string]any{
		"monthly":     statisticsRows(totals.Monthly, "month"),
		"by_type":     statisticsRows(totals.ByType, "claim_type"),
		"by_project":  statisticsRows(totals.ByProject, "project_id"),
		"total_count": totals.Total, "over_standard_count": totals.OverStandard,
		"exception_count": totals.Exception, "amount_basis": "CURRENT_PAID_NET",
		"submitted_count": totals.Submitted, "returned_count": totals.Returned,
		"return_rate_basis_points": statisticsReturnRate(totals.Submitted, totals.Returned),
		"return_rate_basis":        "DISTINCT_SUBMITTED_CLAIMS_WITH_RETURN_ACTION",
		"date_basis":               "CLAIM_CREATED_AT", "date_from": filters.From.Format("2006-01-02"),
		"date_to": filters.Until.AddDate(0, 0, -1).Format("2006-01-02"),
	}, nil
}

// statisticsFilters 建立默认最近十二个北京月份并限制客户端窗口。
// 参数：params 为客户端筛选，now 为当前时刻。
// 返回值：已经校验的条件，超过366日或日期不合法时返回422。
func statisticsFilters(params map[string]string, now time.Time) (queryFilters, error) {
	f, e := parseQueryFilters(params, "claims")
	if e != nil {
		return f, e
	}
	local := now.In(time.FixedZone("Asia/Shanghai", 8*3600))
	first := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, local.Location())
	if f.From.IsZero() {
		f.From = first.AddDate(0, -11, 0)
	}
	if f.Until.IsZero() {
		f.Until = first.AddDate(0, 1, 0)
	}
	if !f.From.Before(f.Until) || f.Until.Sub(f.From) > 366*24*time.Hour {
		return f, domain.Fail("INVALID_STATISTICS_WINDOW", "统计日期范围须为1至366个北京业务日", 422)
	}
	return f, nil
}

// collectStatistics 在一致性事务中按主键流式读取全部匹配单据，不采样或截断。
// 参数：tx 为只读事务，a 为可信身份，f 为合法窗口，totals 为聚合器。
// 返回值：范围、账本完整性或读取错误。
func (s *Service) collectStatistics(tx *gorm.DB, a domain.Actor, f queryFilters, totals *statisticsTotals) error {
	q, e := readableClaims(tx, a, true)
	if e != nil {
		return e
	}
	q, e = applyClaimFilters(tx, q, a, f)
	if e != nil {
		return e
	}
	last := ""
	for {
		var rows []store.Claim
		if e = q.Where("id > ?", last).Order("id ASC").Limit(100).Find(&rows).Error; e != nil {
			return e
		}
		if len(rows) == 0 {
			return nil
		}
		if e = collectStatisticsReturns(tx, a, rows, totals); e != nil {
			return e
		}
		for _, row := range rows {
			if e = s.addStatisticsClaim(tx, a, row, totals); e != nil {
				return e
			}
		}
		last = rows[len(rows)-1].ID
	}
}

// addStatisticsClaim 复用单据可见性裁剪后聚合一单，金额须经不可变账本核对。
// 参数：tx 为只读事务，a 为身份，row 为当前行，totals 为聚合器。
// 返回值：内容编码、权限或账本完整性错误。
func (s *Service) addStatisticsClaim(tx *gorm.DB, a domain.Actor, row store.Claim, totals *statisticsTotals) error {
	var c domain.Claim
	if e := json.Unmarshal([]byte(row.ContentJSON), &c); e != nil {
		return e
	}
	c.Status, c.Version, c.CreatedAt = row.Status, row.Version, row.CreatedAt
	c.ActivePaymentID = ""
	if row.ActivePaymentID != nil {
		c.ActivePaymentID = *row.ActivePaymentID
	}
	value, e := s.claimView(tx, a, c)
	if e != nil {
		return e
	}
	raw, e := json.Marshal(value)
	if e != nil {
		return e
	}
	var visible domain.Claim
	if e = json.Unmarshal(raw, &visible); e != nil {
		return e
	}
	var meta struct {
		ClaimType string `json:"claim_type"`
	}
	if e = json.Unmarshal([]byte(row.ContentJSON), &meta); e != nil {
		return e
	}
	projects, amount, e := statisticsPaidAmounts(tx, c, visible.Items)
	if e != nil {
		return e
	}
	month := c.CreatedAt.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01")
	addStatisticsBucket(totals.Monthly, month, amount)
	addStatisticsBucket(totals.ByType, meta.ClaimType, amount)
	for project, value := range projects {
		addStatisticsBucket(totals.ByProject, project, value)
	}
	totals.Total++
	over, abnormal := false, false
	for _, item := range visible.Items {
		over = over || item.OverStandard
		abnormal = abnormal || item.TravelException
	}
	if over {
		totals.OverStandard++
	}
	if abnormal {
		totals.Exception++
	}
	return nil
}

// statisticsPaidAmounts 仅从已核对当前有效净额的批准分摊取出可见部分。
// 参数：tx 为只读事务，c 为完整单据，items 为已获授权的可见条目。
// 返回值：可见项目金额与合计，账本不一致时失败关闭。
func statisticsPaidAmounts(tx *gorm.DB, c domain.Claim, items []domain.Item) (map[string]*big.Int, *big.Int, error) {
	projects := map[string]*big.Int{}
	total := new(big.Int)
	if c.Status == "PAID" && c.ActivePaymentID == "" {
		return nil, nil, domain.Fail("STATISTICS_LEDGER_CONFLICT", "已付单据缺少有效付款引用，无法统计", 409)
	}
	paid := c.Status == "PAID"
	if paid {
		if e := validateStatisticsPaidNet(tx, c); e != nil {
			return nil, nil, e
		}
	}
	for _, item := range items {
		for _, allocation := range item.Allocations {
			if projects[allocation.ProjectID] == nil {
				projects[allocation.ProjectID] = new(big.Int)
			}
			if paid {
				amount := big.NewInt(int64(allocation.Amount))
				projects[allocation.ProjectID].Add(projects[allocation.ProjectID], amount)
				total.Add(total, amount)
			}
		}
	}
	return projects, total, nil
}

// validateStatisticsPaidNet 核对有效付款与完整不可变效果，禁止历史付款毛额冒充当前净额。
// 参数：tx 为只读事务，c 为PAID且携带当前有效付款的单据。
// 返回值：账本和批准内容不一致时返回公开错误。
func validateStatisticsPaidNet(tx *gorm.DB, c domain.Claim) error {
	var payment store.Payment
	if e := tx.Where("tenant_id = ? AND claim_id = ? AND id = ?", c.TenantID, c.ID, c.ActivePaymentID).First(&payment).Error; e != nil {
		return e
	}
	var content PaymentResult
	if e := json.Unmarshal([]byte(payment.ContentJSON), &content); e != nil {
		return e
	}
	if content.Amount != c.Payable {
		return domain.Fail("STATISTICS_LEDGER_CONFLICT", "当前付款与批准金额不一致，无法统计", 409)
	}
	effects, e := readSpendEffects(tx, c)
	if e != nil {
		return e
	}
	net := map[string]*big.Int{}
	for _, effect := range effects {
		for _, allocation := range effect.Allocations {
			amount, ok := new(big.Int).SetString(allocation.AmountMinor, 10)
			if !ok {
				return domain.Fail("STATISTICS_LEDGER_CONFLICT", "财务效果金额不合法，无法统计", 409)
			}
			key := domain.SpendKey(allocation.ProjectID, allocation.ServiceItemID, allocation.FeeType)
			if net[key] == nil {
				net[key] = new(big.Int)
			}
			net[key].Add(net[key], amount)
		}
	}
	expected := statisticsExpectedAmounts(c)
	expectedTotal := new(big.Int)
	for _, amount := range expected {
		expectedTotal.Add(expectedTotal, amount)
	}
	if expectedTotal.Cmp(big.NewInt(int64(c.Payable))) != 0 {
		return domain.Fail("STATISTICS_LEDGER_CONFLICT", "批准分摊合计与付款金额不一致，无法统计", 409)
	}
	for key, amount := range expected {
		if net[key] == nil || net[key].Cmp(amount) != 0 {
			return domain.Fail("STATISTICS_LEDGER_CONFLICT", "当前付款净额与批准分摊不一致，无法统计", 409)
		}
		delete(net, key)
	}
	for _, amount := range net {
		if amount.Sign() != 0 {
			return domain.Fail("STATISTICS_LEDGER_CONFLICT", "存在未匹配财务净额，无法统计", 409)
		}
	}
	return nil
}

// statisticsExpectedAmounts 按财务协议分组批准分摊，避免相同项目费用键的范围泄漏。
// 参数：c 为完整的已批准单据。
// 返回值：项目、服务项及费用类别分组的批准金额。
func statisticsExpectedAmounts(c domain.Claim) map[string]*big.Int {
	result := map[string]*big.Int{}
	for _, item := range c.Items {
		for _, allocation := range item.Allocations {
			var service *string
			if allocation.ServiceItemID != "" {
				value := allocation.ServiceItemID
				service = &value
			}
			key := domain.SpendKey(allocation.ProjectID, service, item.FeeType)
			if result[key] == nil {
				result[key] = new(big.Int)
			}
			result[key].Add(result[key], big.NewInt(int64(allocation.Amount)))
		}
	}
	return result
}

// addStatisticsBucket 合并一单的精确金额并递增一次对象计数。
// 参数：groups 为聚合分组，key 为维度值，amount 为当前可见净额。
// 返回值：无。
func addStatisticsBucket(groups map[string]*statisticsBucket, key string, amount *big.Int) {
	if groups[key] == nil {
		groups[key] = &statisticsBucket{}
	}
	groups[key].Amount.Add(&groups[key].Amount, amount)
	groups[key].Count++
}

// statisticsRows 将聚合维度按稳定顺序转换为精确字符串金额响应。
// 参数：groups 为分组聚合，field 为服务端固定维度字段。
// 返回值：稳定排序的全部分组，不截断为当前页。
func statisticsRows(groups map[string]*statisticsBucket, field string) []any {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := []any{}
	for _, key := range keys {
		rows = append(rows, map[string]any{field: key, "amount_minor": groups[key].Amount.String(), "count": groups[key].Count})
	}
	return rows
}
