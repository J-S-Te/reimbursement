package httpapi

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestOpenAPIContainsEveryBusinessRoute 防止已注册业务入口遗漏前后端共享契约。
// 参数：t 为纯路由与文档检查上下文。
// 返回值：无；不调用业务网络或数据库写命令。
func TestOpenAPIContainsEveryBusinessRoute(t *testing.T) {
	raw, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`:([A-Za-z_][A-Za-z0-9_]*)`)
	router := testHTTP(t, domain.Actor{TenantID: "t", SubjectID: "actor"})
	for _, route := range router.Routes() {
		path := pattern.ReplaceAllString(strings.TrimPrefix(route.Path, "/api/v1"), "{$1}")
		if _, ok := spec.Paths[path][strings.ToLower(route.Method)]; !ok {
			t.Errorf("OpenAPI缺少 %s %s", route.Method, path)
		}
	}
}
