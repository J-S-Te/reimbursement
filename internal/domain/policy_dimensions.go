package domain

// CityMapping 以组织和精确来源城市文本绑定经人工维护的结构化行政代码。
type CityMapping struct {
	ID         string `json:"id"`
	Version    int64  `json:"version"`
	OrgID      string `json:"org_id"`
	SourceCity string `json:"source_city"`
	CityCode   string `json:"city_code"`
	CityLevel  string `json:"city_level"`
	ValidFrom  string `json:"valid_from"`
	ValidUntil string `json:"valid_until"`
	Active     bool   `json:"active"`
	Published  bool   `json:"published"`
}

// ArchiveRetentionPolicy 显式声明组织档案保留月数与起算规则，不推断法定年限。
type ArchiveRetentionPolicy struct {
	ID              string `json:"id"`
	Version         int64  `json:"version"`
	OrgID           string `json:"org_id"`
	RetentionMonths int    `json:"retention_months"`
	StartBasis      string `json:"start_basis"`
	Hold            bool   `json:"hold"`
	Basis           string `json:"basis"`
	ValidFrom       string `json:"valid_from"`
	ValidUntil      string `json:"valid_until"`
	Published       bool   `json:"published"`
}

// ArchiveRetentionSnapshot 冻结提交时的明确政策版本、保留边界和停止处置标志。
type ArchiveRetentionSnapshot struct {
	Status          string `json:"status"`
	PolicyID        string `json:"policy_id,omitempty"`
	PolicyVersion   int64  `json:"policy_version,omitempty"`
	RetentionMonths int    `json:"retention_months,omitempty"`
	StartBasis      string `json:"start_basis,omitempty"`
	StartDate       string `json:"start_date,omitempty"`
	RetainUntil     string `json:"retain_until,omitempty"`
	Hold            bool   `json:"hold,omitempty"`
	Basis           string `json:"basis,omitempty"`
}

// CityMappingApplication 保留费用城市校验使用的原文、代码及政策版本。
type CityMappingApplication struct {
	PolicyID      string `json:"policy_id,omitempty"`
	PolicyVersion int64  `json:"policy_version,omitempty"`
	SourceCity    string `json:"source_city,omitempty"`
	CityCode      string `json:"city_code,omitempty"`
	CityLevel     string `json:"city_level,omitempty"`
	Status        string `json:"status"`
}
