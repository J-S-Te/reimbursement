package jobs

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/internal/integration"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestCSVFormulaEscaping 防止导出标题在Excel等工具中执行公式。
// 参数：t为测试上下文。
// 返回值：无。
func TestCSVFormulaEscaping(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"=1+1", "'=1+1"}, {" \t@SUM(A1)", "' \t@SUM(A1)"}, {"正常标题", "正常标题"}, {"", ""}} {
		if got := csvCell(tc.input); got != tc.want {
			t.Fatalf("%q: %q", tc.input, got)
		}
	}
}

// TestCommittedExportReplayDoesNotCreateAnotherFile 验证提交后重投直接回收任务，不依赖已撤销的远端能力或生成重复原件。
// 参数：t为测试上下文。
// 返回值：无。
func TestCommittedExportReplayDoesNotCreateAnotherFile(t *testing.T) {
	p := Processor{}
	row := Record{TenantID: "t", PayloadJSON: `{"actor":{"tenant_id":"t","subject_id":"app","permissions":["expense.archive.export"]},"result":{"file_id":"file","sha256":"` + strings.Repeat("a", 64) + `"}}`}
	if err := p.export(context.Background(), row); err != nil {
		t.Fatal(err)
	}
}

// TestExportRevokedAdminCannotRetainFrozenTenantAll 验证保留导出角色但撤管理员后不能沿旧任务导出整租户。
// 参数：t 为临时数据库与目录替身上下文。
// 返回值：无。
func TestExportRevokedAdminCannotRetainFrozenTenantAll(t *testing.T) {
	db, e := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "export.sqlite")), &gorm.Config{})
	if e != nil {
		t.Fatal(e)
	}
	if e = db.AutoMigrate(store.Models()...); e != nil {
		t.Fatal(e)
	}
	conn, _ := db.DB()
	defer conn.Close()
	c := domain.Claim{ID: "c", TenantID: "t", Applicant: "other", OrgID: "org", Version: 1, Status: "DRAFT"}
	content, _ := json.Marshal(c)
	if e = db.Create(&store.Claim{ID: "c", TenantID: "t", Applicant: "other", OrgID: "org", Version: 1, Status: "DRAFT", ContentJSON: string(content)}).Error; e != nil {
		t.Fatal(e)
	}
	generated := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth2/token" {
			w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"directory","expires_in":300}`))
			return
		}
		if r.URL.Path == "/api/v1/internal/owner-directory" {
			roles := r.URL.Query()["role_code"]
			if len(roles) == 1 && roles[0] == "reimbursement_admin" {
				w.Write([]byte(`{"data":{"total":0,"items":[]}}`))
				return
			}
			w.Write([]byte(`{"data":{"total":1,"items":[{"user_id":"former-admin","organizations":[{"organization_id":"org"}]}]}}`))
			return
		}
		generated++
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"denied"}`))
	}))
	defer server.Close()
	client := integration.NewClient(server.URL, server.URL, "test", "secret", "directory")
	p := Processor{DB: db, Service: application.New(db, application.Dependencies{}), Personnel: &integration.Personnel{Client: client}, Files: integration.NewFiles(db, client)}
	row := Record{TenantID: "t", PayloadJSON: `{"actor":{"tenant_id":"t","subject_id":"former-admin","permissions":["expense.archive.export","expense.claim.read","expense.audit.read","expense.scope.tenant_all"]},"claim_ids":["c"]}`}
	if e = p.export(context.Background(), row); e == nil {
		t.Fatal("撤管理员后仍导出他人单据")
	}
	if generated != 0 {
		t.Fatal("范围收窄前已向文件网关写出数据")
	}
}
