package identity

import "testing"

// TestProjectCatalogCompatibilityRejectsOutsideNMinusOne 处理报销身份会话及受控协议边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestProjectCatalogCompatibilityRejectsOutsideNMinusOne(t *testing.T) {
	if err := validateProjectCatalogCompatibility("2", "3", []string{"3", "2"}); err != nil {
		t.Fatalf("N-1 catalog rejected: %v", err)
	}
	if err := validateProjectCatalogCompatibility("1", "3", []string{"3", "2"}); err == nil {
		t.Fatal("catalog older than N-1 accepted")
	}
	if err := validateProjectCatalogCompatibility("2", "3", []string{"3"}); err == nil {
		t.Fatal("incomplete compatibility window accepted")
	}
}
