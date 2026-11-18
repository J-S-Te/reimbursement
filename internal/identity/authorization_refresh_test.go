package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	central "github.com/j-s-te/reimbursement/internal/sessiongovernance"
	"golang.org/x/oauth2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestPageRefreshRechecksAuthorization 验证页面刷新绕过授权缓存及失败退避。
// 参数：t 为测试上下文。
// 返回值：无；旧授权成功兜底或刷新改变会话期限时失败。
func TestPageRefreshRechecksAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, header string
		retry                      bool
		failure                    error
		calls                      int
	}{
		{name: "fresh roles", method: "GET", path: "/auth/session", header: "1", calls: 1},
		{name: "retry deadline", method: "GET", path: "/auth/session", header: "1", retry: true, calls: 1},
		{name: "dependency unavailable", method: "GET", path: "/auth/session", header: "1", failure: ErrAuthorizationUnavailable, calls: 1},
		{name: "authorization removed", method: "GET", path: "/auth/session", header: "1", failure: ErrAuthorizationDenied, calls: 1},
		{name: "ordinary cached read", method: "GET", path: "/api/v1/claims", calls: 0},
		{name: "business header ignored", method: "GET", path: "/api/v1/claims", header: "1", calls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
			record, old := storedPrincipal(t, now)
			record.AuthorizationCheckedAt = now
			if tc.retry {
				record.RefreshRetryAt = now.Add(time.Minute)
			}
			store := &fakeOIDCStore{session: record}
			calls := 0
			auth := &OIDCAuthenticator{
				options: OIDCOptions{TenantID: "tenant-1", SessionCookieName: "reimbursement_session", SessionIdleTTL: 30 * time.Minute, AuthorizationRefreshInterval: time.Minute, AuthorizationMaxStale: 5 * time.Minute},
				store:   store, now: func() time.Time { return now },
				refreshAuthorization: func(_ context.Context, current StoredOIDCSession, _ time.Time, _ string) (StoredOIDCSession, Principal, error) {
					calls++
					updated := old
					updated.AuthorizationRevision = 8
					updated.Roles = []string{"expense_reviewer"}
					updated.Permissions = map[string]bool{"expense.claim.write": true}
					updated.DataScopes = []DataScope{{RoleCode: "expense_reviewer", ScopeType: "SELF", ScopeID: "identity-1"}}
					return current, updated, tc.failure
				},
			}
			request := httptest.NewRequest(tc.method, tc.path, nil)
			request.Header.Set("X-Authorization-Refresh", tc.header)
			request.AddCookie(&http.Cookie{Name: "reimbursement_session", Value: "same-cookie"})
			principal, err := auth.Authenticate(context.Background(), request)
			if calls != tc.calls || !errors.Is(err, tc.failure) {
				t.Fatalf("calls=%d want=%d err=%v want=%v", calls, tc.calls, err, tc.failure)
			}
			if tc.failure == nil && tc.calls == 1 && (principal.AuthorizationRevision != 8 || principal.Has("expense.claim.read") || len(principal.DataScopes) != 1 || principal.DataScopes[0].ScopeType != "SELF") {
				t.Fatalf("old authorization survived: %+v", principal)
			}
			if !store.session.SessionExpiresAt.Equal(record.SessionExpiresAt) || !store.session.LastInteractiveAt.Equal(record.LastInteractiveAt) {
				t.Fatal("page refresh extended session deadlines")
			}
			if tc.failure == ErrAuthorizationUnavailable && (store.revoked || store.revokedAll) {
				t.Fatal("dependency failure revoked login")
			}
		})
	}
}

// TestPageRefreshDoesNotReturnStalePermissionOnFailure 验证过期快照在显式刷新故障时不成功返回。
// 参数：t 为测试上下文。
// 返回值：无；故障时仍返回旧权限即失败。
func TestPageRefreshDoesNotReturnStalePermissionOnFailure(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	record, _ := storedPrincipal(t, now)
	store := &fakeOIDCStore{session: record}
	auth := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1", SessionCookieName: "reimbursement_session", SessionIdleTTL: 30 * time.Minute, AuthorizationRefreshInterval: time.Minute, AuthorizationMaxStale: 5 * time.Minute}, store: store, now: func() time.Time { return now }, refreshAuthorization: func(context.Context, StoredOIDCSession, time.Time, string) (StoredOIDCSession, Principal, error) {
		return StoredOIDCSession{}, Principal{}, ErrAuthorizationUnavailable
	}}
	request := httptest.NewRequest("GET", "/auth/session", nil)
	request.Header.Set("X-Authorization-Refresh", "1")
	request.AddCookie(&http.Cookie{Name: "reimbursement_session", Value: "same-cookie"})
	principal, err := auth.Authenticate(context.Background(), request)
	if !errors.Is(err, ErrAuthorizationUnavailable) || len(principal.Permissions) != 0 {
		t.Fatalf("stale permission succeeded: principal=%+v err=%v", principal, err)
	}
}

// TestPageRefreshCentralRechecksLockedSnapshot 验证中央模式锁内也绕过缓存并传播平台刷新标记。
// 参数：t 为内存数据库和 HTTP 测试上下文。
// 返回值：无；错误分类、持久权限或原期限不符合约定时失败。
func TestPageRefreshCentralRechecksLockedSnapshot(t *testing.T) {
	for _, status := range []int{200, 403, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := db.DB()
			defer raw.Close()
			if err = db.AutoMigrate(&oidcSessionRecord{}); err != nil {
				t.Fatal(err)
			}
			store := NewGORMOIDCStore(db)
			record, _ := storedPrincipal(t, now)
			record.SessionIDHash = tokenDigest("same-cookie")
			record.AuthorizationCheckedAt = now
			codec, err := newSecretCodec(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			record.CentralBindingCipher, err = codec.encrypt([]byte("binding"))
			if err != nil {
				t.Fatal(err)
			}
			record.CentralSessionID = "central-cycle"
			tokenJSON, _ := json.Marshal(&oauth2.Token{AccessToken: "old-valid-token", Expiry: now.Add(time.Hour)})
			record.OAuthTokenCipher, err = codec.encrypt(tokenJSON)
			if err != nil {
				t.Fatal(err)
			}
			record.IDTokenCipher, err = codec.encrypt([]byte("id-token"))
			if err != nil {
				t.Fatal(err)
			}
			if err = store.CreateSession(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("X-Authorization-Refresh") != "1" {
					t.Error("refresh marker did not reach platform")
				}
				w.WriteHeader(status)
				if status == 200 {
					value := validAuthorization(DataScope{RoleCode: "expense_approver", ScopeType: "SELF", ScopeID: "identity-1"})
					value.Roles = []string{"expense_approver"}
					value.Permissions = []string{"expense.approval.act"}
					value.AuthorizationRevision = 9
					_ = json.NewEncoder(w).Encode(value)
				}
			}))
			defer server.Close()
			centralClient := &centralContractClient{state: central.Context{SessionID: "central-cycle", TenantID: "tenant-1", SubjectID: "identity-1", ClientID: "reimbursement-dev-web", ApplicationCode: "reimbursement", EnvironmentCode: "dev", PolicyVersion: 1, IdleTimeoutSeconds: 1800, IdleRemainingMS: (28 * time.Minute).Milliseconds(), AbsoluteRemainingMS: time.Hour.Milliseconds(), AbsoluteExpiresAt: record.SessionExpiresAt}}
			catalog, _ := LoadAuthorizationCatalog()
			auth := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1", ClientID: "reimbursement-dev-web", ApplicationCode: "reimbursement", EnvironmentCode: "dev", SessionCookieName: "reimbursement_session", CentralClient: centralClient, AuthorizationRefreshInterval: time.Minute}, store: store, codec: codec, catalog: catalog, authorization: NewHTTPAuthorizationContextClient(server.URL, server.Client()), now: func() time.Time { return now }}
			request := httptest.NewRequest("GET", "/auth/session", nil)
			request.Header.Set("X-Authorization-Refresh", "1")
			request.AddCookie(&http.Cookie{Name: "reimbursement_session", Value: "same-cookie"})
			principal, err := auth.Authenticate(context.Background(), request)
			var want error
			if status == 403 {
				want = ErrAuthorizationDenied
			}
			if status == 503 {
				want = ErrAuthorizationUnavailable
			}
			if calls != 1 || !errors.Is(err, want) {
				t.Fatalf("calls=%d err=%v want=%v", calls, err, want)
			}
			if status == 200 {
				persisted, findErr := store.FindSession(context.Background(), record.SessionIDHash, now)
				if findErr != nil || principal.AuthorizationRevision != 9 || len(principal.Roles) != 1 || principal.Roles[0] != "expense_approver" || !principal.Has("expense.approval.act") || principal.Has("expense.claim.read") || principal.DataScopes[0].ScopeType != "SELF" || persisted.AuthorizationRevision != 9 {
					t.Fatalf("permissions not persisted: principal=%+v err=%v", principal, findErr)
				}
				if !persisted.SessionExpiresAt.Equal(record.SessionExpiresAt) || !persisted.LastInteractiveAt.Equal(record.LastInteractiveAt) || centralClient.activities != 0 {
					t.Fatal("authorization refresh renewed the session")
				}
			}
		})
	}
}

type centralContractClient struct {
	central.Client
	state      central.Context
	activities int
}

// State 返回当前隔离测试的中央窗口，不记录活动。
// 参数：上下文及绑定为测试输入。
// 返回值：有效中央窗口，无错误。
func (c *centralContractClient) State(context.Context, string) (central.Context, error) {
	return c.state, nil
}

// Activity 记录显式交互，校验权限刷新不会调用活动入口。
// 参数：上下文及绑定为测试输入。
// 返回值：有效中央窗口，无错误。
func (c *centralContractClient) Activity(context.Context, string) (central.Context, error) {
	c.activities++
	return c.state, nil
}

type pageRefreshWriteFailureStore struct{ fakeOIDCStore }

// UpdateSession 返回隔离的会话写回故障，不改变身份有效性。
// 参数：上下文、会话及时间来自正式认证器的权限刷新保存。
// 返回值：明确存储故障。
func (s *pageRefreshWriteFailureStore) UpdateSession(context.Context, StoredOIDCSession, time.Time) error {
	return errors.New("isolated store unavailable")
}

// TestPageRefreshStorageFailureIsUnavailable 验证权限已读取但保存失败时保留依赖故障语义。
// 参数：t 为本地认证器和存储边界测试上下文。
// 返回值：无；错误返回401、旧权限成功或撤销登录时失败。
func TestPageRefreshStorageFailureIsUnavailable(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	record, principal := storedPrincipal(t, now)
	store := &pageRefreshWriteFailureStore{fakeOIDCStore: fakeOIDCStore{session: record}}
	auth := &OIDCAuthenticator{options: OIDCOptions{TenantID: "tenant-1", SessionCookieName: "reimbursement_session", SessionIdleTTL: 30 * time.Minute, AuthorizationRefreshInterval: time.Minute}, store: store, now: func() time.Time { return now }, refreshAuthorization: func(_ context.Context, value StoredOIDCSession, _ time.Time, _ string) (StoredOIDCSession, Principal, error) {
		return value, principal, nil
	}}
	request := httptest.NewRequest("GET", "/auth/session", nil)
	request.Header.Set("X-Authorization-Refresh", "1")
	request.AddCookie(&http.Cookie{Name: "reimbursement_session", Value: "same-cookie"})
	result, err := auth.Authenticate(context.Background(), request)
	if !errors.Is(err, ErrAuthorizationUnavailable) || len(result.Permissions) != 0 || store.revoked || store.revokedAll {
		t.Fatalf("store error changed identity: result=%+v err=%v", result, err)
	}
}

// WithSessionLock 模拟锁内权限已读但原子提交失败，保留旧会话不冒充成功。
// 参数：ctx为请求，hash和now限定会话，update为真实认证器的复核操作。
// 返回值：原始复核错误或持久化依赖故障。
func (s *pageRefreshWriteFailureStore) WithSessionLock(ctx context.Context, hash []byte, now time.Time,
	update sessionUpdateFunc) (StoredOIDCSession, error) {
	current, err := s.fakeOIDCStore.FindSession(ctx, hash, now)
	if err != nil {
		return current, err
	}
	if _, _, err = update(current); err != nil {
		return current, err
	}
	return current, errors.New("isolated store unavailable")
}
