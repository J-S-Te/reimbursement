package application

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
)

var invoiceTaxCode = regexp.MustCompile(`^[0-9]{1,19}$`)

// validateInvoiceContent 限制人工确认明细的数量、字面文本、税码和可选金额。
// 参数：content 为本人核对原件后填写的票面内容，空值兼容历史票据。
// 返回值：非法或超限明细返回422，合法内容返回空。
func validateInvoiceContent(content *domain.InvoiceContent) error {
	if content == nil {
		return nil
	}
	invalid := domain.Fail("INVALID_INVOICE_CONTENT", "票面最多100项明细，名称限256字，税码须为1至19位数字，金额不得为负", 422)
	if len(content.Lines) > 100 {
		return invalid
	}
	for _, line := range content.Lines {
		if !validInvoiceLiteral(line.Name, 256) ||
			(line.TaxClassificationCode != "" && !invoiceTaxCode.MatchString(line.TaxClassificationCode)) ||
			(line.AmountMinor != nil && *line.AmountMinor < 0) ||
			(line.TaxMinor != nil && *line.TaxMinor < 0) {
			return invalid
		}
	}
	return nil
}

// validInvoiceLiteral 检查有界非空字面文本，禁止控制字符及不可见首尾空白。
// 参数：value 为待确认文本，limit 为允许的最大Unicode字符数。
// 返回值：满足文本边界时为真。
func validInvoiceLiteral(value string, limit int) bool {
	if value == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value || utf8.RuneCountInString(value) > limit {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

// validateInvoiceContentMappings 在保存和读取政策时拒绝无界、重复或不确定的归类配置。
// 参数：mappings 为当前票种下独立规则，旧配置允许为空。
// 返回值：非法规则返回配置错误，合法配置返回空。
func validateInvoiceContentMappings(mappings []domain.InvoiceContentMapping) error {
	invalid := domain.Fail("INVALID_CONFIGURATION", "内容归类规则重复、超限或字段非法，请检查税码前缀、关键词、费用编码与优先级", 422)
	if len(mappings) > 100 {
		return invalid
	}
	seen := map[string]bool{}
	for _, rule := range mappings {
		validType := rule.DefaultClaimType == "" || rule.DefaultClaimType == "TRAVEL" || rule.DefaultClaimType == "DAILY" || rule.DefaultClaimType == "PROJECT_EXECUTION"
		if !invoiceFeeCode.MatchString(rule.ID) || seen[rule.ID] || !invoiceFeeCode.MatchString(rule.FeeType) || !validType || rule.Priority < 0 || rule.Priority > 10000 ||
			len(rule.TaxCodePrefixes) > 20 || len(rule.Keywords) > 20 || len(rule.TaxCodePrefixes)+len(rule.Keywords) == 0 {
			return invalid
		}
		seen[rule.ID] = true
		words := map[string]bool{}
		for _, prefix := range rule.TaxCodePrefixes {
			if !invoiceTaxCode.MatchString(prefix) || words["tax:"+prefix] {
				return invalid
			}
			words["tax:"+prefix] = true
		}
		for _, keyword := range rule.Keywords {
			if !validInvoiceLiteral(keyword, 128) || words["word:"+keyword] {
				return invalid
			}
			words["word:"+keyword] = true
		}
	}
	return nil
}

// sameInvoiceContent 比较人工确认内容，防止同票来源归并静默替换既有依据。
// 参数：left/right 为原确认明细与拟确认明细。
// 返回值：二者语义一致时为真，空对象与无明细均视为历史空值。
func sameInvoiceContent(left, right *domain.InvoiceContent) bool {
	if left == nil {
		left = &domain.InvoiceContent{}
	}
	if right == nil {
		right = &domain.InvoiceContent{}
	}
	a, _ := json.Marshal(left.Lines)
	b, _ := json.Marshal(right.Lines)
	if len(left.Lines) == 0 && len(right.Lines) == 0 {
		return true
	}
	return string(a) == string(b)
}

// applyInvoiceContentClassification 用人工确认明细提供建议，歧义时清空自动类别而保留证据。
// 参数：suggestion 为票据及政策版本已绑定的结果，content 为确认内容，rule 为发布政策。
// 返回值：无；建议状态、来源、命中和人工复核原因写回结果。
func applyInvoiceContentClassification(suggestion *domain.InvoiceExpenseSuggestion, content *domain.InvoiceContent, rule domain.InvoicePolicy) {
	suggestion.ClassificationStatus = "MATCHED"
	suggestion.ClassificationSource = "INVOICE_TYPE_DEFAULT"
	if content == nil || len(content.Lines) == 0 || len(rule.ContentMappings) == 0 {
		return
	}
	unmatched := false
	categories := map[string]bool{}
	for index, line := range content.Lines {
		matches := invoiceContentLineMatches(line, index, rule.ContentMappings)
		if len(matches) == 0 {
			unmatched = true
			continue
		}
		if len(matches) > 1 {
			appendInvoiceClassificationReason(suggestion, "RULE_CONFLICT")
		}
		for _, match := range matches {
			categories[match.FeeType+"\x00"+match.DefaultClaimType] = true
			if len(suggestion.ClassificationMatches) < 1000 {
				suggestion.ClassificationMatches = append(suggestion.ClassificationMatches, match)
			} else {
				appendInvoiceClassificationReason(suggestion, "MATCH_LIMIT")
			}
		}
	}
	if len(suggestion.ClassificationMatches) == 0 {
		return
	}
	suggestion.ClassificationSource = "CONTENT"
	if unmatched {
		appendInvoiceClassificationReason(suggestion, "UNMATCHED_LINES")
	}
	if len(categories) > 1 {
		appendInvoiceClassificationReason(suggestion, "MIXED_CATEGORIES")
	}
	if len(suggestion.ClassificationReasons) > 0 {
		suggestion.ClassificationStatus = "NEEDS_REVIEW"
		suggestion.FeeType, suggestion.DefaultClaimType = "", ""
		return
	}
	match := suggestion.ClassificationMatches[0]
	suggestion.FeeType, suggestion.DefaultClaimType = match.FeeType, match.DefaultClaimType
}

// appendInvoiceClassificationReason 保持人工复核原因去重且不随明细数量增长。
// 参数：suggestion 为当前建议，reason 为服务端固定原因码。
// 返回值：无。
func appendInvoiceClassificationReason(suggestion *domain.InvoiceExpenseSuggestion, reason string) {
	for _, existing := range suggestion.ClassificationReasons {
		if existing == reason {
			return
		}
	}
	suggestion.ClassificationReasons = append(suggestion.ClassificationReasons, reason)
}

// invoiceContentLineMatches 保留单条明细的所有最高优先级规则，不静默选择同级第一条。
// 参数：line 为人工确认内容，index 为零起始序号，mappings 为已验证发布规则。
// 返回值：至多100条有来源的命中，同规则税码优先于关键词展示。
func invoiceContentLineMatches(line domain.InvoiceContentLine, index int, mappings []domain.InvoiceContentMapping) []domain.InvoiceClassificationMatch {
	matches := []domain.InvoiceClassificationMatch{}
	priority := -1
	for _, rule := range mappings {
		by, value := invoiceContentRuleMatch(line, rule)
		if by == "" || rule.Priority < priority {
			continue
		}
		if rule.Priority > priority {
			matches = nil
			priority = rule.Priority
		}
		matches = append(matches, domain.InvoiceClassificationMatch{
			MappingID: rule.ID, LineIndex: index, LineName: line.Name,
			TaxClassificationCode: line.TaxClassificationCode, MatchedBy: by, MatchedValue: value,
			FeeType: rule.FeeType, DefaultClaimType: rule.DefaultClaimType, ContentOrigin: "HUMAN_CONFIRMED",
		})
	}
	return matches
}

// invoiceContentRuleMatch 按纯字面税码前缀或名称关键词判断一条规则。
// 参数：line 为确认明细，rule 为已验证归类规则。
// 返回值：命中的依据种类及原始配置值；不命中时均为空。
func invoiceContentRuleMatch(line domain.InvoiceContentLine, rule domain.InvoiceContentMapping) (string, string) {
	for _, prefix := range rule.TaxCodePrefixes {
		if strings.HasPrefix(line.TaxClassificationCode, prefix) {
			return "TAX_CODE_PREFIX", prefix
		}
	}
	for _, keyword := range rule.Keywords {
		if strings.Contains(line.Name, keyword) {
			return "KEYWORD", keyword
		}
	}
	return "", ""
}
