package application

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

var invoiceFeeCode = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// decodeInvoicePolicy 校验票种附加配置，保留旧规则零值兼容。
// 参数：raw 为保存的原始票种配置 JSON。
// 返回值：含默认日期策略的政策；字段类型或业务范围非法时返回配置错误。
func decodeInvoicePolicy(raw string) (domain.InvoicePolicy, error) {
	var rule domain.InvoicePolicy
	if err := json.Unmarshal([]byte(raw), &rule); err != nil {
		return rule, domain.Fail("INVALID_CONFIGURATION", "发票映射或日期配置格式不合法", 422)
	}
	invalidFee := rule.FeeType != "" && !invoiceFeeCode.MatchString(rule.FeeType)
	invalidType := rule.DefaultClaimType != "" && rule.DefaultClaimType != "TRAVEL" &&
		rule.DefaultClaimType != "DAILY" && rule.DefaultClaimType != "PROJECT_EXECUTION"
	invalidDays := rule.MaxInvoiceAgeDays < 0 || rule.MaxInvoiceAgeDays > 3650
	invalidPolicy := rule.DatePolicy != "" && rule.DatePolicy != "BLOCK" && rule.DatePolicy != "WARN"
	invalidPriority := rule.DeductionPriority != "" && rule.DeductionPriority != "NORMAL" &&
		rule.DeductionPriority != "PRIORITY_REVIEW"
	if invalidFee || invalidType || invalidDays || invalidPolicy || invalidPriority {
		return rule, domain.Fail("INVALID_CONFIGURATION", "发票费用编码、报销类型、日期策略或人工复核优先级不合法", 422)
	}
	if err := validateInvoiceContentMappings(rule.ContentMappings); err != nil {
		return rule, err
	}
	if rule.MaxInvoiceAgeDays > 0 && rule.DatePolicy == "" {
		rule.DatePolicy = "BLOCK"
	}
	return rule, nil
}

// invoiceDateWarning 用可信北京时间检查发票年龄，不采用客户端传入的风险结论。
// 参数：inv 为已确认票据，rule 为发布政策，now 为可信服务端时间。
// 返回值：超期 WARN 的冻结事实；非法、未来日期或 BLOCK 超期返回 422。
func invoiceDateWarning(inv domain.Invoice, rule domain.InvoicePolicy, now time.Time) (*domain.InvoiceDateWarning, error) {
	if rule.MaxInvoiceAgeDays == 0 {
		return nil, nil
	}
	zone := time.FixedZone("Asia/Shanghai", 8*3600)
	day := now.In(zone).Format("2006-01-02")
	date, err := time.ParseInLocation("2006-01-02", inv.InvoiceDate, zone)
	if err != nil || inv.InvoiceDate > day {
		return nil, domain.Fail("INVOICE_DATE_INVALID", "发票日期无效或晚于当前北京业务日", 422)
	}
	today, _ := time.ParseInLocation("2006-01-02", day, zone)
	age := int(today.Sub(date) / (24 * time.Hour))
	if age <= rule.MaxInvoiceAgeDays {
		return nil, nil
	}
	if rule.DatePolicy != "WARN" {
		return nil, domain.Fail("INVOICE_DATE_EXPIRED", "发票已超过当前票种允许的有效天数", 422)
	}
	return &domain.InvoiceDateWarning{
		InvoiceID: inv.ID, RuleID: rule.ID, RuleVersion: rule.Version,
		InvoiceDate: inv.InvoiceDate, CheckedBusinessDate: day,
		MaxInvoiceAgeDays: rule.MaxInvoiceAgeDays, AgeDays: age, Code: "INVOICE_DATE_EXPIRED",
	}, nil
}

// InvoiceExpenseSuggestions 从本人已确认票据与已发布映射生成草稿建议。
// 参数：ctx 为请求上下文，actor 为可信身份，ids 为当前租户票据标识且最多一百张。
// 返回值：按选择顺序生成的建议；越权、未确认、占用、缺映射或日期非法时返回错误。
func (s *Service) InvoiceExpenseSuggestions(ctx context.Context, actor domain.Actor, ids []string) ([]domain.InvoiceExpenseSuggestion, error) {
	if err := require(actor, "expense.invoice.read"); err != nil {
		return nil, err
	}
	if err := require(actor, "expense.claim.create"); err != nil {
		return nil, err
	}
	if actor.TenantID == "" || actor.SubjectID == "" || len(ids) == 0 || len(ids) > 100 {
		return nil, domain.Fail("INVALID_INVOICE_SELECTION", "请选择一至一百张本人已确认发票", 422)
	}
	seen := map[string]bool{}
	result := make([]domain.InvoiceExpenseSuggestion, 0, len(ids))
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, id := range ids {
			if id == "" || seen[id] {
				return domain.Fail("INVALID_INVOICE_SELECTION", "票据选择存在空值或重复标识", 422)
			}
			seen[id] = true
			suggestion, err := invoiceExpenseSuggestion(tx, actor, id)
			if err != nil {
				return err
			}
			result = append(result, suggestion)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// invoiceExpenseSuggestion 读取确认票据并应用唯一发布映射，不改动票据及草稿。
// 参数：tx 为只读业务事务，actor 为可信身份，id 为待建议票据标识。
// 返回值：可信票面和政策来源建议；缺失、不符合状态或缺映射返回业务错误。
func invoiceExpenseSuggestion(tx *gorm.DB, actor domain.Actor, id string) (domain.InvoiceExpenseSuggestion, error) {
	var row store.Invoice
	err := tx.Where("tenant_id = ? AND owner = ? AND id = ?", actor.TenantID, actor.SubjectID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.InvoiceExpenseSuggestion{}, domain.Fail("INVOICE_NOT_AVAILABLE", "发票不存在或无权使用", 422)
	}
	if err != nil {
		return domain.InvoiceExpenseSuggestion{}, err
	}
	var inv domain.Invoice
	if err = json.Unmarshal([]byte(row.ContentJSON), &inv); err != nil {
		return domain.InvoiceExpenseSuggestion{}, err
	}
	if inv.ReviewStatus != "CONFIRMED" || inv.Total <= 0 || row.IdentityKey == nil ||
		inv.VerificationStatus == "MISMATCH" || row.ReservationClaimID != "" ||
		row.ReservationState == "CONSUMED" || row.ReservationState == "RESERVED" {
		return domain.InvoiceExpenseSuggestion{}, domain.Fail("INVOICE_NOT_AVAILABLE", "请选择本人已确认且未占用的有效票据", 422)
	}
	if _, err = time.Parse("2006-01-02", inv.InvoiceDate); err != nil {
		return domain.InvoiceExpenseSuggestion{}, domain.Fail("INVOICE_DATE_INVALID", "票面日期无效，请先人工复核", 422)
	}
	rule, err := invoiceRule(tx, actor.TenantID, inv.InvoiceType)
	if err != nil {
		return domain.InvoiceExpenseSuggestion{}, err
	}
	if rule.FeeType == "" && len(rule.ContentMappings) == 0 {
		return domain.InvoiceExpenseSuggestion{}, domain.Fail("INVOICE_MAPPING_MISSING", "此票种尚未发布费用类别映射", 422)
	}
	if _, err = invoiceDateWarning(inv, rule, time.Now().UTC()); err != nil {
		return domain.InvoiceExpenseSuggestion{}, err
	}
	if err = validateInvoiceContent(inv.Content); err != nil {
		return domain.InvoiceExpenseSuggestion{}, err
	}
	suggestion := domain.InvoiceExpenseSuggestion{
		InvoiceID: row.ID, InvoiceVersion: row.Version, RuleID: rule.ID, RuleVersion: rule.Version,
		FeeType: rule.FeeType, DefaultClaimType: rule.DefaultClaimType,
		OccurredDate: inv.InvoiceDate, Requested: inv.Total,
		DeductionPriority:       rule.DeductionPriority,
		DeductionReviewRequired: rule.DeductionPriority == "PRIORITY_REVIEW",
	}
	applyInvoiceContentClassification(&suggestion, inv.Content, rule)
	if suggestion.FeeType == "" && suggestion.ClassificationStatus != "NEEDS_REVIEW" {
		return suggestion, domain.Fail("INVOICE_MAPPING_MISSING", "此票据内容未命中规则且票种未发布默认费用映射", 422)
	}
	return suggestion, nil
}
