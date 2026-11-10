package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// validCompactClaims 处理报销身份会话及受控协议边界。
// 参数：无。
// 返回值：当前业务或协议处理结果。
func validCompactClaims() oidcClaims {
	return oidcClaims{Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", PersonID: "person-1", Nonce: "nonce-1", TokenUse: "id_token", Name: "项目用户"}
}

// TestValidateCompactIDTokenClaims 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestValidateCompactIDTokenClaims(t *testing.T) {
	claims := validCompactClaims()
	claims.IdentityID = ""
	if _, err := validateCompactIDTokenClaims(claims, "nonce-1", "tenant-1"); err == nil {
		t.Fatal("missing identity_id was accepted")
	}
}

// TestCanonicalSubjectIDSupportsRollingCompatibility 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestCanonicalSubjectIDSupportsRollingCompatibility(t *testing.T) {
	for _, input := range [][2]string{{"identity-1", "identity-1"}, {"", "identity-1"}} {
		if got, err := canonicalSubjectID(input[0], input[1]); err != nil || got != "identity-1" {
			t.Fatalf("canonicalSubjectID(%q, %q) = %q, %v", input[0], input[1], got, err)
		}
	}
	if _, err := canonicalSubjectID("subject-1", "identity-1"); err == nil {
		t.Fatal("mismatched subject_id and identity_id were accepted")
	}
}

// TestValidateCompactIDTokenClaimsRejectsSecurityMismatch 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestValidateCompactIDTokenClaimsRejectsSecurityMismatch(t *testing.T) {
	tests := map[string]func(*oidcClaims){
		"identity":  func(value *oidcClaims) { value.IdentityID = "" },
		"tenant":    func(value *oidcClaims) { value.TenantID = "tenant-2" },
		"nonce":     func(value *oidcClaims) { value.Nonce = "nonce-2" },
		"token use": func(value *oidcClaims) { value.TokenUse = "access_token" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			claims := validCompactClaims()
			mutate(&claims)
			if _, err := validateCompactIDTokenClaims(claims, "nonce-1", "tenant-1"); err == nil {
				t.Fatal("invalid compact identity was accepted")
			}
		})
	}
}

// validAuthorization 处理报销身份会话及受控协议边界。
// 参数：scope为本用途要求的权限集合。
// 返回值：当前业务或协议处理结果。
func validAuthorization(scope DataScope) AuthorizationContext {
	return AuthorizationContext{Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", PersonID: "person-1", ClientID: "reimbursement-dev-web", ApplicationCode: "reimbursement", EnvironmentCode: "dev", Roles: []string{"expense_applicant"}, Permissions: []string{"expense.claim.read"}, DataScopes: []DataScope{scope}, AuthorizationRevision: 7}
}

// TestAuthorizationCatalogAcceptsPlatformScopeContract 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestAuthorizationCatalogAcceptsPlatformScopeContract(t *testing.T) {
	catalog, err := LoadAuthorizationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		scope DataScope
	}{
		{"application empty scope id", DataScope{RoleCode: "expense_applicant", ScopeType: "APPLICATION"}},
		{"environment internal id", DataScope{RoleCode: "expense_applicant", ScopeType: "ENVIRONMENT", ScopeID: "env-internal-id", EnvironmentCode: "dev"}},
		{"tenant", DataScope{RoleCode: "expense_applicant", ScopeType: "TENANT", ScopeID: "tenant-1"}},
		{"organization", DataScope{RoleCode: "expense_applicant", ScopeType: "ORG", ScopeID: "org-1", EnvironmentCode: "dev"}},
		{"self", DataScope{RoleCode: "expense_applicant", ScopeType: "SELF", ScopeID: "identity-1"}},
		{"project", DataScope{RoleCode: "expense_applicant", ScopeType: "PROJECT", ScopeID: "PJ-1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := catalog.Validate(validAuthorization(test.scope), "reimbursement-dev-web", "reimbursement", "dev"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestApplicationScopeDoesNotGrantFinancialRange 防止应用授权变成财务全量范围。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApplicationScopeDoesNotGrantFinancialRange(t *testing.T) {
	p := Principal{TenantID: "tenant-1", IdentityID: "identity-1", Permissions: map[string]bool{"expense.claim.read": true}, DataScopes: []DataScope{{RoleCode: "reimbursement_admin", ScopeType: "APPLICATION"}}}
	actor := p.Actor()
	if actor.SubjectID != "identity-1" || len(actor.OrgIDs) != 0 {
		t.Fatalf("application scope leaked range: %+v", actor)
	}
}

// TestAuthorizationCatalogRejectsInvalidBindingAndScopes 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestAuthorizationCatalogRejectsInvalidBindingAndScopes(t *testing.T) {
	catalog, _ := LoadAuthorizationCatalog()
	tests := map[string]AuthorizationContext{
		"client":      validAuthorization(DataScope{RoleCode: "expense_applicant", ScopeType: "APPLICATION"}),
		"environment": validAuthorization(DataScope{RoleCode: "expense_applicant", ScopeType: "ENVIRONMENT", ScopeID: "env-1", EnvironmentCode: "prod"}),
		"unknown":     validAuthorization(DataScope{RoleCode: "expense_applicant", ScopeType: "CUSTOMER", ScopeID: "customer-1"}),
		"empty org":   validAuthorization(DataScope{RoleCode: "expense_applicant", ScopeType: "ORG"}),
		"wildcard":    validAuthorization(DataScope{RoleCode: "expense_applicant", ScopeType: "APPLICATION"}),
	}
	client := tests["client"]
	client.ClientID = "other-client"
	tests["client"] = client
	wildcard := tests["wildcard"]
	wildcard.Permissions = []string{"all"}
	tests["wildcard"] = wildcard
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if err := catalog.Validate(value, "reimbursement-dev-web", "reimbursement", "dev"); err == nil {
				t.Fatal("invalid authorization context was accepted")
			}
		})
	}
}

// TestAuthorizationContextClientClassifiesResponses 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestAuthorizationContextClientClassifiesResponses(t *testing.T) {
	for _, test := range []struct {
		status int
		want   error
	}{{http.StatusUnauthorized, ErrUnauthenticated}, {http.StatusForbidden, ErrAuthorizationDenied}, {http.StatusInternalServerError, ErrAuthorizationUnavailable}} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status) }))
			defer server.Close()
			_, err := NewHTTPAuthorizationContextClient(server.URL, server.Client()).Resolve(context.Background(), "access-token")
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
}

// TestAuthorizationContextClientDecodesMetadata 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestAuthorizationContextClientDecodesMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("authorization header=%q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(validAuthorization(DataScope{RoleCode: "expense_applicant", ScopeType: "APPLICATION"}))
	}))
	defer server.Close()
	value, err := NewHTTPAuthorizationContextClient(server.URL, server.Client()).Resolve(context.Background(), "access-token")
	if err != nil || value.ClientID != "reimbursement-dev-web" || value.ApplicationCode != "reimbursement" || value.EnvironmentCode != "dev" {
		t.Fatalf("value=%+v error=%v", value, err)
	}
}

type fakeOIDCStore struct {
	login      LoginTransaction
	session    StoredOIDCSession
	created    StoredOIDCSession
	calls      int
	revoked    bool
	revokedAll bool
	updated    bool
	touched    bool
	touchErr   error
	touchNow   time.Time
	touchAt    time.Time
}

// SaveLoginTransaction 处理报销身份会话及受控协议边界。
// 参数：_为当前SaveLoginTransaction业务处理的受控输入；value为待校验或持久化的协议业务值。
// 返回值：处理结果及校验、存储或依赖失败错误。
func (s *fakeOIDCStore) SaveLoginTransaction(_ context.Context, value LoginTransaction) error {
	s.login = value
	return nil
}

// ConsumeLoginTransaction 处理报销身份会话及受控协议边界。
// 参数：无。
// 返回值：处理结果及校验、存储或依赖失败错误。
func (s *fakeOIDCStore) ConsumeLoginTransaction(context.Context, []byte, time.Time) (LoginTransaction, error) {
	return LoginTransaction{}, errOIDCRecordNotFound
}

// CreateSession 处理报销身份会话及受控协议边界。
// 参数：_为当前CreateSession业务处理的受控输入；value为待校验或持久化的协议业务值。
// 返回值：处理结果及校验、存储或依赖失败错误。
func (s *fakeOIDCStore) CreateSession(_ context.Context, value StoredOIDCSession) error {
	s.created, s.calls = value, s.calls+1
	return nil
}

// FindSession 处理报销身份会话及受控协议边界。
// 参数：无。
// 返回值：处理结果及校验、存储或依赖失败错误。
func (s *fakeOIDCStore) FindSession(context.Context, []byte, time.Time) (StoredOIDCSession, error) {
	if s.revoked || s.revokedAll {
		return StoredOIDCSession{}, errOIDCRecordNotFound
	}
	return s.session, nil
}

// UpdateSession 处理报销身份会话及受控协议边界。
// 参数：_为当前UpdateSession业务处理的受控输入；value为待校验或持久化的协议业务值；_为当前UpdateSession业务处理的受控输入。
// 返回值：处理结果及校验、存储或依赖失败错误。
func (s *fakeOIDCStore) UpdateSession(_ context.Context, value StoredOIDCSession, _ time.Time) error {
	s.updated, s.session = true, value
	return nil
}

// TouchSessionActivity 处理报销身份会话及受控协议边界。
// 参数：_为当前TouchSessionActivity业务处理的受控输入；_为当前TouchSessionActivity业务处理的受控输入；now为本次服务端校验UTC时间；idleCutoff为当前TouchSessionActivity业务处理的受控输入。
// 返回值：处理结果及校验、存储或依赖失败错误。
func (s *fakeOIDCStore) TouchSessionActivity(_ context.Context, _ []byte, now, idleCutoff time.Time) error {
	if s.touchErr != nil {
		return s.touchErr
	}
	s.touched = true
	s.touchNow, s.touchAt = now, idleCutoff
	s.session.LastInteractiveAt = now
	s.session.LastSeenAt = now
	return nil
}

// RevokeSession 处理报销身份会话及受控协议边界。
// 参数：无。
// 返回值：处理结果及校验、存储或依赖失败错误。
func (s *fakeOIDCStore) RevokeSession(context.Context, []byte, time.Time) error {
	s.revoked = true
	return nil
}

// RevokeSessionsForIdentity 处理报销身份会话及受控协议边界。
// 参数：无。
// 返回值：处理结果及校验、存储或依赖失败错误。
func (s *fakeOIDCStore) RevokeSessionsForIdentity(context.Context, string, string, time.Time) error {
	s.revokedAll = true
	return nil
}

// storedPrincipal 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文；now为本次服务端校验UTC时间。
// 返回值：当前业务或协议处理结果。
func storedPrincipal(t *testing.T, now time.Time) (StoredOIDCSession, Principal) {
	t.Helper()
	principal := Principal{TenantID: "tenant-1", IdentityID: "identity-1", UserID: "identity-1", Roles: []string{"expense_applicant"}, Permissions: map[string]bool{"expense.claim.read": true}, DataScopes: []DataScope{{RoleCode: "expense_applicant", ScopeType: "APPLICATION"}}, AuthorizationRevision: 1, CatalogVersion: "2"}
	encoded, err := json.Marshal(principal)
	if err != nil {
		t.Fatal(err)
	}
	activeAt := now.Add(-2 * time.Minute)
	return StoredOIDCSession{TenantID: "tenant-1", IdentityID: "identity-1", PrincipalJSON: encoded, AuthorizationCheckedAt: activeAt, TokenExpiresAt: now.Add(time.Hour), SessionExpiresAt: now.Add(time.Hour), CreatedAt: activeAt, LastSeenAt: activeAt, LastInteractiveAt: activeAt}, principal
}

// TestAuthorizationDenialRevokesAllIdentitySessions 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestAuthorizationDenialRevokesAllIdentitySessions(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	stored, _ := storedPrincipal(t, now)
	store := &fakeOIDCStore{session: stored}
	auth := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1", SessionCookieName: "project_session", SessionIdleTTL: 30 * time.Minute, AuthorizationRefreshInterval: time.Minute, AuthorizationMaxStale: 5 * time.Minute}, store: store, now: func() time.Time { return now }, refreshAuthorization: func(context.Context, StoredOIDCSession, time.Time, string) (StoredOIDCSession, Principal, error) {
		return StoredOIDCSession{}, Principal{}, ErrAuthorizationDenied
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
	if _, err := auth.Authenticate(context.Background(), request); !errors.Is(err, ErrAuthorizationDenied) || !store.revokedAll || store.revoked {
		t.Fatalf("error=%v revokedAll=%v revoked=%v", err, store.revokedAll, store.revoked)
	}
}

// TestAuthorizationRefreshPersistsNewRevisionAcrossAuthenticatorRestart 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestAuthorizationRefreshPersistsNewRevisionAcrossAuthenticatorRestart(t *testing.T) {
	now := time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC)
	stored, _ := storedPrincipal(t, now)
	stored.SessionIDHash = tokenDigest("shared-browser-session")
	store := &fakeOIDCStore{session: stored}
	updatedPrincipal := Principal{TenantID: "tenant-1", IdentityID: "identity-1", UserID: "identity-1", Roles: []string{"expense_applicant"}, Permissions: map[string]bool{"expense.claim.write": true}, DataScopes: []DataScope{{RoleCode: "expense_applicant", ScopeType: "PROJECT", ScopeID: "PJ-2"}}, AuthorizationRevision: 8, CatalogVersion: "2"}
	updatedJSON, err := json.Marshal(updatedPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	first := &OIDCAuthenticator{
		options: OIDCOptions{TenantID: "tenant-1", SessionCookieName: "project_session", SessionIdleTTL: 30 * time.Minute, AuthorizationRefreshInterval: time.Minute, AuthorizationMaxStale: 5 * time.Minute},
		store:   store, now: func() time.Time { return now },
		refreshAuthorization: func(_ context.Context, value StoredOIDCSession, _ time.Time, _ string) (StoredOIDCSession, Principal, error) {
			value.PrincipalJSON = updatedJSON
			value.AuthorizationRevision = updatedPrincipal.AuthorizationRevision
			value.AuthorizationCheckedAt = now
			return value, updatedPrincipal, nil
		},
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	request.AddCookie(first.cookie("shared-browser-session", now.Add(time.Hour)))
	principal, err := first.Authenticate(context.Background(), request)
	if err != nil || principal.AuthorizationRevision != 8 || !principal.Has("expense.claim.write") || principal.Has("expense.claim.read") || !store.updated || store.session.AuthorizationRevision != 8 {
		t.Fatalf("principal=%+v error=%v stored=%+v updated=%v", principal, err, store.session, store.updated)
	}

	// 新认证器代表重启或不同 API 实例。
	// 它读取相同持久存储，而非进程内会话缓存。
	second := &OIDCAuthenticator{options: first.options, store: store, now: func() time.Time { return now.Add(30 * time.Second) }}
	principal, err = second.Authenticate(context.Background(), request)
	if err != nil || principal.AuthorizationRevision != 8 || !principal.Has("expense.claim.write") || principal.Has("expense.claim.read") {
		t.Fatalf("restarted instance principal=%+v error=%v", principal, err)
	}
}

// TestRevocationIsVisibleToAnotherAuthenticatorInstance 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestRevocationIsVisibleToAnotherAuthenticatorInstance(t *testing.T) {
	now := time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC)
	stored, _ := storedPrincipal(t, now)
	store := &fakeOIDCStore{session: stored}
	first := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1", SessionCookieName: "project_session", SessionIdleTTL: 30 * time.Minute}, store: store, now: func() time.Time { return now }}
	second := &OIDCAuthenticator{options: first.options, store: store, now: first.now}
	if err := first.store.RevokeSessionsForIdentity(context.Background(), "tenant-1", "identity-1", now); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	request.AddCookie(second.cookie("shared-browser-session", now.Add(time.Hour)))
	if _, err := second.Authenticate(context.Background(), request); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked session error=%v", err)
	}
}

// TestAuthorizationOutageAllowsOnlyFreshReadSnapshot 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestAuthorizationOutageAllowsOnlyFreshReadSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	stored, expected := storedPrincipal(t, now)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			store := &fakeOIDCStore{session: stored}
			auth := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1", SessionCookieName: "project_session", SessionIdleTTL: 30 * time.Minute, AuthorizationRefreshInterval: time.Minute, AuthorizationMaxStale: 5 * time.Minute}, store: store, now: func() time.Time { return now }, refreshAuthorization: func(context.Context, StoredOIDCSession, time.Time, string) (StoredOIDCSession, Principal, error) {
				return StoredOIDCSession{}, Principal{}, ErrAuthorizationUnavailable
			}}
			request := httptest.NewRequest(method, "/api/v1/projects", nil)
			request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
			principal, err := auth.Authenticate(context.Background(), request)
			if method == http.MethodGet {
				if err != nil || principal.IdentityID != expected.IdentityID || !store.updated {
					t.Fatalf("principal=%+v error=%v updated=%v", principal, err, store.updated)
				}
			} else if !errors.Is(err, ErrAuthorizationUnavailable) {
				t.Fatalf("write error=%v", err)
			}
		})
	}
}

// TestLoginPersistsOnlyStateDigestAndEncryptedSecrets 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestLoginPersistsOnlyStateDigestAndEncryptedSecrets(t *testing.T) {
	codec, err := newSecretCodec([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeOIDCStore{}
	auth := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1", IdentityProviderHint: "basic-platform"}, store: store, codec: codec, now: time.Now, oauth: oauth2.Config{ClientID: "reimbursement-dev-web", RedirectURL: "http://localhost/callback", Endpoint: oauth2.Endpoint{AuthURL: "http://keycloak/authorize"}}}
	request := httptest.NewRequest(http.MethodGet, "/auth/login?return_to=/projects/PJ-1", nil)
	response := httptest.NewRecorder()
	auth.Login(response, request)
	if response.Code != http.StatusFound || len(store.login.StateHash) != 32 || string(store.login.StateHash) == response.Header().Get("Location") || store.login.ReturnPath != "/projects/PJ-1" {
		t.Fatalf("status=%d login=%+v location=%s", response.Code, store.login, response.Header().Get("Location"))
	}
	if len(store.login.NonceCipher) == 0 || len(store.login.CodeVerifierCipher) == 0 {
		t.Fatal("nonce or verifier ciphertext is empty")
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := location.Query().Get("kc_idp_hint"); got != "basic-platform" {
		t.Fatalf("kc_idp_hint = %q", got)
	}
}

// TestLoginOmitsEmptyIdentityProviderHint 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestLoginOmitsEmptyIdentityProviderHint(t *testing.T) {
	codec, err := newSecretCodec([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeOIDCStore{}
	auth := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1"}, store: store, codec: codec, now: time.Now, oauth: oauth2.Config{ClientID: "reimbursement-dev-web", RedirectURL: "http://localhost/callback", Endpoint: oauth2.Endpoint{AuthURL: "http://keycloak/authorize"}}}
	response := httptest.NewRecorder()
	auth.Login(response, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := location.Query()["kc_idp_hint"]; exists {
		t.Fatalf("unexpected kc_idp_hint in %q", location.String())
	}
}

// TestLoginForwardsForcedLoginPrompt 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestLoginForwardsForcedLoginPrompt(t *testing.T) {
	codec, err := newSecretCodec([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeOIDCStore{}
	auth := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1", IdentityProviderHint: "basic-platform"}, store: store, codec: codec, now: time.Now, oauth: oauth2.Config{ClientID: "reimbursement-dev-web", RedirectURL: "http://localhost/callback", Endpoint: oauth2.Endpoint{AuthURL: "http://keycloak/authorize"}}}
	response := httptest.NewRecorder()
	auth.Login(response, httptest.NewRequest(http.MethodGet, "/auth/login?prompt=login", nil))
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := location.Query().Get("prompt"); got != "login" {
		t.Fatalf("prompt = %q, want login", got)
	}
	if got := location.Query().Get("kc_idp_hint"); got != "basic-platform" {
		t.Fatalf("kc_idp_hint = %q, want basic-platform", got)
	}
}

// sessionTestAuthenticator 构造启用空闲/绝对双期限的认证器；测试用固定时钟。
// 参数：store为持久化会话存储；now为本次服务端校验UTC时间；idle为当前sessionTestAuthenticator业务处理的受控输入；absolute为当前sessionTestAuthenticator业务处理的受控输入。
// 返回值：当前业务或协议处理结果。
func sessionTestAuthenticator(store *fakeOIDCStore, now time.Time, idle, absolute time.Duration) *OIDCAuthenticator {
	return &OIDCAuthenticator{
		options: OIDCOptions{TenantID: "tenant-1", SessionCookieName: "project_session", SessionIdleTTL: idle, SessionAbsoluteTTL: absolute, AuthorizationRefreshInterval: time.Hour, AuthorizationMaxStale: 5 * time.Minute},
		store:   store,
		now:     func() time.Time { return now },
	}
}

// TestCreateSessionUsesUnifiedIdleAndAbsoluteDeadlines 覆盖"新建会话"契约：
// 参数：t为测试上下文。
// 返回值：无。
func TestCreateSessionUsesUnifiedIdleAndAbsoluteDeadlines(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	store := &fakeOIDCStore{}
	auth := sessionTestAuthenticator(store, now, 30*time.Minute, 8*time.Hour)
	stored := StoredOIDCSession{TenantID: "tenant-1", IdentityID: "identity-1", SessionIDHash: tokenDigest("raw-session")}
	expiresAt, err := auth.createSession(context.Background(), now, stored)
	if err != nil {
		t.Fatal(err)
	}
	if !expiresAt.Equal(now.Add(8*time.Hour)) || !store.created.SessionExpiresAt.Equal(now.Add(8*time.Hour)) {
		t.Fatalf("absolute expiry = %s / stored %s, want %s", expiresAt, store.created.SessionExpiresAt, now.Add(8*time.Hour))
	}
	if !store.created.CreatedAt.Equal(now) || !store.created.LastSeenAt.Equal(now) || !store.created.LastInteractiveAt.Equal(now) {
		t.Fatalf("new session timestamps = %+v, want created/last_seen/last_interactive at login time", store.created)
	}
}

// TestSessionIdleExpiryRejectsAuthentication 覆盖空闲到期：即使 Cookie 与绝对期限仍有效，
// 参数：t为测试上下文。
// 返回值：无。
func TestSessionIdleExpiryRejectsAuthentication(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	stored, _ := storedPrincipal(t, now)
	stored.LastInteractiveAt = now.Add(-31 * time.Minute)
	store := &fakeOIDCStore{session: stored}
	auth := sessionTestAuthenticator(store, now, 30*time.Minute, 8*time.Hour)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
	if _, err := auth.Authenticate(context.Background(), request); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("idle-expired session error = %v, want unauthenticated", err)
	}
	if store.updated {
		t.Fatal("idle-expired session must not be refreshed or renewed")
	}
}

// TestSessionAbsoluteExpiryRejectsAuthenticationAndActivity 覆盖绝对到期：
// 参数：t为测试上下文。
// 返回值：无。
func TestSessionAbsoluteExpiryRejectsAuthenticationAndActivity(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	stored, _ := storedPrincipal(t, now)
	stored.SessionExpiresAt = now.Add(-time.Minute)
	stored.LastInteractiveAt = now
	store := &fakeOIDCStore{session: stored}
	auth := sessionTestAuthenticator(store, now, 30*time.Minute, 8*time.Hour)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
	if _, err := auth.Authenticate(context.Background(), request); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("absolute-expired authenticate error = %v", err)
	}
	if _, err := auth.TouchSessionActivity(context.Background(), request); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("absolute-expired activity error = %v", err)
	}
	if store.touched || store.updated {
		t.Fatalf("expired session was renewed: touched=%v updated=%v", store.touched, store.updated)
	}
}

// TestOrdinaryRequestUpdatesLastSeenOnly 覆盖"普通业务请求只更新 last_seen_at"：
// 参数：t为测试上下文。
// 返回值：无。
func TestOrdinaryRequestUpdatesLastSeenOnly(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	stored, expected := storedPrincipal(t, now)
	interactiveAt := now.Add(-2 * time.Minute)
	stored.LastInteractiveAt = interactiveAt
	store := &fakeOIDCStore{session: stored}
	auth := sessionTestAuthenticator(store, now, 30*time.Minute, 8*time.Hour)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
	principal, err := auth.Authenticate(context.Background(), request)
	if err != nil || principal.IdentityID != expected.IdentityID || !store.updated {
		t.Fatalf("principal=%+v error=%v updated=%v", principal, err, store.updated)
	}
	if !store.session.LastSeenAt.Equal(now) {
		t.Fatalf("last_seen_at = %s, want %s", store.session.LastSeenAt, now)
	}
	if !store.session.LastInteractiveAt.Equal(interactiveAt) {
		t.Fatalf("ordinary request advanced last_interactive_at: %s", store.session.LastInteractiveAt)
	}
}

// TestTouchSessionActivityRenewsIdleButNotAbsolute 覆盖活动续期：显式活动把空闲期限
// 参数：t为测试上下文。
// 返回值：无。
func TestTouchSessionActivityRenewsIdleButNotAbsolute(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	stored, _ := storedPrincipal(t, now)
	stored.CreatedAt = now.Add(-30 * time.Minute)
	stored.LastInteractiveAt = now.Add(-29 * time.Minute)
	absoluteExpiry := now.Add(time.Hour)
	stored.SessionExpiresAt = absoluteExpiry
	store := &fakeOIDCStore{session: stored}
	auth := sessionTestAuthenticator(store, now, 30*time.Minute, 8*time.Hour)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/activity", nil)
	request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
	status, err := auth.TouchSessionActivity(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !store.touched || !store.touchNow.Equal(now) || !store.touchAt.Equal(now.Add(-30*time.Minute)) {
		t.Fatalf("activity update = touched %v now %s idleCutoff %s", store.touched, store.touchNow, store.touchAt)
	}
	if status.IdleTimeout != 30*time.Minute || status.IdleRemaining != 30*time.Minute {
		t.Fatalf("activity status = %+v, want idle 30m and remaining 30m (absolute 1h away)", status)
	}
	if !store.session.LastInteractiveAt.Equal(now) || !store.session.SessionExpiresAt.Equal(absoluteExpiry) {
		t.Fatalf("activity renewal changed absolute expiry or missed interactive time: %+v", store.session)
	}

	// 31 分钟后再来：空闲期限已再次到期，必须 401；绝对到期也未因活动而延长。
	later := now.Add(31 * time.Minute)
	laterAuth := sessionTestAuthenticator(store, later, 30*time.Minute, 8*time.Hour)
	laterRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/activity", nil)
	laterRequest.AddCookie(laterAuth.cookie("session-1", absoluteExpiry))
	if _, err := laterAuth.TouchSessionActivity(context.Background(), laterRequest); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("activity after renewed idle window error = %v", err)
	}

	// 越过绝对期限：空闲仍在窗口内也不算有效，且不能被续期。
	atAbsolute := now.Add(2 * time.Hour)
	absoluteAuth := sessionTestAuthenticator(store, atAbsolute, 2*time.Hour, 8*time.Hour)
	absoluteRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/activity", nil)
	absoluteRequest.AddCookie(absoluteAuth.cookie("session-1", now.Add(3*time.Hour)))
	store.session.LastInteractiveAt = atAbsolute.Add(-time.Minute)
	if _, err := absoluteAuth.TouchSessionActivity(context.Background(), absoluteRequest); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("activity past absolute deadline error = %v", err)
	}
}

// TestLegacySessionWithoutInteractiveTimestampUsesCreatedAt 覆盖旧存量行兼容：
// 参数：t为测试上下文。
// 返回值：无。
func TestLegacySessionWithoutInteractiveTimestampUsesCreatedAt(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	stored, _ := storedPrincipal(t, now)
	stored.LastInteractiveAt = time.Time{}
	stored.CreatedAt = now.Add(-10 * time.Minute)
	store := &fakeOIDCStore{session: stored}
	auth := sessionTestAuthenticator(store, now, 30*time.Minute, 8*time.Hour)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
	if _, err := auth.Authenticate(context.Background(), request); err != nil {
		t.Fatalf("legacy session rejected: %v", err)
	}
	status, err := auth.SessionIdleStatus(context.Background(), request)
	if err != nil || status.IdleRemaining != 20*time.Minute {
		t.Fatalf("legacy idle remaining = %s error = %v, want 20m", status.IdleRemaining, err)
	}

	// 同一旧行超过 30 分钟无交互后按 created_at 判为到期。
	stored.CreatedAt = now.Add(-31 * time.Minute)
	store.session = stored
	if _, err := auth.Authenticate(context.Background(), request); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("legacy idle-expired session error = %v", err)
	}
}

// TestSessionIdleStatusUsesEarliestDeadline 覆盖 /auth/me 剩余时间：取空闲剩余与
// 参数：t为测试上下文。
// 返回值：无。
func TestSessionIdleStatusUsesEarliestDeadline(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	stored, _ := storedPrincipal(t, now)
	stored.LastInteractiveAt = now
	stored.SessionExpiresAt = now.Add(10 * time.Minute)
	store := &fakeOIDCStore{session: stored}
	auth := sessionTestAuthenticator(store, now, 30*time.Minute, 8*time.Hour)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
	status, err := auth.SessionIdleStatus(context.Background(), request)
	if err != nil || status.IdleTimeout != 30*time.Minute || status.IdleRemaining != 10*time.Minute {
		t.Fatalf("status = %+v error = %v, want remaining 10m (absolute sooner)", status, err)
	}
	if store.touched {
		t.Fatal("/auth/me must not renew the idle deadline")
	}
}

// TestTouchSessionActivityPropagatesStoreFailure 覆盖存储故障：活动续期不能把数据库
// 参数：t为测试上下文。
// 返回值：无。
func TestTouchSessionActivityPropagatesStoreFailure(t *testing.T) {
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	stored, _ := storedPrincipal(t, now)
	store := &fakeOIDCStore{session: stored, touchErr: errors.New("database unavailable")}
	auth := sessionTestAuthenticator(store, now, 30*time.Minute, 8*time.Hour)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/activity", nil)
	request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
	if _, err := auth.TouchSessionActivity(context.Background(), request); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("store failure error = %v, want non-authentication error", err)
	}
}

// TestOIDCSessionRecordRoundTripsNullableInteractiveTimestamp 覆盖存储映射：
// 参数：t为测试上下文。
// 返回值：无。
func TestOIDCSessionRecordRoundTripsNullableInteractiveTimestamp(t *testing.T) {
	legacy := sessionRecord(StoredOIDCSession{SessionIDHash: []byte{1}})
	if legacy.LastInteractiveAt != nil {
		t.Fatalf("zero interactive time must persist as NULL, got %v", legacy.LastInteractiveAt)
	}
	if got := storedSession(legacy); !got.LastInteractiveAt.IsZero() {
		t.Fatalf("NULL interactive time = %s, want zero", got.LastInteractiveAt)
	}
	interactiveAt := time.Date(2026, 10, 30, 9, 30, 0, 0, time.UTC)
	record := sessionRecord(StoredOIDCSession{SessionIDHash: []byte{1}, LastInteractiveAt: interactiveAt})
	if record.LastInteractiveAt == nil || !record.LastInteractiveAt.Equal(interactiveAt) {
		t.Fatalf("record interactive time = %v, want %s", record.LastInteractiveAt, interactiveAt)
	}
	if got := storedSession(record); !got.LastInteractiveAt.Equal(interactiveAt) {
		t.Fatalf("round-trip interactive time = %s, want %s", got.LastInteractiveAt, interactiveAt)
	}
}

// TestNewOIDCAuthenticatorFailsClosedOnInvalidSessionDeadlines 覆盖启动失败关闭：
// 参数：t为测试上下文。
// 返回值：无。
func TestNewOIDCAuthenticatorFailsClosedOnInvalidSessionDeadlines(t *testing.T) {
	store := &fakeOIDCStore{}
	for _, test := range []struct {
		name     string
		idle     time.Duration
		absolute time.Duration
	}{
		{"missing idle", 0, 8 * time.Hour},
		{"missing absolute", 30 * time.Minute, 0},
		{"absolute equals idle", 30 * time.Minute, 30 * time.Minute},
		{"absolute shorter than idle", 2 * time.Hour, time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewOIDCAuthenticator(context.Background(), OIDCOptions{SessionIdleTTL: test.idle, SessionAbsoluteTTL: test.absolute}, store, nil); err == nil {
				t.Fatal("invalid session deadlines were accepted")
			}
		})
	}
}

// TestLogoutUsesDiscoveredEndSessionEndpoint 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestLogoutUsesDiscoveredEndSessionEndpoint(t *testing.T) {
	codec, err := newSecretCodec([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	idCipher, err := codec.encrypt([]byte("id-token"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	store := &fakeOIDCStore{session: StoredOIDCSession{TenantID: "tenant-1", IdentityID: "identity-1", IDTokenCipher: idCipher, SessionExpiresAt: now.Add(time.Hour)}}
	auth := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1", ClientID: "reimbursement-dev-web", SessionCookieName: "project_session", PostLogoutRedirectURI: "http://localhost/logged-out"}, store: store, codec: codec, endSessionEndpoint: "http://keycloak/realms/basic-platform/protocol/openid-connect/logout", now: func() time.Time { return now }}
	request := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
	response := httptest.NewRecorder()
	auth.Logout(response, request)
	location := response.Header().Get("Location")
	for _, expected := range []string{"/protocol/openid-connect/logout", "client_id=reimbursement-dev-web", "id_token_hint=id-token", "post_logout_redirect_uri="} {
		if !strings.Contains(location, expected) {
			t.Fatalf("logout location missing %q: %s", expected, location)
		}
	}
	if !store.revoked {
		t.Fatal("local session was not revoked")
	}
}

// TestSensitiveWriteAlwaysRefreshesAuthorization 防止一分钟缓存窗口内继续执行已撤销的敏感写操作。
// 参数：t为测试上下文。
// 返回值：无。
func TestSensitiveWriteAlwaysRefreshesAuthorization(t *testing.T) {
	now := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	stored, _ := storedPrincipal(t, now)
	stored.AuthorizationCheckedAt = now.Add(-time.Second)
	store := &fakeOIDCStore{session: stored}
	auth := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1", SessionCookieName: "expense_session", SessionIdleTTL: 30 * time.Minute, AuthorizationRefreshInterval: time.Minute, AuthorizationMaxStale: time.Minute}, store: store, now: func() time.Time { return now }, refreshAuthorization: func(context.Context, StoredOIDCSession, time.Time, string) (StoredOIDCSession, Principal, error) {
		return StoredOIDCSession{}, Principal{}, ErrAuthorizationUnavailable
	}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/claims/c/payments", nil)
	request.AddCookie(auth.cookie("session-1", now.Add(time.Hour)))
	if _, err := auth.Authenticate(context.Background(), request); !errors.Is(err, ErrAuthorizationUnavailable) {
		t.Fatalf("write reused cached authorization: %v", err)
	}
}
