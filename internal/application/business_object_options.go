package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// OwnInvoiceOptions 只返回本人票据的业务标签，财务共享范围不能扩大此选择入口。
// 参数：ctx/a 为可信身份；keyword 为号码、日期或票据代码；page/size 为分页。
// 返回值：票据内部引用和业务名称分页；无读取能力或搜索非法时拒绝。
func (s *Service) OwnInvoiceOptions(ctx context.Context, a domain.Actor, keyword string, page, size int) (json.RawMessage, error) {
	if err := require(a, "expense.invoice.read"); err != nil {
		return nil, err
	}
	keyword, err := objectOptionKeyword(keyword)
	if err != nil {
		return nil, err
	}
	db := s.db.WithContext(ctx)
	q := db.Model(&store.Invoice{}).Where("tenant_id = ? AND owner = ?", a.TenantID, a.SubjectID)
	if keyword != "" {
		conditions := []string{}
		args := []any{}
		for _, field := range []string{"invoice_number", "invoice_date", "invoice_code"} {
			conditions = append(conditions, queryJSONText(db, "content_json", field)+" LIKE ? ESCAPE '!'")
			args = append(args, queryLikePattern(keyword))
		}
		q = q.Where("("+strings.Join(conditions, " OR ")+")", args...)
	}
	var total int64
	if err = q.Count(&total).Error; err != nil {
		return nil, err
	}
	page, size, offset := pageArgs(map[string]string{"page": strconv.Itoa(page), "page_size": strconv.Itoa(size)})
	if size > 50 {
		size = 50
		offset = (page - 1) * size
	}
	var rows []store.Invoice
	if err = q.Order("created_at DESC,id DESC").Limit(size).Offset(offset).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := []domain.NameOption{}
	for _, row := range rows {
		var invoice domain.Invoice
		if err = json.Unmarshal([]byte(row.ContentJSON), &invoice); err != nil {
			return nil, err
		}
		name := strings.TrimSpace(invoice.InvoiceNumber)
		if name == "" {
			name = "票号待完善"
		}
		if invoice.InvoiceDate != "" {
			name += " · " + invoice.InvoiceDate
		}
		items = append(items, domain.NameOption{ID: row.ID, Name: name})
	}
	return json.Marshal(map[string]any{"items": items, "total": total, "page": page, "page_size": size})
}

// OrganizationOptions 仅从真实当前任职解析组织名称，政策目标另受明确配置范围限制。
// 参数：ctx/a 为可信身份；subjectID 为空表示本人；keyword/page/size 为名称搜索分页。
// 返回值：真实组织引用和名称分页；普通用户查他人或管理范围不明时拒绝。
func (s *Service) OrganizationOptions(ctx context.Context, a domain.Actor, subjectID, keyword string, page, size int) (json.RawMessage, error) {
	if a.TenantID == "" || a.SubjectID == "" {
		return nil, domain.Fail("SESSION_EXPIRED", "请重新登录", 401)
	}
	keyword, err := objectOptionKeyword(keyword)
	if err != nil {
		return nil, err
	}
	if len(subjectID) > 128 || subjectID != strings.TrimSpace(subjectID) {
		return nil, domain.Fail("OPTIONS_INVALID", "人员选择不合法", 422)
	}
	policy := a.Has("expense.config.manage") && subjectID != ""
	if !policy && subjectID != "" && subjectID != a.SubjectID {
		return nil, domain.Fail("FORBIDDEN", "只能读取本人任职组织", 403)
	}
	if !policy && !a.Has("expense.claim.create") && !a.Has("expense.claim.update") && !a.Has("expense.config.manage") {
		return nil, domain.Fail("FORBIDDEN", "没有草稿或政策管理能力", 403)
	}
	person, allowed, err := s.organizationOptionPerson(ctx, a, subjectID, policy)
	if err != nil {
		return nil, err
	}
	items := []domain.NameOption{}
	for _, org := range person.Organizations {
		if org.ID == "" || org.Name == "" {
			return nil, domain.Fail("PERSONNEL_UNAVAILABLE", "任职组织资料不完整", 503)
		}
		if allowed != nil && !allowed[org.ID] {
			continue
		}
		if strings.Contains(strings.ToLower(org.Name), strings.ToLower(keyword)) {
			items = append(items, domain.NameOption{ID: org.ID, Name: org.Name})
		}
	}
	return nameOptionsPage(items, page, size)
}

// organizationOptionPerson 按本人或政策目标精确读取任职并生成允许的组织集合。
// 参数：ctx/a 为可信身份；subject 为精确目标；policy 表示政策选择用途。
// 返回值：真实任职资料及可选范围，nil范围仅用于本人或明确全量授权。
func (s *Service) organizationOptionPerson(ctx context.Context, a domain.Actor, subject string, policy bool) (domain.PersonOption, map[string]bool, error) {
	if !policy {
		person, err := s.currentDraftApplicant(ctx, a)
		return person, nil, err
	}
	orgs, all, err := scopeOrganizations(s.db.WithContext(ctx), a)
	if err != nil {
		return domain.PersonOption{}, nil, err
	}
	if !all && len(orgs) == 0 {
		return domain.PersonOption{}, nil, domain.Fail("FORBIDDEN", "政策管理尚未配置明确组织范围", 403)
	}
	directory, ok := s.deps.Personnel.(selectionDirectory)
	if !ok {
		return domain.PersonOption{}, nil, domain.Fail("PERSONNEL_UNAVAILABLE", "人员任职服务未配置", 503)
	}
	person, found, err := directory.LookupPerson(ctx, a, subject, "")
	if err != nil {
		return domain.PersonOption{}, nil, err
	}
	if !found {
		return domain.PersonOption{Organizations: []domain.OrganizationOption{}}, nil, nil
	}
	if person.ID != subject {
		return domain.PersonOption{}, nil, domain.Fail("PERSONNEL_UNAVAILABLE", "人员任职响应不一致", 503)
	}
	if all {
		return person, nil, nil
	}
	allowed := map[string]bool{}
	for _, org := range orgs {
		allowed[org] = true
	}
	return person, allowed, nil
}

// ConfigurationOptions 沿用配置查阅授权，只投影当前已发布启用记录的业务名称。
// 参数：ctx/a 为可信管理身份；kind 为允许配置类别；keyword/page/size 为名称搜索分页。
// 返回值：安全配置引用和名称；不返回外部账户引用或新增平行授权模型。
func (s *Service) ConfigurationOptions(ctx context.Context, a domain.Actor, kind, keyword string, page, size int) (json.RawMessage, error) {
	if a.TenantID == "" || a.SubjectID == "" {
		return nil, domain.Fail("SESSION_EXPIRED", "请重新登录", 401)
	}
	if !a.Has("expense.config.manage") && !a.Has("expense.policy.publish") {
		return nil, domain.Fail("FORBIDDEN", "无配置查阅权限", 403)
	}
	if kind != "finance_entities" && kind != "payee_profiles" && kind != "payment_accounts" {
		return nil, domain.Fail("OPTIONS_KIND_INVALID", "不支持的配置选择类别", 422)
	}
	keyword, err := objectOptionKeyword(keyword)
	if err != nil {
		return nil, err
	}
	rows, err := configs(s.db.WithContext(ctx), a.TenantID, kind, true)
	if err != nil {
		return nil, err
	}
	items := []domain.NameOption{}
	people := map[string]string{}
	for _, row := range rows {
		name, err := s.configurationObjectLabel(ctx, a, row, people)
		if err != nil {
			return nil, err
		}
		if name != "" && strings.Contains(strings.ToLower(name), strings.ToLower(keyword)) {
			items = append(items, domain.NameOption{ID: row.ID, Name: name})
		}
	}
	return nameOptionsPage(items, page, size)
}

// configurationObjectLabel 读取真实业务名，旧收款资料只使用真实人员姓名及资料版本补充。
// 参数：ctx/a 为查询身份；row 为已授权发布配置；people 为本次请求的姓名缓存。
// 返回值：业务名称；停用或无法获得真实名称的记录为空，目录故障失败关闭。
func (s *Service) configurationObjectLabel(ctx context.Context, a domain.Actor, row store.Config, people map[string]string) (string, error) {
	var fields struct {
		Name      string `json:"name"`
		SubjectID string `json:"subject_id"`
		Active    bool   `json:"active"`
	}
	if err := json.Unmarshal([]byte(row.ContentJSON), &fields); err != nil {
		return "", err
	}
	if !fields.Active {
		return "", nil
	}
	if name := strings.TrimSpace(fields.Name); name != "" {
		return name, nil
	}
	if row.Kind != "payee_profiles" || fields.SubjectID == "" {
		return "", nil
	}
	name, exists := people[fields.SubjectID]
	if !exists {
		if len(people) >= 100 {
			return "", domain.Fail("OPTIONS_LIMIT", "未命名收款资料过多，请管理员补全业务名称", 422)
		}
		directory, ok := s.deps.Personnel.(selectionDirectory)
		if !ok {
			return "", domain.Fail("PERSONNEL_UNAVAILABLE", "旧收款资料姓名服务未配置", 503)
		}
		person, found, err := directory.LookupPerson(ctx, a, fields.SubjectID, "")
		if err != nil {
			return "", err
		}
		if found && person.ID == fields.SubjectID {
			name = person.Name
		}
		people[fields.SubjectID] = name
	}
	if name == "" {
		return "", nil
	}
	return fmt.Sprintf("%s · 收款资料（版本%d）", name, row.Version), nil
}

// objectOptionKeyword 限制业务对象名称搜索的长度和空白，不扩展SQL通配语义。
// 参数：value 为客户端关键词。
// 返回值：规范关键词；超过一百字时返回参数错误。
func objectOptionKeyword(value string) (string, error) {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > 100 {
		return "", domain.Fail("OPTIONS_INVALID", "搜索条件过长", 422)
	}
	return value, nil
}
