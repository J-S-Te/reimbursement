package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/j-s-te/reimbursement/internal/authorizationrefresh"
)

var (
	ErrAuthorizationDenied      = errors.New("application authorization denied")
	ErrAuthorizationUnavailable = errors.New("authorization service unavailable")
	ErrInvalidAuthorization     = errors.New("authorization context is invalid")
)

type AuthorizationContext struct {
	Subject         string `json:"sub"`
	SubjectID       string `json:"subject_id"`
	IdentityID      string `json:"identity_id"`
	TenantID        string `json:"tenant_id"`
	PersonID        string `json:"person_id"`
	ClientID        string `json:"client_id"`
	ApplicationCode string `json:"application_code"`
	EnvironmentCode string `json:"environment_code"`
	// 平台可选元数据仅用于协议兼容，不参与主体、租户或财务范围推导。
	UserLoginIP                string      `json:"user_login_ip,omitempty"`
	CustomerRef                string      `json:"customer_ref,omitempty"`
	Roles                      []string    `json:"roles"`
	Permissions                []string    `json:"permissions"`
	DataScopes                 []DataScope `json:"data_scopes"`
	CatalogVersion             string      `json:"catalog_version"`
	CompatibleCatalogVersions  []string    `json:"compatible_catalog_versions"`
	RoleConfigHash             string      `json:"role_config_hash"`
	CompatibleRoleConfigHashes []string    `json:"compatible_role_config_hashes"`
	AuthorizationRevision      uint64      `json:"authorization_revision"`
}

type AuthorizationContextClient interface {
	Resolve(context.Context, string) (AuthorizationContext, error)
}

type HTTPAuthorizationContextClient struct {
	endpoint string
	client   *http.Client
}

// NewHTTPAuthorizationContextClient 处理报销身份会话及受控协议边界。
// 参数：platformBaseURL为平台控制面地址；client为有时限的服务端HTTP客户端。
// 返回值：当前业务或协议处理结果。
func NewHTTPAuthorizationContextClient(platformBaseURL string, client *http.Client) *HTTPAuthorizationContextClient {
	return &HTTPAuthorizationContextClient{
		endpoint: strings.TrimRight(strings.TrimSpace(platformBaseURL), "/") + "/oauth2/authorization-context",
		client:   client,
	}
}

// Resolve 在线读取平台授权并传播页面新鲜读取要求。
// 参数：ctx 为请求上下文，accessToken 为服务端持有的访问令牌。
// 返回值：当前授权快照或认证、授权、依赖分类错误。
func (c *HTTPAuthorizationContextClient) Resolve(ctx context.Context, accessToken string) (AuthorizationContext, error) {
	if c == nil || c.client == nil || strings.TrimSpace(c.endpoint) == "/oauth2/authorization-context" || strings.TrimSpace(accessToken) == "" {
		return AuthorizationContext{}, ErrInvalidAuthorization
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return AuthorizationContext{}, fmt.Errorf("%w: create request: %v", ErrAuthorizationUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if authorizationrefresh.Forced(ctx) {
		req.Header.Set(authorizationrefresh.Header, "1")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return AuthorizationContext{}, fmt.Errorf("%w: %v", ErrAuthorizationUnavailable, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return AuthorizationContext{}, ErrUnauthenticated
	case http.StatusForbidden:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return AuthorizationContext{}, ErrAuthorizationDenied
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return AuthorizationContext{}, fmt.Errorf("%w: HTTP %d", ErrAuthorizationUnavailable, resp.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var result AuthorizationContext
	if err := decoder.Decode(&result); err != nil {
		return AuthorizationContext{}, fmt.Errorf("%w: decode response: %v", ErrInvalidAuthorization, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return AuthorizationContext{}, fmt.Errorf("%w: response must contain one JSON object", ErrInvalidAuthorization)
	}
	return result, nil
}
