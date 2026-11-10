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

type archiveProjectGroup struct {
	ClaimCount     int
	Amount         big.Int
	LastArchivedAt time.Time
}

// ArchiveProjects 按当前整单档案范围聚合已封存项目及当前净付金额。
// 参数：ctx 为请求上下文，a 为可信身份，params 为项目、单据关键词、北京封存日和分页。
// 返回值：项目档案分页或公开权限、筛选与账本错误。
func (s *Service) ArchiveProjects(ctx context.Context, a domain.Actor, params map[string]string) (any, error) {
	if a.TenantID == "" || a.SubjectID == "" {
		return nil, domain.Fail("SESSION_EXPIRED", "请重新登录", 401)
	}
	if e := require(a, "expense.archive.read"); e != nil {
		return nil, e
	}
	f, e := parseQueryFilters(params, "claims")
	if e != nil {
		return nil, e
	}
	groups := map[string]*archiveProjectGroup{}
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.collectArchiveProjects(tx, a, f, groups)
	}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if e != nil {
		return nil, e
	}
	return archiveProjectPage(groups, params), nil
}

// archivedClaims 构造归档筛选，仅使用租户内已封存版本作为候选。
// 参数：tx 为只读事务，a 为可信身份，f 为合法筛选条件。
// 返回值：已限制整单范围的单据查询或配置错误。
func archivedClaims(tx *gorm.DB, a domain.Actor, f queryFilters) (*gorm.DB, error) {
	q, e := readableClaims(tx, a, false)
	if e != nil {
		return nil, e
	}
	claimFilters := f
	claimFilters.From, claimFilters.Until = time.Time{}, time.Time{}
	claimFilters.Project = ""
	q, e = applyClaimFilters(tx, q, a, claimFilters)
	if e != nil {
		return nil, e
	}
	archives := archiveProjectsQuery(tx, a.TenantID, f).Select("claim_id")
	return q.Where("id IN (?)", archives), nil
}

// archiveProjectsQuery 按北京封存业务日限制已封存版本，结束日次日不含。
// 参数：tx 为只读数据库，tenant 为可信租户，f 为合法日期条件。
// 返回值：受租户和封存状态约束的档案查询。
func archiveProjectsQuery(tx *gorm.DB, tenant string, f queryFilters) *gorm.DB {
	q := tx.Model(&store.Archive{}).Where("tenant_id = ? AND status = ?", tenant, "SEALED")
	// 生产封存时间由worker写入UTC RFC3339Nano；秒级前缀避免小数精度影响日边界。
	sealed := "SUBSTR(" + queryJSONText(tx, "manifest_json", "sealed_at") + ", 1, 19)"
	if !f.From.IsZero() {
		q = q.Where(sealed+" >= ?", f.From.UTC().Format("2006-01-02T15:04:05"))
	}
	if !f.Until.IsZero() {
		q = q.Where(sealed+" < ?", f.Until.UTC().Format("2006-01-02T15:04:05"))
	}
	return q
}

// collectArchiveProjects 在一致性事务内分批扫描全部候选，不截断为单据当前页。
// 参数：tx 为只读事务，a 为可信身份，f 为合法条件，groups 接收项目聚合。
// 返回值：权限、档案或财务账本错误。
func (s *Service) collectArchiveProjects(tx *gorm.DB, a domain.Actor, f queryFilters, groups map[string]*archiveProjectGroup) error {
	q, e := archivedClaims(tx, a, f)
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
		for _, row := range rows {
			if e = s.addArchiveProjectClaim(tx, a, f, row, groups); e != nil {
				return e
			}
		}
		last = rows[len(rows)-1].ID
	}
}

// addArchiveProjectClaim 逐单确认整单档案权限并合并当前净付项目分摊。
// 参数：tx 为只读事务，a 为可信身份，f 为筛选，row 为单据，groups 为聚合器。
// 返回值：内容、范围或净付账本核对错误。
func (s *Service) addArchiveProjectClaim(tx *gorm.DB, a domain.Actor, f queryFilters, row store.Claim, groups map[string]*archiveProjectGroup) error {
	var c domain.Claim
	if e := json.Unmarshal([]byte(row.ContentJSON), &c); e != nil {
		return e
	}
	c.Status, c.Version, c.TenantID, c.ID = row.Status, row.Version, row.TenantID, row.ID
	c.ActivePaymentID = ""
	if row.ActivePaymentID != nil {
		c.ActivePaymentID = *row.ActivePaymentID
	}
	full, e := s.fullRead(tx, a, c)
	if e != nil || !full {
		return e
	}
	sealedAt, e := latestArchiveSealedAt(tx, a.TenantID, c.ID, f)
	if e != nil {
		return e
	}
	if sealedAt.IsZero() {
		return nil
	}
	projects, _, e := statisticsPaidAmounts(tx, c, c.Items)
	if e != nil {
		return e
	}
	for project, amount := range projects {
		if project == "" || f.Project != "" && project != f.Project {
			continue
		}
		if groups[project] == nil {
			groups[project] = &archiveProjectGroup{}
		}
		group := groups[project]
		group.ClaimCount++
		group.Amount.Add(&group.Amount, amount)
		if sealedAt.After(group.LastArchivedAt) {
			group.LastArchivedAt = sealedAt
		}
	}
	return nil
}

// latestArchiveSealedAt 逐批读取完整匹配封存版本，精确比较纳秒封存时间。
// 参数：tx 为只读事务，tenant/claim 为可信租户与单据，f 为封存日窗口。
// 返回值：真实最新封存时间，元数据缺失或非法时失败关闭。
func latestArchiveSealedAt(tx *gorm.DB, tenant, claim string, f queryFilters) (time.Time, error) {
	latest, last := time.Time{}, ""
	for {
		var rows []store.Archive
		e := archiveProjectsQuery(tx, tenant, f).Where("claim_id = ? AND id > ?", claim, last).
			Order("id ASC").Limit(100).Find(&rows).Error
		if e != nil {
			return time.Time{}, e
		}
		if len(rows) == 0 {
			return latest, nil
		}
		for _, row := range rows {
			var content struct {
				SealedAt string `json:"sealed_at"`
			}
			if e = json.Unmarshal([]byte(row.ManifestJSON), &content); e != nil {
				return time.Time{}, e
			}
			sealed, e := time.Parse(time.RFC3339Nano, content.SealedAt)
			if e != nil || len(content.SealedAt) == 0 || content.SealedAt[len(content.SealedAt)-1] != 'Z' {
				return time.Time{}, domain.Fail("ARCHIVE_METADATA_INVALID", "已封存档案缺少有效封存时间", 409)
			}
			if sealed.After(latest) {
				latest = sealed
			}
		}
		last = rows[len(rows)-1].ID
	}
}

// archiveProjectPage 对完整项目聚合结果稳定排序后分页，金额保持十进制字符串。
// 参数：groups 为全部已授权项目聚合，params 为有限分页条件。
// 返回值：含统计口径、总项目数与分页数据的响应。
func archiveProjectPage(groups map[string]*archiveProjectGroup, params map[string]string) map[string]any {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	page, size, offset := pageArgs(params)
	if offset > len(keys) {
		offset = len(keys)
	}
	end := offset + size
	if end > len(keys) {
		end = len(keys)
	}
	items := []any{}
	for _, key := range keys[offset:end] {
		group := groups[key]
		items = append(items, map[string]any{"project_id": key, "claim_count": group.ClaimCount,
			"amount_minor": group.Amount.String(), "last_archived_at": group.LastArchivedAt})
	}
	return map[string]any{"items": items, "total": len(keys), "page": page, "page_size": size,
		"amount_basis": "CURRENT_PAID_NET", "date_basis": "SEALED_AT"}
}
