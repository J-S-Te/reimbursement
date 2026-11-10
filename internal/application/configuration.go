package application

import (
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

// configure 保存配置草稿或发布不可变版本，绝不覆盖已发布政策。
// 参数：tx 为事务，cc 为配置管理员命令，payload 为当前类型配置。
// 返回值：配置内容、配置标识与错误。
func (s *Service) configure(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input struct {
		CommandInput
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	kind := input.Kind
	if kind == "" && cc.operation == "config.save" {
		kind = cc.resourceID
	}
	allowed := map[string]bool{"standards": true, "historical_tail_policies": true, "route_rules": true, "assignments": true, "finance_entities": true, "payee_profiles": true, "scopes": true, "employee_profiles": true, "invoice_rules": true, "city_mappings": true, "archive_retention_policies": true}
	if !allowed[kind] {
		return nil, "", domain.Fail("INVALID_CONFIGURATION", "不支持的配置种类", 422)
	}
	id := input.ID
	if cc.operation == "config.publish" {
		id = cc.resourceID
		if e := require(cc.actor, "expense.policy.publish"); e != nil {
			return nil, "", e
		}
	}
	if cc.operation == "config.publish" {
		if e := lockPolicyTenant(tx, cc.actor.TenantID); e != nil {
			return nil, "", e
		}
	}
	var row store.Config
	var body map[string]any
	var e error
	previous := int64(0)
	if id != "" {
		e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ? AND kind = ?", cc.actor.TenantID, id, kind).First(&row).Error
		if e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "配置版本不存在", 404)
			}
			return nil, "", e
		}
		if row.Published {
			return nil, "", domain.Fail("PUBLISHED_IMMUTABLE", "已发布配置不可修改，请新建版本", 409)
		}
		if e = verifyVersion(row.Version, input.ExpectedVersion); e != nil {
			return nil, "", e
		}
		previous = row.Version
		if cc.operation == "config.publish" {
			payload = json.RawMessage(row.ContentJSON)
		}
	} else {
		id, e = newID()
		if e != nil {
			return nil, "", e
		}
		row = store.Config{ID: id, TenantID: cc.actor.TenantID, Kind: kind}
	}
	if e = json.Unmarshal(payload, &body); e != nil {
		return nil, "", e
	}
	delete(body, "command_id")
	delete(body, "expected_version")
	delete(body, "kind")
	body["id"] = id
	body["version"] = previous + 1
	body["published"] = cc.operation == "config.publish"
	raw, e := encode(body)
	if e != nil {
		return nil, "", e
	}
	if e = validateConfig(kind, raw); e != nil {
		return nil, "", e
	}
	if kind == "invoice_rules" {
		policy, err := decodeInvoicePolicy(raw)
		if err != nil {
			return nil, "", err
		}
		if policy.MaxInvoiceAgeDays > 0 {
			body["date_policy"] = policy.DatePolicy
			raw, e = encode(body)
			if e != nil {
				return nil, "", e
			}
		}
	}
	row.Version = previous + 1
	row.Published = cc.operation == "config.publish"
	row.ContentJSON = raw
	if row.Published {
		if e = checkPublicationConflict(tx, row, ""); e != nil {
			return nil, "", e
		}
	}
	if previous == 0 {
		e = tx.Create(&row).Error
	} else {
		e = tx.Model(&store.Config{}).Where("tenant_id = ? AND id = ? AND version = ?", cc.actor.TenantID, id, previous).Updates(map[string]any{"version": row.Version, "published": row.Published, "content_json": raw}).Error
	}
	return body, id, e
}

// validateConfig 校验可发布配置的业务字段与有效日期。
// 参数：kind 为配置种类，raw 为标准化配置 JSON。
// 返回值：缺字段、日期或范围错误。
func validateConfig(kind, raw string) error {
	invalid := domain.Fail("INVALID_CONFIGURATION", "配置必填字段、范围或日期不合法", 422)
	switch kind {
	case "city_mappings", "archive_retention_policies":
		return validateAdditionalPolicy(kind, raw)
	case "historical_tail_policies":
		var v domain.HistoricalTailPolicy
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		if v.OrgID == "" || v.ProjectID == "" || !validPeriod(v.ValidFrom, v.ValidUntil) {
			return invalid
		}
		if _, e := time.Parse("2006-01-02", v.ExpenseCutoff); e != nil {
			return invalid
		}
	case "route_rules":
		v, e := decodeRouteRule(raw)
		if e != nil {
			return e
		}
		if v.OrgID == "" || !validPeriod(v.ValidFrom, v.ValidUntil) || v.AccountingRole != "ACCOUNTANT" {
			return invalid
		}
		if (v.RouteType == "TECHNICAL" && (v.BaseRole != "PROJECT_EXPENSE_OWNER" || v.ExtraRole != "TECHNICAL_DIRECTOR")) || (v.RouteType == "SALES" && (v.BaseRole != "SALES_DIRECTOR" || v.ExtraRole != "FINANCE_DIRECTOR")) || (v.RouteType != "TECHNICAL" && v.RouteType != "SALES") {
			return invalid
		}
	case "standards":
		var v domain.Standard
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		if v.FeeType == "" || v.OrgID == "" || v.Grade == "" || v.Limit <= 0 || !validPeriod(v.ValidFrom, v.ValidUntil) {
			return invalid
		}
		if v.Unit != "PER_NIGHT" && v.Unit != "PER_PERSON_DAY" && v.Unit != "PER_OCCURRENCE" && v.Unit != "PER_KM" {
			return invalid
		}
		if e := validateStandardDimensions(v); e != nil {
			return e
		}
	case "assignments":
		var v domain.Assignment
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		roles := map[string]bool{"PROJECT_EXPENSE_OWNER": true, "SALES_DIRECTOR": true, "TECHNICAL_DIRECTOR": true, "FINANCE_DIRECTOR": true, "ACCOUNTANT": true, "BUSINESS_REVIEW": true}
		if v.SubjectID == "" || v.OrgID == "" || !roles[v.Role] || !validPeriod(v.ValidFrom, v.ValidUntil) {
			return invalid
		}
		if v.Role == "PROJECT_EXPENSE_OWNER" && v.ProjectID == "" {
			return invalid
		}
	case "finance_entities":
		var v domain.FinanceEntity
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		if v.Name == "" || v.TaxID == "" || len(v.OrgIDs) == 0 {
			return invalid
		}
	case "payee_profiles":
		var v domain.PayeeProfile
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		if v.SubjectID == "" || strings.TrimSpace(v.AccountRef) == "" || len(v.AccountRef) > 128 {
			return invalid
		}
	case "scopes":
		var v domain.Scope
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		if v.SubjectID == "" || (!v.TenantAll && len(v.OrgIDs) == 0) {
			return invalid
		}
	case "employee_profiles":
		var v domain.EmployeeProfile
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		if v.SubjectID == "" || v.OrgID == "" || v.Grade == "" || (v.RouteType != "TECHNICAL" && v.RouteType != "SALES") {
			return invalid
		}
	case "invoice_rules":
		if _, e := decodeInvoicePolicy(raw); e != nil {
			return e
		}
		var v domain.InvoiceRule
		if e := json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		if v.InvoiceType == "" || len(v.IdentityFields) == 0 {
			return invalid
		}
		valid := map[string]bool{"invoice_type": true, "invoice_code": true, "invoice_number": true, "seller_tax_id": true}
		seen := map[string]bool{}
		for _, field := range v.IdentityFields {
			if !valid[field] || seen[field] {
				return invalid
			}
			seen[field] = true
		}
		if !seen["invoice_number"] {
			return invalid
		}
	}
	return nil
}

// validPeriod 检查闭区间日期配置，空结束日期表示持续有效。
// 参数：start/end 为 ISO 业务日期。
// 返回值：日期存在且区间不倒置时为真。
func validPeriod(start, end string) bool {
	if _, e := time.Parse("2006-01-02", start); e != nil {
		return false
	}
	if end != "" {
		if _, e := time.Parse("2006-01-02", end); e != nil || end < start {
			return false
		}
	}
	return true
}

// employeeProfile 读取唯一有效的人员费用职级与线路，拒绝客户端自报职级。
// 参数：tx 为事务，c 为申请人和组织边界。
// 返回值：当前已审核人员配置或缺失冲突错误。
func employeeProfile(tx *gorm.DB, c domain.Claim) (domain.EmployeeProfile, error) {
	rows, e := configs(tx, c.TenantID, "employee_profiles", true)
	if e != nil {
		return domain.EmployeeProfile{}, e
	}
	var found *domain.EmployeeProfile
	for _, row := range rows {
		var p domain.EmployeeProfile
		if e = json.Unmarshal([]byte(row.ContentJSON), &p); e != nil {
			return p, e
		}
		if p.Active && p.SubjectID == c.Applicant && p.OrgID == c.OrgID {
			if found != nil {
				return p, domain.Fail("EMPLOYEE_PROFILE_CONFLICT", "人员费用资料存在多个有效版本", 422)
			}
			copy := p
			found = &copy
		}
	}
	if found == nil {
		return domain.EmployeeProfile{}, domain.Fail("EMPLOYEE_PROFILE_MISSING", "请先配置申请人的组织、费用职级和业务线路", 422)
	}
	return *found, nil
}
