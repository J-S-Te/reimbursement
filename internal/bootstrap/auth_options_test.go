package bootstrap

import (
	"net/url"
	"testing"

	"github.com/j-s-te/reimbursement/internal/config"
	central "github.com/j-s-te/reimbursement/internal/sessiongovernance"
)

// TestOIDCPublicEndpoints 用途：复现线上应用前缀污染中央登录与退出地址的问题。
// 参数：t为纯配置测试上下文，不连接数据库或身份服务。
// 返回值：无；真实中央客户端生成错误路径或重复退出前缀时失败。
func TestOIDCPublicEndpoints(t *testing.T) {
	for _, public := range []string{"https://apps.example.test", "https://apps.example.test/reimbursement/"} {
		t.Run(public, func(t *testing.T) {
			cfg := config.Config{
				PublicURL: public, Central: true,
				CentralSessionPublicURL: "https://identity.example.test",
				PlatformURL:             "http://platform-api:8080", ClientID: "fixture-client",
				RedirectURI: "https://apps.example.test/reimbursement/auth/callback",
			}
			options := oidcOptions(cfg)
			client, err := central.NewHTTPClient(options.PlatformBaseURL,
				options.CentralSessionPublicBaseURL, options.ClientID, options.RedirectURI)
			if err != nil {
				t.Fatal(err)
			}
			begin, err := url.Parse(client.BeginURL("fixture-state", "fixture-nonce"))
			if err != nil || begin.Host != "identity.example.test" || begin.Path != "/oauth2/session-links/begin" {
				t.Fatalf("central login endpoint=%v error=%v", begin, err)
			}
			if options.PostLogoutRedirectURI != "https://apps.example.test/reimbursement/logged-out" {
				t.Fatalf("logout endpoint=%q", options.PostLogoutRedirectURI)
			}
			if begin.Query().Get("redirect_uri") != cfg.RedirectURI || options.PathPrefix != "/reimbursement" {
				t.Fatal("registered callback or RP cookie scope changed")
			}
		})
	}
}
