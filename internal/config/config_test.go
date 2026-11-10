package config

import "testing"

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
