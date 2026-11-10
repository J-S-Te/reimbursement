package domain

// InvoiceContent 保存人工确认的票面商品服务明细；解析候选须经本人复核才可写入。
type InvoiceContent struct {
	Lines []InvoiceContentLine `json:"lines"`
}

// InvoiceContentLine 是票面一项商品服务的确认事实，金额以分表达且允许缺省。
type InvoiceContentLine struct {
	Name                  string `json:"name"`
	TaxClassificationCode string `json:"tax_classification_code,omitempty"`
	AmountMinor           *Money `json:"amount_minor,omitempty"`
	TaxMinor              *Money `json:"tax_minor,omitempty"`
}

// InvoiceContentMapping 以字面词或税码前缀提供确定性归类，数值较大的优先级优先。
type InvoiceContentMapping struct {
	ID               string   `json:"id"`
	TaxCodePrefixes  []string `json:"tax_code_prefixes,omitempty"`
	Keywords         []string `json:"keywords,omitempty"`
	FeeType          string   `json:"fee_type"`
	DefaultClaimType string   `json:"default_claim_type,omitempty"`
	Priority         int      `json:"priority"`
}

// InvoiceClassificationMatch 记录规则与人工确认明细的实际命中，明细序号从零开始。
type InvoiceClassificationMatch struct {
	MappingID             string `json:"mapping_id"`
	LineIndex             int    `json:"line_index"`
	LineName              string `json:"line_name"`
	TaxClassificationCode string `json:"tax_classification_code,omitempty"`
	MatchedBy             string `json:"matched_by"`
	MatchedValue          string `json:"matched_value"`
	FeeType               string `json:"fee_type"`
	DefaultClaimType      string `json:"default_claim_type,omitempty"`
	ContentOrigin         string `json:"content_origin"`
}

// InvoiceClassificationEvidence 是草稿保存时服务器重建的归类建议及人工选值证据。
type InvoiceClassificationEvidence struct {
	InvoiceID        string                       `json:"invoice_id"`
	InvoiceVersion   int64                        `json:"invoice_version"`
	RuleID           string                       `json:"rule_id"`
	RuleVersion      int64                        `json:"rule_version"`
	Status           string                       `json:"status"`
	Source           string                       `json:"source"`
	Reasons          []string                     `json:"reasons,omitempty"`
	Matches          []InvoiceClassificationMatch `json:"matches,omitempty"`
	SuggestedFeeType string                       `json:"suggested_fee_type,omitempty"`
	SelectedFeeType  string                       `json:"selected_fee_type"`
	ManualOverride   bool                         `json:"manual_override"`
}
