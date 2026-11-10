package authz

import (
	"encoding/json"
	"testing"
)

// TestSuperAdministratorCatalogKeepsExplicitRangeCapability 验证稳定角色码、全动作及唯一全租户标记。
// 参数：t 为目录契约测试上下文。
// 返回值：无。
func TestSuperAdministratorCatalogKeepsExplicitRangeCapability(t *testing.T) {
	var catalog struct {
		Version     string `json:"catalog_version"`
		Permissions []struct {
			Code string `json:"code"`
		} `json:"permissions"`
		Roles []struct {
			Code, Name  string
			Permissions []string `json:"permissions"`
		} `json:"roles"`
	}
	if e := json.Unmarshal(PermissionManifest, &catalog); e != nil {
		t.Fatal(e)
	}
	if catalog.Version != "4" || len(catalog.Roles) != 9 || len(catalog.Permissions) != 32 {
		t.Fatal("目录版本或原角色/动作数量漂移")
	}
	markerHolders := 0
	for _, role := range catalog.Roles {
		permissions := map[string]bool{}
		for _, permission := range role.Permissions {
			permissions[permission] = true
		}
		if permissions["expense.scope.tenant_all"] {
			markerHolders++
			if role.Code != "reimbursement_admin" {
				t.Fatal("普通角色获得全租户标记")
			}
		}
		if role.Code != "reimbursement_admin" {
			continue
		}
		if role.Name != "超级管理员" {
			t.Fatal("稳定角色码未按要求更名")
		}
		for _, permission := range catalog.Permissions {
			if !permissions[permission.Code] {
				t.Fatalf("超级管理员缺少动作:%s", permission.Code)
			}
		}
	}
	if markerHolders != 1 {
		t.Fatal("全租户标记必须仅授予超级管理员")
	}
}

// TestInvoiceCorrectionCatalogGrantsOnlyIndependentFinanceRoles 验证票据重大更正仅由财务复核角色与超级管理员独立批准。
// 参数：t 为目录契约测试上下文。
// 返回值：无。
func TestInvoiceCorrectionCatalogGrantsOnlyIndependentFinanceRoles(t *testing.T) {
	var catalog struct {
		Permissions []struct {
			Code         string `json:"code"`
			Action       string `json:"action"`
			ResourceCode string `json:"resource_code"`
			ResourceName string `json:"resource_name"`
			RiskLevel    string `json:"risk_level"`
		} `json:"permissions"`
		Roles []struct {
			Code        string   `json:"code"`
			Permissions []string `json:"permissions"`
		} `json:"roles"`
	}
	if e := json.Unmarshal(PermissionManifest, &catalog); e != nil {
		t.Fatal(e)
	}
	declared := false
	for _, permission := range catalog.Permissions {
		if permission.Code != "expense.invoice.correct" {
			continue
		}
		declared = true
		if permission.Action != "correct" || permission.ResourceCode != "expense.invoice" || permission.ResourceName != "报销" || permission.RiskLevel != "HIGH" {
			t.Fatal("票据重大更正能力字段漂移")
		}
	}
	if !declared {
		t.Fatal("缺少票据重大更正独立批准能力")
	}
	expected := map[string]bool{"reimbursement_admin": true, "expense_accountant": true}
	for _, role := range catalog.Roles {
		holder := false
		for _, permission := range role.Permissions {
			holder = holder || permission == "expense.invoice.correct"
		}
		if holder != expected[role.Code] {
			t.Fatalf("票据重大更正能力授予范围错误:%s", role.Code)
		}
	}
}
