package httpapi

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestBusinessOptionsRequireAuthenticatedActor 验证对象选择端点不会成为匿名目录。
// 参数：t为实际HTTP路由与统一认证门禁回归上下文。
// 返回值：无；路由缺失或未认证请求进入选项服务时失败。
func TestBusinessOptionsRequireAuthenticatedActor(t *testing.T) {
	router := New(nil, func(c *gin.Context) { c.Next() })
	for _, path := range []string{"/personnel-options", "/invoice-document-options",
		"/invoice-options", "/organization-options", "/configuration-options",
		"/claims/c/payment-options", "/payment-exceptions/e/resolution-options"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1"+path, nil))
		if response.Code != 401 {
			t.Errorf("%s status=%d want 401", path, response.Code)
		}
	}
}
