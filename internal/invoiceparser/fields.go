package invoiceparser

import (
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// normalizeText 使用版本固定的全角、空白与连字规则恢复可比较文字。
// 参数：raw为字形或XML原始文字，不作为已确认业务字段。
// 返回值：保留可读空格的规范文本。
func normalizeText(raw string) string {
	raw = strings.NewReplacer("ﬁ", "fi", "ﬂ", "fl", "ﬀ", "ff", "ﬃ", "ffi", "ﬄ", "ffl", "−", "-", "\u200b", "").Replace(raw)
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r >= 0xff01 && r <= 0xff5e {
			return r - 0xfee0
		}
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, raw))
}

// compactText 去除标签内排版空白供标签识别，原文字仍保留在Source中。
// 参数：raw为已提取文字。
// 返回值：全角归一并去除Unicode空白的标签匹配字符串。
func compactText(raw string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, normalizeText(raw))
}

// newCandidate 建立永不自动确认或验真的候选容器。
// 参数：无。
// 返回值：人工PENDING、税务NOT_CHECKED且携带固定问题的候选。
func newCandidate() Candidate {
	return Candidate{ReviewStatus: "PENDING", VerificationStatus: "NOT_CHECKED", GroupKind: "SINGLE_CANDIDATE", FieldCandidates: map[string][]FieldValue{}, Issues: []string{"MANUAL_CONFIRMATION_REQUIRED"}, Sources: []Source{}, DetailLines: []DetailLine{}}
}

// addIssue 去重追加稳定问题码，不静默覆盖已有诊断。
// 参数：c为候选，code为稳定问题码。
// 返回值：无。
func addIssue(c *Candidate, code string) {
	if !slices.Contains(c.Issues, code) {
		c.Issues = append(c.Issues, code)
	}
}

// addField 保留每个不同字段候选以及同值的全部证据来源。
// 参数：c为候选，field为固定字段名，value为规范值，raw为原值，source为实际来源。
// 返回值：无；空值不会被当作有效候选。
func addField(c *Candidate, field, value, raw string, source Source) {
	if value == "" {
		return
	}
	source.Field = field
	values := c.FieldCandidates[field]
	for i := range values {
		if values[i].Value == value {
			values[i].Sources = append(values[i].Sources, source)
			c.FieldCandidates[field] = values
			return
		}
	}
	c.FieldCandidates[field] = append(values, FieldValue{Value: value, RawValue: raw, Sources: []Source{source}})
}

// canonicalDate 规范中国票面日期并拒绝无效日历日期。
// 参数：raw为标签或结构化XML日期文字。
// 返回值：YYYY-MM-DD日期；无法确定时返回空。
func canonicalDate(raw string) string {
	raw = compactText(raw)
	for _, layout := range []string{"2006-01-02", "2006/01/02", "2006年1月2日", "20060102"} {
		if at, err := time.Parse(layout, raw); err == nil {
			return at.Format("2006-01-02")
		}
	}
	return ""
}

// resolveFields 仅将唯一候选提升为单值，冲突全部保留并阻止无证据选取。
// 参数：c为收集完候选的票据。
// 返回值：无；冲突及必要字段缺失作为问题码附加。
func resolveFields(c *Candidate) {
	targets := map[string]*string{
		"invoice_type": &c.InvoiceType, "invoice_number": &c.Number, "invoice_code": &c.Code,
		"invoice_date": &c.Date, "buyer_name": &c.BuyerName, "buyer_tax_id": &c.BuyerTaxID,
		"seller_name": &c.SellerName, "seller_tax_id": &c.SellerTaxID, "issuer": &c.Issuer,
		"amount_minor": &c.AmountMinor, "tax_minor": &c.TaxMinor, "gross_minor": &c.GrossMinor,
	}
	for field, target := range targets {
		values := c.FieldCandidates[field]
		*target = ""
		if len(values) == 1 {
			*target = values[0].Value
		}
		if len(values) > 1 {
			addIssue(c, "FIELD_CONFLICT:"+field)
		}
	}
	for _, field := range []string{"invoice_number", "invoice_date", "gross_minor"} {
		if len(c.FieldCandidates[field]) == 0 {
			addIssue(c, "FIELD_MISSING:"+field)
		}
	}
	validateCandidateAmounts(c)
}

// validateCandidateAmounts 精确验证已知净额税额合计以及可完整复算的明细。
// 参数：c为规范候选；缺失字段不自动视为零。
// 返回值：无；关系不符或明细不完整均保留问题，不修改金额。
func validateCandidateAmounts(c *Candidate) {
	if c.AmountMinor != "" && c.TaxMinor != "" && c.GrossMinor != "" {
		net, _ := new(big.Int).SetString(c.AmountMinor, 10)
		tax, _ := new(big.Int).SetString(c.TaxMinor, 10)
		gross, _ := new(big.Int).SetString(c.GrossMinor, 10)
		if net == nil || tax == nil || gross == nil || new(big.Int).Add(net, tax).Cmp(gross) != 0 {
			addIssue(c, "AMOUNT_CONFLICT")
		}
	}
	if strings.HasPrefix(c.GrossMinor, "-") {
		addIssue(c, "RED_OR_NEGATIVE_INVOICE_REQUIRES_FINANCE_REVIEW")
	}
	if len(c.DetailLines) == 0 {
		addIssue(c, "DETAIL_LINES_REQUIRE_REVIEW")
		return
	}
	net, tax := new(big.Int), new(big.Int)
	complete := true
	for _, line := range c.DetailLines {
		a, okA := new(big.Int).SetString(line.AmountMinor, 10)
		t, okT := new(big.Int).SetString(line.TaxMinor, 10)
		if !okA || !okT {
			complete = false
			continue
		}
		net.Add(net, a)
		tax.Add(tax, t)
	}
	if !complete {
		addIssue(c, "DETAIL_AMOUNTS_INCOMPLETE")
		return
	}
	if c.AmountMinor != "" && net.String() != c.AmountMinor {
		addIssue(c, "DETAIL_NET_TOTAL_CONFLICT")
	}
	if c.TaxMinor != "" && tax.String() != c.TaxMinor {
		addIssue(c, "DETAIL_TAX_TOTAL_CONFLICT")
	}
}

// addMoneyField 精确归一票面金额并保留非法精度问题。
// 参数：c为候选，field为金额字段，raw为原值，source为证据。
// 返回值：无；不做隐式四舍五入或把缺失金额填零。
func addMoneyField(c *Candidate, field, raw string, source Source) {
	amount, err := Minor(raw)
	if err != nil {
		addIssue(c, "AMOUNT_PRECISION_OR_FORMAT_INVALID:"+field)
		return
	}
	addField(c, field, strconv.FormatInt(amount, 10), raw, source)
}
