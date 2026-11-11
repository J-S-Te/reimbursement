package config

import (
	"errors"
	"strings"
)

type CostMachine struct {
	Enabled                                                bool
	ClientID, OAuthClientID, ApplicationID, EnvironmentID  string
	CallerApplicationCode, CallerEnvironmentCode, TenantID string
	Audience, Issuer, PublicKeyPath, RequiredScope         string
}

// CostMachineFrom 读取独立报销成本机器用途并与本服务租户环境精确绑定。
// 参数：get为配置读取函数，tenant/environment为本服务注册身份。
// 返回值：完整受控配置；未启用默认关闭，非法或缺失字段返回键名错误。
func CostMachineFrom(get func(string) string, tenant, environment string) (CostMachine, error) {
	var c CostMachine
	enabled := get("REIMBURSEMENT_COST_MACHINE_ENABLED")
	if enabled == "" {
		enabled = "false"
	}
	var err error
	c.Enabled, err = strictBool(enabled)
	if err != nil {
		return c, errors.New("invalid REIMBURSEMENT_COST_MACHINE_ENABLED")
	}
	if !c.Enabled {
		return c, nil
	}
	for key, dest := range map[string]*string{"CLIENT_ID": &c.ClientID, "OAUTH_CLIENT_ID": &c.OAuthClientID, "APPLICATION_ID": &c.ApplicationID, "ENVIRONMENT_ID": &c.EnvironmentID, "CALLER_APPLICATION_CODE": &c.CallerApplicationCode, "CALLER_ENVIRONMENT_CODE": &c.CallerEnvironmentCode, "TENANT_ID": &c.TenantID, "AUDIENCE": &c.Audience, "ISSUER": &c.Issuer, "PUBLIC_KEY_PATH": &c.PublicKeyPath, "REQUIRED_SCOPE": &c.RequiredScope} {
		*dest = get("REIMBURSEMENT_COST_MACHINE_" + key)
		if *dest == "" || strings.TrimSpace(*dest) != *dest {
			return c, errors.New("missing or invalid REIMBURSEMENT_COST_MACHINE_" + key)
		}
	}
	if c.Audience != "basic-platform-application" || c.Issuer != "basic-platform" || c.TenantID != tenant || c.CallerEnvironmentCode != environment || c.CallerApplicationCode != "settlement" || c.RequiredScope != "reimbursement.cost_candidate.read" {
		return c, errors.New("reimbursement cost machine trust binding mismatch")
	}
	return c, nil
}
