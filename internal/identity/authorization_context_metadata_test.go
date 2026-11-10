package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// resolveMetadataContext 用途：通过真实HTTP客户端读取带扩展字段的平台授权上下文。
// 参数：t 为测试上下文，fields 为平台响应的指定元数据或非法字段。
// 返回值：实际解码结果与错误；测试服务器由测试结束自动关闭。
func resolveMetadataContext(t *testing.T, fields map[string]any) (AuthorizationContext, error) {
	t.Helper()
	base, err := json.Marshal(validAuthorization(DataScope{
		RoleCode: "expense_applicant", ScopeType: "APPLICATION",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err = json.Unmarshal(base, &response); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		response[key] = value
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return NewHTTPAuthorizationContextClient(server.URL, server.Client()).Resolve(
		context.Background(), "metadata-test-token")
}

// TestAuthorizationContextAcceptsPlatformOptionalMetadata 用途：兼容平台已声明的可选字符串元数据。
// 参数：t 为身份协议测试上下文。
// 返回值：无；有效上下文因元数据被拒或元数据扩大主体权限时失败。
func TestAuthorizationContextAcceptsPlatformOptionalMetadata(t *testing.T) {
	value, err := resolveMetadataContext(t, map[string]any{
		"user_login_ip": "203.0.113.7", "customer_ref": "customer-other",
	})
	if err != nil {
		t.Fatalf("platform optional metadata rejected: %v", err)
	}
	catalog, err := LoadAuthorizationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	a := OIDCAuthenticator{catalog: catalog, options: OIDCOptions{
		ClientID: "reimbursement-dev-web", ApplicationCode: "reimbursement", EnvironmentCode: "dev",
	}}
	principal, err := a.principalFromAuthorization(compactIdentity{
		Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1",
	}, value)
	if err != nil {
		t.Fatal("valid identity lost after metadata decode", err)
	}
	actor := principal.Actor()
	if actor.TenantID != "tenant-1" || actor.SubjectID != "identity-1" ||
		len(actor.OrgIDs) != 0 || principal.Has("expense.payment.record") {
		t.Fatal("optional metadata changed trusted identity or financial scope")
	}
}

// TestAuthorizationContextMetadataRetainsStrictDecoding 用途：拒绝类型错误及未声明授权字段。
// 参数：t 为平台契约负向测试上下文。
// 返回值：无；字段兼容放宽未知字段或接受非字符串元数据时失败。
func TestAuthorizationContextMetadataRetainsStrictDecoding(t *testing.T) {
	for _, fields := range []map[string]any{
		{"user_login_ip": 7}, {"customer_ref": map[string]any{"tenant_id": "other"}},
		{"unknown_grant": "all"},
	} {
		if _, err := resolveMetadataContext(t, fields); !errors.Is(err, ErrInvalidAuthorization) {
			t.Fatalf("undeclared or malformed metadata accepted: %v", err)
		}
	}
}
