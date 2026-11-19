package bootstrap

import (
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/config"
	"github.com/j-s-te/reimbursement/internal/identity"
)

// oidcOptions 用途：将受控配置装配为独立RP参数，区分平台来源和应用路径。
// 参数：cfg为已由config.From校验的报销运行配置。
// 返回值：保留回调与Cookie边界的OIDC选项，不建立网络连接。
func oidcOptions(cfg config.Config) identity.OIDCOptions {
	applicationURL := strings.TrimRight(cfg.PublicURL, "/")
	if !strings.HasSuffix(applicationURL, "/reimbursement") {
		applicationURL += "/reimbursement"
	}
	return identity.OIDCOptions{
		Issuer: cfg.Issuer, BackchannelBaseURL: cfg.Backchannel,
		PlatformBaseURL: cfg.PlatformURL, ClientID: cfg.ClientID,
		ClientSecret: cfg.ClientSecret, RedirectURI: cfg.RedirectURI,
		PostLogoutRedirectURI: applicationURL + "/logged-out",
		TenantID:              cfg.TenantID, ApplicationCode: "reimbursement",
		EnvironmentCode: cfg.Environment, SessionCookieName: cfg.CookieName,
		PathPrefix: "/reimbursement", SessionSecure: cfg.Secure,
		SessionIdleTTL: cfg.IdleTTL, SessionAbsoluteTTL: cfg.AbsoluteTTL,
		AuthorizationRefreshInterval: cfg.AuthorizationRefresh, AuthorizationMaxStale: time.Minute,
		AuthorizationTimeout: 8 * time.Second, CentralSessionEnabled: cfg.Central,
		CentralSessionPublicBaseURL: cfg.CentralSessionPublicURL,
	}
}
