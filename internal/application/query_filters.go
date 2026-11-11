package application

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type queryFilters struct {
	ExceptionOnly                                  bool
	Keyword, Applicant, Project, Status, ClaimType string
	RouteType                                      string
	InvoiceType, VerificationStatus, ReviewStatus  string
	Statuses                                       []string
	From, Until                                    time.Time
}

// parseQueryFilters 校验有限列表条件，拒绝静默忽略的非法日期和状态。
// 参数：params 为客户端条件，kind 为单据、任务或票据列表类别。
// 返回值：规范筛选条件或公开的422错误。
func parseQueryFilters(params map[string]string, kind string) (queryFilters, error) {
	f := queryFilters{
		Keyword: strings.TrimSpace(params["q"]), Applicant: params["applicant_subject_id"],
		Project: params["project_id"], Status: params["status"], ClaimType: params["claim_type"],
		InvoiceType: params["invoice_type"], VerificationStatus: params["verification_status"],
		ReviewStatus: params["review_status"], RouteType: params["route_type"],
	}
	if value := params["exception_only"]; value != "" {
		if value != "true" && value != "false" {
			return f, domain.Fail("INVALID_QUERY_FILTER", "exception_only须为true或false", 422)
		}
		f.ExceptionOnly = value == "true"
	}
	for key, limit := range map[string]int{"q": 128, "applicant_subject_id": 128, "project_id": 64, "invoice_type": 64} {
		if utf8.RuneCountInString(params[key]) > limit {
			return f, domain.Fail("INVALID_QUERY_FILTER", "查询字段过长："+key, 422)
		}
	}
	enums := map[string]string{
		"route_type":          "TECHNICAL SALES",
		"claim_type":          "TRAVEL DAILY PROJECT_EXECUTION",
		"verification_status": "NOT_CHECKED MANUAL_REVIEWED VERIFIED MISMATCH UNAVAILABLE",
		"review_status":       "PENDING CONFIRMED DISPUTED",
	}
	// 单据状态允许逗号分隔多值，单值与任务、票据枚举语义保持不变。
	if kind == "claims" {
		statuses, err := claimStatuses(f.Status)
		if err != nil {
			return f, err
		}
		f.Statuses, f.Status = statuses, ""
	}
	if kind == "tasks" {
		enums["status"] = "WAITING PENDING APPROVED RETURNED TRANSFERRED CANCELLED"
	}
	if kind == "invoices" {
		enums["status"] = "AVAILABLE RESERVED CONSUMED MERGED"
	}
	for key, values := range enums {
		value := params[key]
		valid := value == ""
		for _, allowed := range strings.Fields(values) {
			valid = valid || value == allowed
		}
		if !valid {
			return f, domain.Fail("INVALID_QUERY_FILTER", "查询状态或类型不合法："+key, 422)
		}
	}
	if e := parseQueryDates(params, &f); e != nil {
		return f, e
	}
	return f, nil
}

// claimStatuses 将单据状态按逗号拆分为受控多值，空段和非法值一律拒绝。
// 参数：value 为客户端原值，可为空、单值或逗号分隔的多个状态。
// 返回值：去重后的合法状态；空值表示不过滤，非法时返回422。
func claimStatuses(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	allowed := map[string]bool{
		"DRAFT": true, "RETURNED": true, "IN_APPROVAL": true,
		"APPROVED_PENDING_PAYMENT": true, "PAID": true, "VOID": true,
	}
	statuses := make([]string, 0, len(allowed))
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		status := strings.TrimSpace(part)
		if !allowed[status] {
			return nil, domain.Fail("INVALID_QUERY_FILTER", "查询状态或类型不合法：status", 422)
		}
		if !seen[status] {
			seen[status] = true
			statuses = append(statuses, status)
		}
	}
	return statuses, nil
}

// parseQueryDates 按北京日解析开始与包含结束日，并校验先后顺序。
// 参数：params 为查询字段，f 接收含开始与不含结束边界。
// 返回值：日期格式、有效范围或顺序不合法时返回422。
func parseQueryDates(params map[string]string, f *queryFilters) error {
	var err error
	for key, target := range map[string]*time.Time{"date_from": &f.From, "date_to": &f.Until} {
		if value := params[key]; value != "" {
			*target, err = time.ParseInLocation("2006-01-02", value, time.FixedZone("Asia/Shanghai", 8*3600))
			if err != nil || target.Year() < 1000 || target.Year() > 9998 {
				return domain.Fail("INVALID_QUERY_DATE", "查询日期须为有效的YYYY-MM-DD："+key, 422)
			}
		}
	}
	if !f.From.IsZero() && !f.Until.IsZero() && f.From.After(f.Until) {
		return domain.Fail("INVALID_QUERY_DATE", "查询开始日期不能晚于结束日期", 422)
	}
	if !f.Until.IsZero() {
		f.Until = f.Until.AddDate(0, 0, 1)
	}
	return nil
}

// queryJSONText 为受控字段生成两种数据库一致的JSON文本读取表达式。
// 参数：db 为当前方言，column 为服务端列名，field 为服务端固定字段名。
// 返回值：可组合的SQL表达式，不包含客户端输入。
func queryJSONText(db *gorm.DB, column, field string) string {
	expr := "JSON_EXTRACT(" + column + ", '$." + field + "')"
	if db.Dialector.Name() == "mysql" {
		return "JSON_UNQUOTE(" + expr + ")"
	}
	return expr
}

// queryLikePattern 将用户关键词按字面量匹配，防止百分号扩大扫描结果。
// 参数：value 为已校验关键词。
// 返回值：使用感叹号转义的包含匹配模式。
func queryLikePattern(value string) string {
	return "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(value) + "%"
}

// applyClaimFilters 将合法条件叠加到已经受租户和对象限制的查询。
// 参数：db 为基础库，q 为受控单据查询，a 为身份，f 为合法条件。
// 返回值：筛选后的单据查询或范围读取错误。
func applyClaimFilters(db, q *gorm.DB, a domain.Actor, f queryFilters) (*gorm.DB, error) {
	if f.Keyword != "" {
		number := queryJSONText(db, "content_json", "claim_no")
		title := queryJSONText(db, "content_json", "title")
		pattern := queryLikePattern(f.Keyword)
		q = q.Where("("+number+" LIKE ? ESCAPE '!' OR "+title+" LIKE ? ESCAPE '!')", pattern, pattern)
	}
	if f.Applicant != "" {
		q = q.Where("applicant = ?", f.Applicant)
	}
	if len(f.Statuses) > 0 {
		q = q.Where("status IN ?", f.Statuses)
	} else if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.ClaimType != "" {
		q = q.Where(queryJSONText(db, "content_json", "claim_type")+" = ?", f.ClaimType)
	}
	if f.RouteType != "" {
		q = q.Where(queryJSONText(db, "content_json", "route_type")+" = ?", f.RouteType)
	}
	if !f.From.IsZero() {
		q = q.Where("created_at >= ?", f.From.UTC())
	}
	if !f.Until.IsZero() {
		q = q.Where("created_at < ?", f.Until.UTC())
	}
	if f.Project != "" {
		refs := db.Model(&store.ClaimReference{}).Select("claim_id").Where("tenant_id = ? AND project_id = ?", a.TenantID, f.Project)
		full, e := readableClaims(db, a, false)
		if e != nil {
			return nil, e
		}
		fullIDs := full.Select("id")
		// 非整单读者只能按本人任务覆盖的分摊检索，避免旁路推知其他项目。
		expr := queryTaskAllocationMatch(db)
		tasks := db.Table("exp_approval_task AS query_task").Select("query_task.claim_id").Where("query_task.tenant_id = ? AND query_task.assignee = ?", a.TenantID, a.SubjectID).Where(expr)
		refs = refs.Where(db.Where("claim_id IN (?)", fullIDs).Or("claim_id IN (?)", tasks))
		q = q.Where("id IN (?)", refs)
	}
	return q, nil
}

// queryTaskAllocationMatch 将引用与任务分摊键逐项匹配，不读取无权分摊。
// 参数：db 为当前SQL方言。
// 返回值：引用子查询中的相关匹配表达式。
func queryTaskAllocationMatch(db *gorm.DB) string {
	key := "exp_claim_reference.item_id || '/' || exp_claim_reference.project_id || '/' || exp_claim_reference.service_item_id"
	if db.Dialector.Name() == "mysql" {
		key = "CONCAT(exp_claim_reference.item_id, '/', exp_claim_reference.project_id, '/', exp_claim_reference.service_item_id)"
		return "query_task.claim_id = exp_claim_reference.claim_id AND (" +
			"JSON_CONTAINS(query_task.content_json, JSON_QUOTE(" + key + "), '$.allocation_keys') OR " +
			"JSON_CONTAINS(query_task.content_json, JSON_QUOTE('*'), '$.allocation_keys'))"
	}
	return "query_task.claim_id = exp_claim_reference.claim_id AND EXISTS (" +
		"SELECT 1 FROM json_each(query_task.content_json, '$.allocation_keys') " +
		"WHERE value = '*' OR value = " + key + ")"
}

// applyInvoiceFilters 在已授权票据范围内按票面字段与日期筛选。
// 参数：db 为基础库，q 为受控票据查询，f 为合法条件。
// 返回值：筛选后的查询。
func applyInvoiceFilters(db, q *gorm.DB, f queryFilters) *gorm.DB {
	if f.Keyword != "" {
		q = q.Where(queryJSONText(db, "content_json", "invoice_number")+" LIKE ? ESCAPE '!'", queryLikePattern(f.Keyword))
	}
	for field, value := range map[string]string{"invoice_type": f.InvoiceType, "verification_status": f.VerificationStatus, "review_status": f.ReviewStatus} {
		if value != "" {
			q = q.Where(queryJSONText(db, "content_json", field)+" = ?", value)
		}
	}
	if f.ExceptionOnly {
		review := queryJSONText(db, "content_json", "review_status")
		verification := queryJSONText(db, "content_json", "verification_status")
		risks := db.Model(&store.InvoiceRisk{}).Select("invoice_id").Where("tenant_id = exp_invoice.tenant_id AND status = ?", "OPEN")
		q = q.Where("(COALESCE("+review+", '') <> ? OR "+verification+" = ? OR id IN (?))", "CONFIRMED", "MISMATCH", risks)
	}
	if f.Applicant != "" {
		q = q.Where("owner = ?", f.Applicant)
	}
	if f.Status != "" {
		q = q.Where("reservation_state = ?", f.Status)
	}
	date := queryJSONText(db, "content_json", "invoice_date")
	if !f.From.IsZero() {
		q = q.Where(date+" >= ?", f.From.Format("2006-01-02"))
	}
	if !f.Until.IsZero() {
		q = q.Where(date+" < ?", f.Until.Format("2006-01-02"))
	}
	return q
}
