package httpapi

import (
	"net/http/httptest"
	"testing"
)

// TestCostCandidatesDisabledFailsClosed 验证未接线用途明确失败关闭。
// 参数：t为测试上下文。
// 返回值：无，未配置入口未明确拒绝时失败。
func TestCostCandidatesDisabledFailsClosed(t *testing.T) {
	router := New(nil, nil)
	out := httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest("GET", "/internal/v1/settlement/cost-candidates", nil))
	if out.Code != 503 {
		t.Fatalf("got %d want 503", out.Code)
	}
}
