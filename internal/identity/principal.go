package identity

import (
	"github.com/j-s-te/reimbursement/internal/domain"
	"sort"
)

type DataScope struct {
	RoleCode        string `json:"role_code"`
	ScopeType       string `json:"scope_type"`
	ScopeID         string `json:"scope_id"`
	EnvironmentCode string `json:"environment_code"`
}

type Principal struct {
	Subject               string
	TenantID              string
	IdentityID            string
	PersonID              string
	UserID                string
	Username              string
	DisplayName           string
	Roles                 []string
	Permissions           map[string]bool
	DataScopes            []DataScope
	AuthorizationRevision uint64
	CatalogVersion        string
}

// Has 检查当前有效授权快照的动作权限。
// 参数：permission 为完整报销权限码。
// 返回值：存在有效权限时为真。
func (p Principal) Has(permission string) bool { return p.Permissions[permission] }

// Actor 转换可信身份动作能力，财务数据范围由报销本地发布映射决定。
// 参数：无。
// 返回值：主体及动作权限；应用和平台组织grant不自动授予报销财务范围。
func (p Principal) Actor() domain.Actor {
	permissions := make([]string, 0, len(p.Permissions))
	for code, enabled := range p.Permissions {
		if enabled {
			permissions = append(permissions, code)
		}
	}
	sort.Strings(permissions)
	return domain.Actor{TenantID: p.TenantID, SubjectID: p.IdentityID, Permissions: permissions}
}
