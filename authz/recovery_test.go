package authz

import (
	"encoding/json"
	"testing"
)

// TestRecoveryAbilitiesRequireExplicitCatalogGrant 验证异常恢复为独立授予且普通岗位不自动持有。
// 参数：t 为目录契约测试上下文。
// 返回值：无。
func TestRecoveryAbilitiesRequireExplicitCatalogGrant(t *testing.T) {
	var catalog struct {
		Permissions []struct{ Code string } `json:"permissions"`
		Roles       []struct {
			Code        string
			Permissions []string
		} `json:"roles"`
	}
	if err := json.Unmarshal(PermissionManifest, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"expense.approval.recover", "expense.integration.manage"} {
		found := false
		for _, permission := range catalog.Permissions {
			if permission.Code == code {
				found = true
			}
		}
		if !found {
			t.Fatalf("缺少独立恢复能力 %s", code)
		}
		for _, role := range catalog.Roles {
			granted := false
			for _, permission := range role.Permissions {
				if permission == code {
					granted = true
				}
			}
			if granted != (role.Code == "reimbursement_admin") {
				t.Fatalf("恢复默认角色不正确 %s/%s", code, role.Code)
			}
		}
	}
}
