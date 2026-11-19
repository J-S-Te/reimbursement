package config

import (
	"encoding/base64"
	"errors"
	driver "github.com/go-sql-driver/mysql"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	CostMachine                                                                    CostMachine
	OwnerID, OwnerSecret                                                           string
	Address, DSN, PlatformURL, ProjectURL, FileURL, Environment, PublicURL         string
	Issuer, Backchannel, ClientID, ClientSecret, TenantID, RedirectURI, CookieName string
	SessionKey                                                                     []byte
	Secure, Central                                                                bool
	CentralSessionPublicURL                                                        string
	IdleTTL, AbsoluteTTL                                                           time.Duration
	// AuthorizationRefresh 是只读请求回源平台授权快照的最大间隔；写请求始终回源。
	AuthorizationRefresh                                                                                                             time.Duration
	ReferenceID, ReferenceSecret, SpendID, SpendSecret, FileID, FileSecret, AuditID, AuditSecret, NotificationID, NotificationSecret string
	ApplicationID, CatalogID, CatalogSecret                                                                                          string
}

// From 加载报销运行参数并对安全必填项失败关闭。
// 参数：get为环境变量读取函数，测试可用隔离映射替代。
// 返回值：经校验配置；错误仅描述键名而不输出敏感值。
func From(get func(string) string) (Config, error) {
	c := Config{Address: value(get, "HTTP_ADDRESS", ":8087"), DSN: get("MYSQL_DSN"), PlatformURL: get("PLATFORM_BASE_URL"), ProjectURL: get("PROJECT_API_BASE_URL"), FileURL: get("FILE_GATEWAY_BASE_URL"), Environment: get("PLATFORM_ENVIRONMENT_CODE"), PublicURL: get("PUBLIC_BASE_URL"), Issuer: get("OIDC_ISSUER"), Backchannel: get("OIDC_BACKCHANNEL_BASE_URL"), ClientID: get("OIDC_CLIENT_ID"), ClientSecret: get("OIDC_CLIENT_SECRET"), TenantID: get("OIDC_TENANT_ID"), RedirectURI: get("OIDC_REDIRECT_URI"), CookieName: value(get, "OIDC_SESSION_COOKIE_NAME", "reimbursement_session"), IdleTTL: 30 * time.Minute, AbsoluteTTL: 8 * time.Hour}
	c.OwnerID = get("PLATFORM_OWNER_DIRECTORY_CLIENT_ID")
	c.OwnerSecret = get("PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET")
	c.ReferenceID = get("PROJECT_REFERENCE_CLIENT_ID")
	c.ReferenceSecret = get("PROJECT_REFERENCE_CLIENT_SECRET")
	c.SpendID = get("PROJECT_SPEND_CLIENT_ID")
	c.SpendSecret = get("PROJECT_SPEND_CLIENT_SECRET")
	c.FileID = get("FILE_GATEWAY_CLIENT_ID")
	c.FileSecret = get("FILE_GATEWAY_CLIENT_SECRET")
	c.AuditID = get("PLATFORM_AUDIT_CLIENT_ID")
	c.AuditSecret = get("PLATFORM_AUDIT_CLIENT_SECRET")
	c.NotificationID = get("PLATFORM_NOTIFICATION_CLIENT_ID")
	c.NotificationSecret = get("PLATFORM_NOTIFICATION_CLIENT_SECRET")
	var costErr error
	c.CostMachine, costErr = CostMachineFrom(get, c.TenantID, c.Environment)
	if costErr != nil {
		return c, costErr
	}
	c.ApplicationID = get("PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID")
	c.CatalogID = get("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID")
	c.CatalogSecret = get("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET")
	required := []string{"MYSQL_DSN", "PLATFORM_BASE_URL", "PLATFORM_ENVIRONMENT_CODE", "PUBLIC_BASE_URL", "PROJECT_API_BASE_URL", "FILE_GATEWAY_BASE_URL", "OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_TENANT_ID", "OIDC_REDIRECT_URI", "PROJECT_REFERENCE_CLIENT_ID", "PROJECT_REFERENCE_CLIENT_SECRET", "PROJECT_SPEND_CLIENT_ID", "PROJECT_SPEND_CLIENT_SECRET", "FILE_GATEWAY_CLIENT_ID", "FILE_GATEWAY_CLIENT_SECRET", "PLATFORM_AUDIT_CLIENT_ID", "PLATFORM_AUDIT_CLIENT_SECRET", "PLATFORM_NOTIFICATION_CLIENT_ID", "PLATFORM_NOTIFICATION_CLIENT_SECRET", "PLATFORM_OWNER_DIRECTORY_CLIENT_ID", "PLATFORM_OWNER_DIRECTORY_CLIENT_SECRET"}
	for _, k := range required {
		if strings.TrimSpace(get(k)) == "" {
			return c, errors.New("missing runtime key: " + k)
		}
	}
	refresh, refreshErr := time.ParseDuration(value(get, "OIDC_AUTHORIZATION_REFRESH_INTERVAL", "10s"))
	if refreshErr != nil || refresh <= 0 || refresh >= c.AbsoluteTTL {
		return c, errors.New("invalid OIDC_AUTHORIZATION_REFRESH_INTERVAL")
	}
	c.AuthorizationRefresh = refresh
	var err error
	c.SessionKey, err = base64.StdEncoding.DecodeString(get("OIDC_SESSION_ENCRYPTION_KEY_BASE64"))
	if err != nil || len(c.SessionKey) != 32 {
		return c, errors.New("invalid OIDC_SESSION_ENCRYPTION_KEY_BASE64")
	}
	for _, k := range []string{"REIMBURSEMENT_AUDIT_REQUIRED", "CENTRAL_SESSION_ENABLED", "OIDC_SESSION_COOKIE_SECURE"} {
		if _, e := strictBool(value(get, k, "true")); e != nil {
			return c, errors.New("invalid boolean: " + k)
		}
	}
	if value(get, "REIMBURSEMENT_AUDIT_REQUIRED", "true") != "true" {
		return c, errors.New("REIMBURSEMENT_AUDIT_REQUIRED must be true")
	}
	c.Central, _ = strictBool(value(get, "CENTRAL_SESSION_ENABLED", "true"))
	c.Secure, _ = strictBool(value(get, "OIDC_SESSION_COOKIE_SECURE", "true"))
	dsn, err := driver.ParseDSN(c.DSN)
	if err != nil || dsn.DBName == "" || !dsn.ParseTime {
		return c, errors.New("MYSQL_DSN must select independent schema and parseTime=true")
	}
	if dsn.Loc != time.UTC {
		return c, errors.New("MYSQL_DSN location must stay UTC")
	}
	for _, raw := range []string{c.PlatformURL, c.ProjectURL, c.FileURL, c.Issuer, c.PublicURL, c.RedirectURI} {
		u, e := url.Parse(raw)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return c, errors.New("invalid service URL")
		}
	}
	public, _ := url.Parse(c.PublicURL)
	c.CentralSessionPublicURL = strings.TrimSpace(get("CENTRAL_SESSION_PUBLIC_BASE_URL"))
	if c.CentralSessionPublicURL == "" {
		c.CentralSessionPublicURL = public.Scheme + "://" + public.Host
	}
	central, centralErr := url.Parse(c.CentralSessionPublicURL)
	if centralErr != nil || central.Host == "" || central.User != nil ||
		(central.Scheme != "http" && central.Scheme != "https") ||
		(central.Path != "" && central.Path != "/") || central.RawQuery != "" || central.Fragment != "" {
		return c, errors.New("invalid CENTRAL_SESSION_PUBLIC_BASE_URL")
	}
	c.CentralSessionPublicURL = central.Scheme + "://" + central.Host
	callback, _ := url.Parse(c.RedirectURI)
	if public.Scheme != callback.Scheme || public.Host != callback.Host || !strings.HasSuffix(callback.Path, "/reimbursement/auth/callback") {
		return c, errors.New("OIDC_REDIRECT_URI must match public reimbursement callback")
	}
	if !c.Secure && public.Scheme != "http" {
		return c, errors.New("HTTPS requires secure session cookies")
	}
	return c, nil
}

// value 为可选运行键提供明确默认值。
// 参数：get为读取函数；key为键名；fallback为默认值。
// 返回值：非空原值或默认值。
func value(get func(string) string, key, fallback string) string {
	if v := get(key); v != "" {
		return v
	}
	return fallback
}

// strictBool 仅接受明确true或false，非法值不得退化放行。
// 参数：value为运行开关原始值。
// 返回值：开关值及格式错误。
func strictBool(value string) (bool, error) {
	if value != "true" && value != "false" {
		return false, errors.New("invalid boolean")
	}
	return strconv.ParseBool(value)
}
