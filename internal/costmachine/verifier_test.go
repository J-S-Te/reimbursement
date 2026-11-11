package costmachine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/config"
	"testing"
	"time"
)

// costSigned 生成仅用于内存回归的平台签名声明，不写入或输出凭据。
// 参数：key为一次性测试私钥，claims为待验证声明。
// 返回值：测试JWT。
func costSigned(key ed25519.PrivateKey, claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	body := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(body)))
}

// TestCostMachineExactBindings 验证注册ID、OAuth标签、调用方、租户及时间均精确绑定。
// 参数：t为测试上下文。
// 返回值：无，跨用途、错身份、额外scope或过期被接受时失败。
func TestCostMachineExactBindings(t *testing.T) {
	public, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	options := config.CostMachine{ClientID: "settlement-local-reimbursement-cost", OAuthClientID: "registration", ApplicationID: "app-id", EnvironmentID: "env-id", TenantID: "tenant", CallerApplicationCode: "settlement", CallerEnvironmentCode: "local", Issuer: "basic-platform", Audience: "basic-platform-application"}
	verifier := &Verifier{options: options, key: public}
	claims := map[string]any{"iss": options.Issuer, "aud": options.Audience, "sub": options.ClientID, "oauth_client_id": options.OAuthClientID, "application_id": options.ApplicationID, "environment_id": options.EnvironmentID, "tenant_id": options.TenantID, "application_code": "settlement", "environment_code": "local", "token_use": "application", "scope": []string{"reimbursement.cost_candidate.read"}, "iat": now.Unix() - 1, "nbf": now.Unix() - 1, "exp": now.Unix() + 60}
	if tenant, e := verifier.Verify(costSigned(key, claims), now); e != nil || tenant != "tenant" {
		t.Fatalf("valid token failed: %v", e)
	}
	for field, bad := range map[string]any{"iss": "wrong", "aud": "wrong", "sub": "registration", "oauth_client_id": "wrong", "application_id": "wrong", "environment_id": "wrong", "tenant_id": "other", "application_code": "project_management", "environment_code": "prod", "token_use": "access_token", "scope": []string{"reimbursement.cost_candidate.read", "expense.claim.read"}, "iat": now.Unix() + 10, "nbf": now.Unix() + 10, "exp": now.Unix() + 901} {
		old := claims[field]
		claims[field] = bad
		if _, e := verifier.Verify(costSigned(key, claims), now); e == nil {
			t.Errorf("accepted %s drift", field)
		}
		claims[field] = old
	}
	claims["exp"] = now.Unix()
	if _, e := verifier.Verify(costSigned(key, claims), now); e == nil {
		t.Fatal("accepted expired token")
	}
}
