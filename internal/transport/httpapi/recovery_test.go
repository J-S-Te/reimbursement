package httpapi

import (
	"github.com/j-s-te/reimbursement/internal/domain"
	"net/http/httptest"
	"testing"
)

// TestRecoveryHTTPRejectsOrdinaryApplicant 验证恢复入口独立授权而非使用申请人权限。
// 参数：t 为HTTP契约测试上下文。
// 返回值：无。
func TestRecoveryHTTPRejectsOrdinaryApplicant(t *testing.T) {
	router := testHTTP(t, domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}})
	request := httptest.NewRequest("GET", "/api/v1/operations/outboxes", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatalf("普通申请人恢复列表响应=%d，期望403", response.Code)
	}
}
