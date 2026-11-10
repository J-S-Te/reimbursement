package domain

import "encoding/json"

// ArchivedInvoiceControl 固定归档请求时的案件元数据与业务证据，后续决定不覆盖旧包。
type ArchivedInvoiceControl struct {
	Kind    string          `json:"kind"`
	Record  json.RawMessage `json:"record"`
	Content json.RawMessage `json:"content"`
}

// ArchivedInvoiceEvidence 保存不可变来源精确集合和可变案件的请求时快照。
type ArchivedInvoiceEvidence struct {
	SourceIDs []string                 `json:"source_ids"`
	Controls  []ArchivedInvoiceControl `json:"controls"`
}
