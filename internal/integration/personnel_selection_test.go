package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
)

// TestCurrentApplicantUsesExactUserDirectory 验证本人资料仅按可信会话用户精确读取。
// 参数：t 为测试上下文。
// 返回值：无；查询不限用户、错读姓名或组织名称时失败。
func TestCurrentApplicantUsesExactUserDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth2/token" {
			w.Write([]byte(`{"access_token":"test-token","token_type":"Bearer","scope":"owner_directory.read","expires_in":900}`))
			return
		}
		if r.URL.Path != "/api/v1/internal/owner-directory" || r.URL.Query().Get("user_id") != "app" ||
			r.URL.Query().Get("page_size") != "2" || r.URL.Query().Get("keyword") != "" {
			t.Errorf("本人查询扩大或改变边界: %s", r.URL)
		}
		w.Write([]byte(`{"data":{"total":1,"items":[{"user_id":"app","display_name":"张申请","organizations":[{"organization_id":"sales","organization_name":"销售一部","is_primary":true}],"email":"not-returned@example.test"}]}}`))
	}))
	defer server.Close()
	p := &Personnel{Client: NewClient(server.URL, server.URL, "directory", "secret", "owner_directory.read")}
	person, err := p.CurrentApplicant(context.Background(), domain.Actor{TenantID: "tenant", SubjectID: "app"})
	if err != nil || person.Name != "张申请" || len(person.Organizations) != 1 ||
		person.Organizations[0].Name != "销售一部" || !person.Organizations[0].IsPrimary {
		t.Fatalf("本人资料未遵循真实目录契约: %+v %v", person, err)
	}
	raw, _ := json.Marshal(person)
	var safe map[string]any
	json.Unmarshal(raw, &safe)
	if _, exists := safe["email"]; exists {
		t.Fatal("不应返回联系方式")
	}
}

// TestPersonnelSelectionRejectsDirectoryDrift 验证错用户、重复组织和不完整目录响应失败关闭。
// 参数：t 为测试上下文。
// 返回值：无；损坏目录被当成有效资料时失败。
func TestPersonnelSelectionRejectsDirectoryDrift(t *testing.T) {
	for _, raw := range []string{
		`{"total":1,"items":[{"user_id":"other","display_name":"其它用户"}]}`,
		`{"total":1,"items":[{"user_id":"app","display_name":""}]}`,
		`{"total":2,"items":[{"user_id":"app","display_name":"张申请"}]}`,
		`{"total":1,"items":[{"user_id":"app","display_name":"张申请","organizations":[{"organization_id":"org","organization_name":"组织"},{"organization_id":"org","organization_name":"重复"}]}]}`,
	} {
		if _, _, err := decodePersonnelSelection(json.RawMessage(raw), "app"); err == nil {
			t.Fatalf("损坏目录未拒绝: %s", raw)
		}
	}
	if _, found, err := decodePersonnelSelection(json.RawMessage(`{"total":0,"items":[]}`), "app"); err != nil || found {
		t.Fatal("合法空目录应明确表示未命中")
	}
}
