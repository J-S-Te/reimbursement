package domain

// TravelDeclaration 为申请人声明的实际行程，不是出差批准或服务器核验结论。
type TravelDeclaration struct {
	OriginCityCode      string `json:"origin_city_code"`
	DestinationCityCode string `json:"destination_city_code"`
	DepartureDate       string `json:"departure_date"`
	ReturnDate          string `json:"return_date"`
}

// InvoiceTravelFacts 为人工复核原件上的行程，HOTEL日期使用入住至退房半开区间。
type InvoiceTravelFacts struct {
	Kind                string `json:"kind"`
	OriginCityCode      string `json:"origin_city_code,omitempty"`
	DestinationCityCode string `json:"destination_city_code"`
	StartDate           string `json:"start_date"`
	EndDate             string `json:"end_date"`
}

// TravelInvoiceCheck 固定人工票面事实、来源与相对声明的核验结论。
type TravelInvoiceCheck struct {
	InvoiceID      string              `json:"invoice_id"`
	InvoiceVersion int64               `json:"invoice_version"`
	SourceKind     string              `json:"source_kind"`
	SourceIDs      []string            `json:"source_ids"`
	FactDigest     string              `json:"fact_digest,omitempty"`
	Facts          *InvoiceTravelFacts `json:"facts,omitempty"`
	Status         string              `json:"status"`
	ReasonCodes    []string            `json:"reason_codes"`
}

// TravelCrossCheck 仅表示已声明行程、当前项目来源及人工票面之间的结构化交叉结果。
type TravelCrossCheck struct {
	PolicyVersion      string               `json:"policy_version"`
	Declaration        *TravelDeclaration   `json:"declaration,omitempty"`
	Status             string               `json:"status"`
	ProjectStatus      string               `json:"project_status"`
	ProjectReasonCodes []string             `json:"project_reason_codes"`
	Invoices           []TravelInvoiceCheck `json:"invoices"`
	ReasonCodes        []string             `json:"reason_codes"`
}
