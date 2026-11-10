package domain

// HistoricalTailPolicy 仅对精确组织和项目的历史费用定义有期限、默认关闭的尾款例外。
type HistoricalTailPolicy struct {
	ID            string `json:"id"`
	Version       int64  `json:"version"`
	OrgID         string `json:"org_id"`
	ProjectID     string `json:"project_id"`
	ExpenseCutoff string `json:"expense_cutoff"`
	ValidFrom     string `json:"valid_from"`
	ValidUntil    string `json:"valid_until"`
	Enabled       bool   `json:"enabled"`
	Published     bool   `json:"published"`
}
type HistoricalTailApplication struct {
	ProjectID          string               `json:"project_id"`
	ServiceItemID      string               `json:"service_item_id,omitempty"`
	ReferenceVersion   string               `json:"reference_version"`
	Policy             HistoricalTailPolicy `json:"policy"`
	TerminationReasons []string             `json:"termination_reasons"`
}
type TravelConfirmation struct {
	Decision        string   `json:"decision"`
	Reason          string   `json:"reason"`
	EvidenceFileIDs []string `json:"evidence_file_ids"`
}
type TravelAssessmentReference struct {
	CrossCheck       *TravelCrossCheck       `json:"cross_check,omitempty"`
	CityMapping      *CityMappingApplication `json:"city_mapping,omitempty"`
	ProjectID        string                  `json:"project_id"`
	ServiceItemID    string                  `json:"service_item_id,omitempty"`
	ReferenceVersion string                  `json:"reference_version"`
	FactDigest       string                  `json:"fact_digest,omitempty"`
	Status           string                  `json:"status"`
	ReasonCodes      []string                `json:"reason_codes"`
}
type TravelAssessment struct {
	CrossCheckStatus string                      `json:"cross_check_status,omitempty"`
	Status           string                      `json:"status"`
	ReasonCodes      []string                    `json:"reason_codes"`
	ConfirmedUnknown bool                        `json:"confirmed_unknown"`
	References       []TravelAssessmentReference `json:"references"`
}
type TravelSource struct {
	EventID       string `json:"event_id"`
	ServiceItemID string `json:"service_item_id"`
	EventVersion  string `json:"event_version"`
	ContentDigest string `json:"content_digest"`
}

// TravelSnapshot 与项目权威事实保持相同字段和顺序，明确准备安排不等同于出差审批。
type TravelSnapshot struct {
	Status             string         `json:"status"`
	Mode               string         `json:"mode"`
	SourceEventID      string         `json:"source_event_id,omitempty"`
	SourceEventVersion string         `json:"source_event_version,omitempty"`
	ContentDigest      string         `json:"content_digest"`
	SourceChain        []TravelSource `json:"source_chain"`
	Origin             string         `json:"origin,omitempty"`
	Destination        string         `json:"destination,omitempty"`
	DepartureDate      string         `json:"departure_date,omitempty"`
	ReturnDate         string         `json:"return_date,omitempty"`
	Transport          string         `json:"transport,omitempty"`
	AccommodationNeed  string         `json:"accommodation_need,omitempty"`
	ActorInTravelers   *bool          `json:"actor_in_travelers"`
	SiteMatchStatus    string         `json:"site_match_status"`
	CityCodeStatus     string         `json:"city_code_status"`
	DateMatchStatus    string         `json:"date_match_status"`
	ReasonCodes        []string       `json:"reason_codes"`
}
