package domain

// InvoicePolicy 组合身份规则及独立费用建议、日期控制，不代表税务验真。
type InvoicePolicy struct {
	InvoiceRule
	ContentMappings   []InvoiceContentMapping `json:"content_mappings,omitempty"`
	DeductionPriority string                  `json:"deduction_priority,omitempty"`
	FeeType           string                  `json:"fee_type,omitempty"`
	DefaultClaimType  string                  `json:"default_claim_type,omitempty"`
	MaxInvoiceAgeDays int                     `json:"max_invoice_age_days,omitempty"`
	DatePolicy        string                  `json:"date_policy,omitempty"`
}

// InvoiceDateWarning 保存提交时服务器计算的日期风险及所依据的政策版本。
type InvoiceDateWarning struct {
	InvoiceID           string `json:"invoice_id"`
	RuleID              string `json:"rule_id"`
	RuleVersion         int64  `json:"rule_version"`
	InvoiceDate         string `json:"invoice_date"`
	CheckedBusinessDate string `json:"checked_business_date"`
	MaxInvoiceAgeDays   int    `json:"max_invoice_age_days"`
	AgeDays             int    `json:"age_days"`
	Code                string `json:"code"`
}

// InvoiceExpenseSuggestion 只建议已确认票据的费用归类，仍需人工编辑分摊与确认草稿。
type InvoiceExpenseSuggestion struct {
	ClassificationStatus    string                       `json:"classification_status"`
	ClassificationSource    string                       `json:"classification_source"`
	ClassificationReasons   []string                     `json:"classification_reasons,omitempty"`
	ClassificationMatches   []InvoiceClassificationMatch `json:"classification_matches,omitempty"`
	DeductionPriority       string                       `json:"deduction_priority,omitempty"`
	DeductionReviewRequired bool                         `json:"deduction_review_required,omitempty"`
	InvoiceID               string                       `json:"invoice_id"`
	InvoiceVersion          int64                        `json:"invoice_version"`
	RuleID                  string                       `json:"rule_id"`
	RuleVersion             int64                        `json:"rule_version"`
	FeeType                 string                       `json:"fee_type"`
	DefaultClaimType        string                       `json:"default_claim_type,omitempty"`
	OccurredDate            string                       `json:"occurred_date"`
	Requested               Money                        `json:"requested_minor"`
}
