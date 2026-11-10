package invoiceparser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	hardMaxBytes         = 20 << 20
	hardMaxExpandedBytes = 100 << 20
	hardMaxPages         = 100
	rulesVersion         = "expense-invoice-v3"
	normalizationVersion = "invoice-text-nfwidth-v1"
)

type Limits struct {
	MaxBytes         int
	MaxExpandedBytes uint64
	MaxPages         int
}
type Parser struct {
	limits             Limits
	configurationError error
}
type Source struct {
	Page    int     `json:"page,omitempty"`
	X       float64 `json:"x,omitempty"`
	Y       float64 `json:"y,omitempty"`
	Width   float64 `json:"width,omitempty"`
	Height  float64 `json:"height,omitempty"`
	Text    string  `json:"text,omitempty"`
	XMLPath string  `json:"xml_path,omitempty"`
	Field   string  `json:"field,omitempty"`
	Region  string  `json:"region,omitempty"`
	Method  string  `json:"method,omitempty"`
}
type FieldValue struct {
	Value    string   `json:"value"`
	RawValue string   `json:"raw_value"`
	Sources  []Source `json:"sources"`
}
type PageInfo struct {
	Number           int     `json:"number"`
	Width            float64 `json:"width"`
	Height           float64 `json:"height"`
	Rotation         int     `json:"rotation"`
	GlyphCount       int     `json:"glyph_count"`
	CoordinateSystem string  `json:"coordinate_system"`
}
type DetailLine struct {
	FieldCandidates map[string][]FieldValue `json:"field_candidates,omitempty"`
	Name            string                  `json:"name"`
	Model           string                  `json:"model,omitempty"`
	Unit            string                  `json:"unit,omitempty"`
	Quantity        string                  `json:"quantity,omitempty"`
	UnitPrice       string                  `json:"unit_price,omitempty"`
	AmountMinor     string                  `json:"amount_minor,omitempty"`
	TaxRate         string                  `json:"tax_rate,omitempty"`
	TaxMinor        string                  `json:"tax_minor,omitempty"`
	Sources         []Source                `json:"sources"`
	Issues          []string                `json:"issues"`
}
type Candidate struct {
	InvoiceType        string                  `json:"invoice_type"`
	Code               string                  `json:"invoice_code"`
	Number             string                  `json:"invoice_number"`
	Date               string                  `json:"invoice_date"`
	BuyerName          string                  `json:"buyer_name"`
	BuyerTaxID         string                  `json:"buyer_tax_id"`
	SellerName         string                  `json:"seller_name"`
	SellerTaxID        string                  `json:"seller_tax_id"`
	GrossMinor         string                  `json:"gross_minor"`
	AmountMinor        string                  `json:"amount_minor,omitempty"`
	TaxMinor           string                  `json:"tax_minor,omitempty"`
	Raw                json.RawMessage         `json:"raw_fields"`
	Sources            []Source                `json:"sources"`
	Issues             []string                `json:"issues"`
	FieldCandidates    map[string][]FieldValue `json:"field_candidates,omitempty"`
	DetailLines        []DetailLine            `json:"detail_lines,omitempty"`
	Pages              []PageInfo              `json:"pages,omitempty"`
	GroupKind          string                  `json:"group_kind,omitempty"`
	SourcePages        []int                   `json:"source_pages,omitempty"`
	ReviewStatus       string                  `json:"review_status"`
	VerificationStatus string                  `json:"verification_status"`
}
type Result struct {
	Status               string      `json:"status"`
	ParserVersion        string      `json:"parser_version"`
	RulesVersion         string      `json:"rules_version"`
	NormalizationVersion string      `json:"normalization_version,omitempty"`
	Documents            []Candidate `json:"documents"`
	Warnings             []string    `json:"warnings"`
}

// New 创建有明确输入及展开硬上限的票据解析器。
// 参数：limits为显式限制；零值读取REIMBURSEMENT_PARSER_MAX_BYTES/MAX_EXPANDED_BYTES/MAX_PAGES。
// 返回值：默认20MiB/100MiB/100页；非法或扩大硬上限的配置会使Parse失败关闭。
func New(limits Limits) *Parser {
	p := &Parser{limits: limits}
	values := []struct {
		name    string
		current uint64
		hard    uint64
		set     func(uint64)
	}{
		{"MAX_BYTES", uint64(max(limits.MaxBytes, 0)), hardMaxBytes, func(v uint64) { p.limits.MaxBytes = int(v) }},
		{"MAX_EXPANDED_BYTES", limits.MaxExpandedBytes, hardMaxExpandedBytes, func(v uint64) { p.limits.MaxExpandedBytes = v }},
		{"MAX_PAGES", uint64(max(limits.MaxPages, 0)), hardMaxPages, func(v uint64) { p.limits.MaxPages = int(v) }},
	}
	for _, item := range values {
		value := item.current
		if value == 0 {
			raw := os.Getenv("REIMBURSEMENT_PARSER_" + item.name)
			if raw != "" {
				parsed, err := strconv.ParseUint(raw, 10, 64)
				if err != nil || parsed == 0 {
					p.configurationError = errors.New("PARSER_LIMIT_CONFIGURATION_INVALID")
				}
				value = parsed
			} else {
				value = item.hard
			}
		}
		if value > item.hard {
			p.configurationError = errors.New("PARSER_LIMIT_CONFIGURATION_INVALID")
		}
		item.set(value)
	}
	if limits.MaxBytes < 0 || limits.MaxPages < 0 {
		p.configurationError = errors.New("PARSER_LIMIT_CONFIGURATION_INVALID")
	}
	return p
}

// Minor 将票面十进制元精确转换为整数分，拒绝错误千分位与隐式舍入。
// 参数：raw为可带合法千分位与人民币符号的票面金额。
// 返回值：有符号整数分；格式、精度或范围不符返回错误。
func Minor(raw string) (int64, error) {
	value := strings.NewReplacer("￥", "", "¥", "", " ", "").Replace(normalizeText(raw))
	if !regexp.MustCompile(`^-?(?:[0-9]+|[0-9]{1,3}(?:,[0-9]{3})+)(?:\.[0-9]{1,2})?$`).MatchString(value) {
		return 0, errors.New("INVALID_MONEY")
	}
	r, ok := new(big.Rat).SetString(strings.ReplaceAll(value, ",", ""))
	if !ok {
		return 0, errors.New("INVALID_MONEY")
	}
	r.Mul(r, big.NewRat(100, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, errors.New("INVALID_MONEY")
	}
	return r.Num().Int64(), nil
}

// Parse 在格式与资源预检后调用锁定库，不把提取等同于签章或税务查验。
// 参数：ctx为取消边界，data为原件，media为网关核验的预期媒体类型。
// 返回值：始终人工PENDING/NOT_CHECKED的候选；损坏、超限或不支持返回稳定错误。
func (p *Parser) Parse(ctx context.Context, data []byte, media string) (result Result, err error) {
	defer func() {
		if recover() != nil {
			result = Result{}
			err = errors.New("MALFORMED_DOCUMENT")
			return
		}
		if err == nil {
			result, err = boundResultEvidence(result)
		}
	}()
	if p == nil {
		return result, errors.New("PARSER_LIMIT_CONFIGURATION_INVALID")
	}
	if p.configurationError != nil {
		return result, p.configurationError
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if len(data) == 0 || len(data) > p.limits.MaxBytes {
		return result, errors.New("DOCUMENT_SIZE_LIMIT")
	}
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")) && media == "application/pdf":
		return p.parsePDF(ctx, data)
	case bytes.HasPrefix(data, []byte("PK\x03\x04")) && (media == "application/ofd" || media == "application/vnd.ofd"):
		inventory, err := p.inspectOFD(ctx, data)
		if err != nil {
			return result, err
		}
		return p.parseOFD(data, inventory)
	default:
		return result, errors.New("UNSUPPORTED_OR_MISMATCHED_FORMAT")
	}
}
