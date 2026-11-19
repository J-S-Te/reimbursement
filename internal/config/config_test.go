package config

import (
	"encoding/base64"
	"testing"
	"time"
)

// loginRuntimeFixture 用途：提供不连接外部服务的完整合成登录配置。
// 参数：无。
// 返回值：仅用于单测的配置映射，不包含真实运行凭据。
func loginRuntimeFixture() map[string]string {
	values := map[string]string{
		"MYSQL_DSN":                 "test:fixture@tcp(localhost:3306)/reimbursement?parseTime=true&loc=UTC",
		"PLATFORM_ENVIRONMENT_CODE": "prod", "OIDC_CLIENT_ID": "fixture-client",
		"OIDC_CLIENT_SECRET": "fixture", "OIDC_TENANT_ID": "fixture-tenant",
		"PUBLIC_BASE_URL":                    "https://apps.example.test/reimbursement",
		"OIDC_REDIRECT_URI":                  "https://apps.example.test/reimbursement/auth/callback",
		"OIDC_SESSION_ENCRYPTION_KEY_BASE64": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}
	for _, key := range []string{"PLATFORM_BASE_URL", "PROJECT_API_BASE_URL", "FILE_GATEWAY_BASE_URL", "OIDC_ISSUER"} {
		values[key] = "https://platform.example.test"
	}
	for _, prefix := range []string{"PROJECT_REFERENCE", "PROJECT_SPEND", "FILE_GATEWAY",
		"PLATFORM_AUDIT", "PLATFORM_NOTIFICATION", "PLATFORM_OWNER_DIRECTORY"} {
		values[prefix+"_CLIENT_ID"] = "fixture-client"
		values[prefix+"_CLIENT_SECRET"] = "fixture"
	}
	return values
}

// TestRejectsInvalidCentralPublicURL 用途：防止中央登录配置被忽略而误启动。
// 参数：t为隔离配置测试上下文。
// 返回值：无；接受非平台来源、凭据或查询参数时失败。
func TestRejectsInvalidCentralPublicURL(t *testing.T) {
	for _, invalid := range []string{"ftp://platform.example.test", "https://user:pass@platform.example.test",
		"https://platform.example.test/reimbursement", "https://platform.example.test?key=fixture",
		"https://platform.example.test#fragment"} {
		t.Run(invalid, func(t *testing.T) {
			values := loginRuntimeFixture()
			values["CENTRAL_SESSION_PUBLIC_BASE_URL"] = invalid
			if _, err := From(func(key string) string { return values[key] }); err == nil {
				t.Fatal("invalid central public endpoint accepted")
			}
		})
	}
}

// TestCentralPublicOrigin 用途：区分平台登录来源与带报销路径的应用地址。
// 参数：t为隔离配置测试上下文。
// 返回值：无；显式平台来源未保留或旧配置带入子系统路径时失败。
func TestCentralPublicOrigin(t *testing.T) {
	for _, test := range []struct{ name, configured, expected string }{
		{"explicit", "https://identity.example.test/", "https://identity.example.test"},
		{"legacy", "", "https://apps.example.test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := loginRuntimeFixture()
			values["CENTRAL_SESSION_PUBLIC_BASE_URL"] = test.configured
			cfg, err := From(func(key string) string { return values[key] })
			if err != nil || cfg.CentralSessionPublicURL != test.expected {
				t.Fatalf("central origin=%q error=%v", cfg.CentralSessionPublicURL, err)
			}
		})
	}
}

// TestAuthorizationRefresh 用途：默认 10 秒、显式值生效、非法值失败关闭。
// 参数：t 为隔离配置测试上下文。
// 返回值：无；默认或校验行为不符时失败。
func TestAuthorizationRefresh(t *testing.T) {
	values := loginRuntimeFixture()
	cfg, err := From(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("From() error = %v", err)
	}
	if cfg.AuthorizationRefresh != 10*time.Second {
		t.Fatalf("AuthorizationRefresh = %v, want 10s", cfg.AuthorizationRefresh)
	}

	values["OIDC_AUTHORIZATION_REFRESH_INTERVAL"] = "3s"
	cfg, err = From(func(key string) string { return values[key] })
	if err != nil || cfg.AuthorizationRefresh != 3*time.Second {
		t.Fatalf("AuthorizationRefresh = %v error = %v, want 3s", cfg.AuthorizationRefresh, err)
	}

	for _, raw := range []string{"0s", "nonsense", "8h"} {
		values["OIDC_AUTHORIZATION_REFRESH_INTERVAL"] = raw
		if _, err := From(func(key string) string { return values[key] }); err == nil {
			t.Fatalf("OIDC_AUTHORIZATION_REFRESH_INTERVAL=%q accepted", raw)
		}
	}
}

// TestRejectsUnsafeRuntime 防止缺租户、非法开关及无会话加密材料时误启动。
// 参数：t为测试上下文。
// 返回值：无。
func TestRejectsUnsafeRuntime(t *testing.T) {
	for _, env := range []map[string]string{{}, {"REIMBURSEMENT_AUDIT_REQUIRED": "garbage"}, {"MYSQL_DSN": "user:secret@tcp(localhost:3306)/business"}} {
		if _, err := From(func(k string) string { return env[k] }); err == nil {
			t.Fatal("unsafe runtime accepted")
		}
	}
}
