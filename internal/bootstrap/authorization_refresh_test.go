package bootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/reimbursement/internal/identity"
)

type pageSessionStore struct {
	identity.OIDCStore
	record identity.StoredOIDCSession
}

// FindSession 返回测试账号的当前授权记录。
// 参数：调用上下文、Cookie 摘要和当前时间均由生产认证器传入。
// 返回值：隔离会话记录，无错误。
func (s *pageSessionStore) FindSession(context.Context, []byte, time.Time) (identity.StoredOIDCSession, error) {
	return s.record, nil
}

// TouchSessionLastSeen 保持测试会话原始活动窗口。
// 参数：上下文、摘要和观测时间不改变显式活动时间。
// 返回值：无错误。
func (s *pageSessionStore) TouchSessionLastSeen(context.Context, []byte, time.Time) error { return nil }

// TestAuthSessionExposesCurrentDataScopes 验证报销会话响应包含服务端当前真实授权范围。
// 参数：t 为独立 OIDC 发现端点和会话路由的测试上下文。
// 返回值：无；遗漏范围或返回人工推测范围时失败。
func TestAuthSessionExposesCurrentDataScopes(t *testing.T) {
	var issuer string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	defer provider.Close()
	issuer = provider.URL
	now := time.Now().UTC()
	principal := identity.Principal{TenantID: "tenant-1", IdentityID: "identity-1", UserID: "identity-1", Roles: []string{"expense_applicant"}, Permissions: map[string]bool{"expense.claim.read": true}, DataScopes: []identity.DataScope{{RoleCode: "expense_applicant", ScopeType: "SELF", ScopeID: "identity-1"}}, AuthorizationRevision: 9}
	raw, _ := json.Marshal(principal)
	store := &pageSessionStore{record: identity.StoredOIDCSession{TenantID: "tenant-1", IdentityID: "identity-1", PrincipalJSON: raw, AuthorizationCheckedAt: now, TokenExpiresAt: now.Add(time.Hour), SessionExpiresAt: now.Add(8 * time.Hour), CreatedAt: now, LastInteractiveAt: now}}
	auth, err := identity.NewOIDCAuthenticator(context.Background(), identity.OIDCOptions{Issuer: issuer, PlatformBaseURL: issuer, TenantID: "tenant-1", ClientID: "test-client", SessionCookieName: "reimbursement_session", SessionIdleTTL: 30 * time.Minute, SessionAbsoluteTTL: 8 * time.Hour, AuthorizationRefreshInterval: time.Minute, AuthorizationTimeout: time.Second}, store, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	registerAuth(router, auth, &Runtime{})
	request := httptest.NewRequest("GET", "/auth/session", nil)
	request.AddCookie(&http.Cookie{Name: "reimbursement_session", Value: "same-cookie"})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var payload struct {
		Data struct {
			DataScopes []identity.DataScope `json:"data_scopes"`
		}
	}
	if err = json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(payload.Data.DataScopes) != 1 || payload.Data.DataScopes[0].ScopeType != "SELF" || payload.Data.DataScopes[0].ScopeID != "identity-1" {
		t.Fatalf("current scopes missing: status=%d body=%s", response.Code, response.Body.String())
	}
}
