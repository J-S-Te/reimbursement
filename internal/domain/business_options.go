package domain

// OrganizationOption 只携带当前任职组织的安全展示信息。
type OrganizationOption struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrimary bool   `json:"is_primary"`
}

// PersonOption 保留提交所需身份引用，不暴露账号、联系方式或角色清单。
type PersonOption struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Organizations []OrganizationOption `json:"organizations"`
}

// PersonOptionsPage 是平台目录安全资料的有界分页，不直接作为业务授权结果。
type PersonOptionsPage struct {
	Items []PersonOption `json:"items"`
	Total int64          `json:"total"`
}

// NameOption 用于已完成业务范围校验的显示名选择，不披露其它任职。
type NameOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DraftContext 表示任职组织与已发布费用资料共同决定的只读草稿上下文。
type DraftContext struct {
	Status            string `json:"status"`
	OrgID             string `json:"org_id,omitempty"`
	RouteType         string `json:"route_type,omitempty"`
	EmployeeProfileID string `json:"employee_profile_id,omitempty"`
	Message           string `json:"message,omitempty"`
}
