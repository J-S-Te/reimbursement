package integration

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTokenRejectsExpandedScope 防止凭据被错误扩大权限后静默使用。
// 参数：t 为测试上下文。
// 返回值：无。
func TestTokenRejectsExpandedScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "secret", "token_type": "Bearer", "scope": "read write", "expires_in": 300})
	}))
	defer server.Close()
	c := NewClient(server.URL, server.URL, "reader", "secret", "read")
	if _, err := c.Token(context.Background()); err == nil {
		t.Fatal("expanded scope must fail closed")
	}
}

// TestCallRejectsHTMLSuccess 防止代理回退页被识别为业务成功。
// 参数：t 为测试上下文。
// 返回值：无。
func TestCallRejectsHTMLSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "secret", "token_type": "Bearer", "scope": "read", "expires_in": 300})
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>fallback</html>"))
	}))
	defer server.Close()
	c := NewClient(server.URL, server.URL, "reader", "secret", "read")
	if _, err := c.Call(context.Background(), http.MethodGet, "/resource", nil); err == nil {
		t.Fatal("HTML success must fail closed")
	}
}

// TestV2UploadRejectsForeignDocument 防止仅凭文件ID对他人资源创建上传会话。
// 参数：t为测试上下文。
// 返回值：无。
func TestV2UploadRejectsForeignDocument(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&store.Document{}); err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&store.Document{ID: "document", TenantID: "tenant", Owner: "someone-else", ContentJSON: "{}", Status: "UPLOAD_PENDING"}).Error; err != nil {
		t.Fatal(err)
	}
	f := NewFiles(db, nil)
	payload := json.RawMessage(`{"purpose":"reimbursement.invoice.original","original_name":"test.pdf","media_type":"application/pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","idempotency_key":"upload1"}`)
	if _, err = f.CreateUpload(context.Background(), domain.Actor{TenantID: "tenant", SubjectID: "attacker"}, "document", payload); err == nil {
		t.Fatal("foreign document accepted")
	}
}

// TestProjectReferenceDecodesAuthoritativeVersions 防止项目数字版本与报销DTO不一致阻断真实提交。
// 参数：t为测试上下文。
// 返回值：无。
func TestProjectReferenceDecodesAuthoritativeVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth2/token" {
			w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"reimbursement.project_reference.read","expires_in":300}`))
			return
		}
		w.Write([]byte(`{"data":{"claim_id":"claim","draft_version":2,"items":[{"project_id":"p1","project_name":"原生项目","source_kind":"NATIVE","eligible":true,"eligibility_source":"SERVICE_ITEM_ASSIGNMENT","required_approver_refs":[{"subject_id":"lead","node_role":"PROJECT_EXPENSE_OWNER","source_field":"manager_identity_id","source_version":3}],"project_version":2,"reference_version":"hash","validated_at":"2026-11-10T08:00:00Z","valid_until":"2026-11-10T08:01:00Z"}]}}`))
	}))
	defer server.Close()
	p := Projects{Client: NewClient(server.URL, server.URL, "reader", "secret", "reimbursement.project_reference.read")}
	refs, err := p.Validate(context.Background(), domain.Actor{TenantID: "t", SubjectID: "applicant"}, "claim", 2, []domain.ReferenceRequest{{ProjectID: "p1", OccurredDate: "2026-11-10"}})
	if err != nil || len(refs) != 1 || len(refs[0].RequiredApproverRefs) != 1 || refs[0].RequiredApproverRefs[0].NodeRole != "PROJECT_EXPENSE_OWNER" {
		t.Fatalf("authoritative version failed: %+v %v", refs, err)
	}
}

// TestAuthorizedFinancialDownloadDoesNotRequireTenantAll 防止已通过整单ACL的组织会计被适配层再次误拒。
// 参数：t为测试上下文。
// 返回值：无。
func TestAuthorizedFinancialDownloadDoesNotRequireTenantAll(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&store.Document{}); err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&store.Document{ID: "original", TenantID: "t", Owner: "applicant", FileID: "file", Status: "PARSED", ContentJSON: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth2/token" {
			w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"download","expires_in":300}`))
			return
		}
		w.Write([]byte(`{"data":{"ticket":"once","download_url":"/file-gateway/api/v2/files/file/content"}}`))
	}))
	defer server.Close()
	f := NewFiles(db, NewClient(server.URL, server.URL, "files", "secret", "download"))
	// 本接口只由已核验完整单据ACL的application.DownloadInvoice调用。
	_, err = f.Download(context.Background(), domain.Actor{TenantID: "t", SubjectID: "accountant", Permissions: []string{"expense.invoice.read", "expense.finance.review"}}, "file")
	if err != nil {
		t.Fatalf("authorized organization accountant rejected: %v", err)
	}
}

// TestUploadRemoteWaitCannotEraseParseHistory 防止远端会话返回期间并发完成解析的历史被旧快照覆盖。
// 参数：t为测试上下文。
// 返回值：无。
func TestUploadRemoteWaitCannotEraseParseHistory(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&store.Document{}); err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&store.Document{ID: "doc", TenantID: "t", Owner: "applicant", Status: "UPLOAD_PENDING", Version: 1, ContentJSON: `{"request_hash":"original"}`}).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth2/token":
			w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"upload","expires_in":300}`))
		case "/api/v2/upload-sessions":
			if e := db.Model(&store.Document{}).Where("id = ?", "doc").Updates(map[string]any{"status": "PARSED", "version": 2, "file_id": "file", "content_json": `{"request_hash":"original","parse_complete":true,"parse_result":{"status":"PARTIAL"},"parse_job_id":"job"}`}).Error; e != nil {
				t.Error(e)
			}
			w.Write([]byte(`{"data":{"upload_id":"upload","file_id":"file","status":"READY","purpose":"reimbursement.invoice.original","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","media_type":"application/pdf"}}`))
		default:
			w.Write([]byte(`{"data":{"ticket":"once","upload_id":"upload","file_id":"file"}}`))
		}
	}))
	defer server.Close()
	f := NewFiles(db, NewClient(server.URL, server.URL, "client", "secret", "upload"))
	payload := json.RawMessage(`{"purpose":"reimbursement.invoice.original","original_name":"test.pdf","media_type":"application/pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","idempotency_key":"key"}`)
	if _, err = f.CreateUpload(context.Background(), domain.Actor{TenantID: "t", SubjectID: "applicant"}, "doc", payload); err != nil {
		t.Fatal(err)
	}
	var doc store.Document
	db.First(&doc, "id = ?", "doc")
	var metadata map[string]any
	json.Unmarshal([]byte(doc.ContentJSON), &metadata)
	if metadata["parse_complete"] != true || metadata["parse_job_id"] != "job" || metadata["parse_result"] == nil {
		t.Fatalf("concurrent history erased: %s", doc.ContentJSON)
	}
}
