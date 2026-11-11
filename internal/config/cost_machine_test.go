package config

import "testing"

// TestCostMachineConfigurationFailsClosed 验证缺配置默认关闭且启用必须完整精确绑定。
// 参数：t为测试上下文。
// 返回值：无，非法开关或漂移身份被接受时失败。
func TestCostMachineConfigurationFailsClosed(t *testing.T) {
	values := map[string]string{}
	get := func(key string) string { return values[key] }
	c, e := CostMachineFrom(get, "tenant", "local")
	if e != nil || c.Enabled {
		t.Fatal("absent config must be disabled")
	}
	values["REIMBURSEMENT_COST_MACHINE_ENABLED"] = "true"
	if _, e = CostMachineFrom(get, "tenant", "local"); e == nil {
		t.Fatal("enabled missing identity accepted")
	}
	for k, v := range map[string]string{"CLIENT_ID": "settlement-local-reimbursement-cost", "OAUTH_CLIENT_ID": "registered-id", "APPLICATION_ID": "app", "ENVIRONMENT_ID": "env", "CALLER_APPLICATION_CODE": "settlement", "CALLER_ENVIRONMENT_CODE": "local", "TENANT_ID": "tenant", "AUDIENCE": "basic-platform-application", "ISSUER": "basic-platform", "PUBLIC_KEY_PATH": "public.pem", "REQUIRED_SCOPE": "reimbursement.cost_candidate.read"} {
		values["REIMBURSEMENT_COST_MACHINE_"+k] = v
	}
	if c, e = CostMachineFrom(get, "tenant", "local"); e != nil || !c.Enabled {
		t.Fatal(e)
	}
	for _, key := range []string{"CLIENT_ID", "OAUTH_CLIENT_ID", "APPLICATION_ID", "ENVIRONMENT_ID", "TENANT_ID", "ISSUER", "AUDIENCE", "PUBLIC_KEY_PATH"} {
		old := values["REIMBURSEMENT_COST_MACHINE_"+key]
		delete(values, "REIMBURSEMENT_COST_MACHINE_"+key)
		if _, e = CostMachineFrom(get, "tenant", "local"); e == nil {
			t.Errorf("accepted missing %s", key)
		}
		values["REIMBURSEMENT_COST_MACHINE_"+key] = old
	}
	values["REIMBURSEMENT_COST_MACHINE_REQUIRED_SCOPE"] = "expense.claim.read"
	if _, e = CostMachineFrom(get, "tenant", "local"); e == nil {
		t.Fatal("wrong scope accepted")
	}
}
