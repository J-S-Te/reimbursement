package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"time"
)

// configs 读取租户内同类配置，限制配置规模防止无界加载。
// 参数：tx 为数据库，tenant 为租户，kind 为配置种类，published 为是否只读发布版本。
// 返回值：配置版本与读取错误。
func configs(tx *gorm.DB, tenant, kind string, published bool) ([]store.Config, error) {
	var rows []store.Config
	q := tx.Where("tenant_id = ? AND kind = ?", tenant, kind)
	if published {
		q = q.Where("published = ?", true).Where("id NOT IN (?)", tx.Model(&store.PolicyLifecycle{}).Select("config_id").Where("tenant_id = ? AND effective_at <= ?", tenant, time.Now().UTC()))
	}
	e := q.Order("created_at DESC").Limit(2001).Find(&rows).Error
	if len(rows) > 2000 {
		return nil, domain.Fail("CONFIGURATION_LIMIT", "配置超过受控上限，请管理员整理", 422)
	}
	return rows, e
}

// financeScope 检查当前可信全量能力或显式本地组织范围，不从角色名推断。
// 参数：tx 为数据库，a 为可信身份，org 为单据组织。
// 返回值：当前组织是否属于已发布财务范围及数据库错误。
func financeScope(tx *gorm.DB, a domain.Actor, org string) (bool, error) {
	if a.TenantID != "" && a.SubjectID != "" && a.Has("expense.scope.tenant_all") {
		return true, nil
	}
	rows, e := configs(tx, a.TenantID, "scopes", true)
	if e != nil {
		return false, e
	}
	for _, row := range rows {
		var scope domain.Scope
		if e = json.Unmarshal([]byte(row.ContentJSON), &scope); e != nil {
			return false, e
		}
		if scope.SubjectID != a.SubjectID {
			continue
		}
		if scope.TenantAll {
			return true, nil
		}
		for _, o := range scope.OrgIDs {
			if o == org {
				return true, nil
			}
		}
	}
	return false, nil
}

// fullRead 确定是否可以查看完整跨项目金额和票据。
// 参数：tx 为数据库，a 为身份，c 为单据。
// 返回值：整单访问权及读取错误。
func (s *Service) fullRead(tx *gorm.DB, a domain.Actor, c domain.Claim) (bool, error) {
	if a.TenantID == "" || a.TenantID != c.TenantID || !a.Has("expense.claim.read") {
		return false, nil
	}
	if a.SubjectID == c.Applicant {
		return true, nil
	}
	if !hasFinancialReadAbility(a) {
		return false, nil
	}
	return financeScope(tx, a, c.OrgID)
}

// canRead 检查整单或本人审批任务的最小可见性。
// 参数：tx 为数据库，a 为身份，c 为单据。
// 返回值：无可见范围时隐藏对象。
func (s *Service) canRead(tx *gorm.DB, a domain.Actor, c domain.Claim) error {
	if a.TenantID == "" || a.TenantID != c.TenantID {
		return domain.Fail("RESOURCE_NOT_FOUND", "单据不存在或无权访问", 404)
	}
	full, e := s.fullRead(tx, a, c)
	if e != nil {
		return e
	}
	if full {
		return nil
	}
	if a.Has("expense.claim.read") && a.Has("expense.approval.act") {
		var n int64
		e = tx.Model(&store.Task{}).Where("tenant_id = ? AND claim_id = ? AND assignee = ?", a.TenantID, c.ID, a.SubjectID).Count(&n).Error
		if e != nil {
			return e
		}
		if n > 0 {
			return nil
		}
	}
	return domain.Fail("RESOURCE_NOT_FOUND", "单据不存在或无权访问", 404)
}

// requireFinancial 将财务动作能力与本地组织范围同时作为前置条件。
// 参数：tx 为数据库，a 为身份，c 为单据，permission 为具体财务动作。
// 返回值：能力或对象范围错误。
func requireFinancial(tx *gorm.DB, a domain.Actor, c domain.Claim, permission string) error {
	if a.TenantID == "" || a.TenantID != c.TenantID {
		return domain.Fail("FORBIDDEN", "租户边界不匹配", 403)
	}
	if e := require(a, permission); e != nil {
		return e
	}
	ok, e := financeScope(tx, a, c.OrgID)
	if e != nil {
		return e
	}
	if !ok {
		return domain.Fail("FORBIDDEN", "没有此单据的财务数据范围", 403)
	}
	return nil
}

// hasFinancialReadAbility 识别需要显式本地范围的财务读取或受控处理能力。
// 参数：a 为当前可信身份。
// 返回值：是否具备财务对象读取的动作基础，仍须另行检查范围。
func hasFinancialReadAbility(a domain.Actor) bool {
	for _, permission := range []string{"expense.approval.recover", "expense.integration.manage", "expense.scope.tenant_all", "expense.finance.review", "expense.payment.read", "expense.payment.record", "expense.audit.read", "expense.archive.read", "expense.claim.reopen", "expense.claim.approved_void", "expense.payment.exception.record", "expense.payment.exception.review"} {
		if a.Has(permission) {
			return true
		}
	}
	return false
}
