package httpapi

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPhaseOneReadInterfaces 用途：核验新统计与费用建议使用可信身份并保留错误语义。
// 参数：t 为真实HTTP到应用层的隔离测试上下文。
// 返回值：无；路由缺失、越权或忽略非法选择时失败。
func TestPhaseOneReadInterfaces(t *testing.T) {
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read", "expense.invoice.read", "expense.claim.create"}}
	for _, c := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/v1/statistics", "", 200},
		{"GET", "/api/v1/parse-exceptions", "", 200},
		{"POST", "/api/v1/invoice-expense-suggestions", `{"invoice_ids":[]}`, 422},
		{"POST", "/api/v1/claims/c/preview", `{"claim_id":"other","expected_version":1}`, 422},
		{"POST", "/api/v1/claims/c/preview", `{"claim_id":"c","expected_version":1}`, 403},
	} {
		router := testHTTP(t, actor)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		if response.Code != c.status {
			t.Fatalf("%s: status=%d body=%s", c.path, response.Code, response.Body.String())
		}
	}
	router := testHTTP(t, domain.Actor{TenantID: "t", SubjectID: "no-permission"})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/statistics", nil))
	if response.Code != 403 {
		t.Fatalf("unauthorized statistics=%d", response.Code)
	}
}

// TestArchiveDirectoryBindsAuthenticatedViewer 验证跨系统目录响应绑定本次真实查询主体。
// 参数：t为HTTP与临时SQLite契约测试上下文。
// 返回值：无；返回身份缺失或采用客户端身份时失败。
func TestArchiveDirectoryBindsAuthenticatedViewer(t *testing.T) {
	actor := domain.Actor{TenantID: "trusted", SubjectID: "viewer", Permissions: []string{"expense.archive.read", "expense.claim.read"}}
	router := testHTTP(t, actor)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/archive-projects?subject_id=forged&tenant_id=other", nil))
	if response.Code != 200 {
		t.Fatalf("directory=%d %s", response.Code, response.Body.String())
	}
	var result struct {
		Data struct {
			Viewer map[string]string `json:"viewer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Viewer["tenant_id"] != "trusted" || result.Data.Viewer["subject_id"] != "viewer" {
		t.Fatalf("viewer binding lost: %+v", result.Data.Viewer)
	}
}
