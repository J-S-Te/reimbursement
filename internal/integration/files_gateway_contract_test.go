package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// gatewayDocumentDB 创建仅供网关协议回归的隔离文档表。
// 参数：t 为测试上下文；status 为本地文档当前状态。
// 返回值：含当前用户文档的内存数据库；初始化失败立即终止测试。
func gatewayDocumentDB(t *testing.T, status string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if err = db.AutoMigrate(&store.Document{}); err != nil {
		t.Fatal(err)
	}
	doc := store.Document{
		ID: "document", TenantID: "tenant", Owner: "applicant", FileID: "file",
		Status: status, Version: 1, ContentJSON: `{"request_hash":"original"}`,
	}
	if err = db.Create(&doc).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

// TestUploadMachineIdentityOmitsUserActor 验证机器上传遵循网关的应用身份约束。
// 参数：t 为测试上下文。
// 返回值：无；声明用户身份、丢失资源绑定或未签发真实票据时失败。
func TestUploadMachineIdentityOmitsUserActor(t *testing.T) {
	db := gatewayDocumentDB(t, "UPLOAD_PENDING")
	created, ticketed := false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth2/token":
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "token", "token_type": "Bearer", "scope": "upload", "expires_in": 900,
			})
		case "/api/v2/upload-sessions":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			// 网关机器令牌没有 User.ID，明确拒绝调用方代报用户身份。
			if _, exists := input["actor_user_id"]; exists {
				w.WriteHeader(http.StatusUnprocessableEntity)
				w.Write([]byte(`{"code":"FILE_ACTOR_MISMATCH"}`))
				return
			}
			if input["resource_type"] != "invoice_document" || input["resource_id"] != "document" ||
				input["binding_type"] != "original" || input["purpose"] != "reimbursement.invoice.original" ||
				input["sha256"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
				t.Errorf("上传资源绑定或摘要改变: %+v", input)
			}
			created = true
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"data":{"upload_id":"upload","file_id":"file","purpose":"invoice-original","status":"CREATED"}}`))
		case "/api/v2/upload-sessions/upload/tickets":
			ticketed = true
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"data":{"upload_id":"upload","file_id":"file","ticket":"once"}}`))
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	f := NewFiles(db, NewClient(server.URL, server.URL, "files", "secret", "upload"))
	payload := json.RawMessage(`{"purpose":"reimbursement.invoice.original","original_name":"test.pdf","media_type":"application/pdf","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","idempotency_key":"key"}`)
	data, err := f.CreateUpload(context.Background(), domain.Actor{
		TenantID: "tenant", SubjectID: "applicant",
	}, "document", payload)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !created || !ticketed || result["ticket"] != "once" || result["document_id"] != "document" {
		t.Fatalf("未完成网关会话与票据链路: %s", data)
	}
	var doc store.Document
	if err = db.First(&doc, "id = ?", "document").Error; err != nil {
		t.Fatal(err)
	}
	if doc.Owner != "applicant" || doc.Status != "UPLOAD_PENDING" {
		t.Fatalf("上传会话不得改变本地归属或伪造完成: %+v", doc)
	}
}

// TestValidateGatewayPurposeChecksExactPolicy 验证网关存储用途与本地业务用途精确对应。
// 参数：t 为测试上下文。
// 返回值：无；拒绝合法用途、放行其它用途或未知用途时失败。
func TestValidateGatewayPurposeChecksExactPolicy(t *testing.T) {
	cases := []struct {
		name, requested, remote string
		allowed                 bool
	}{
		{"invoice", "reimbursement.invoice.original", "invoice-original", true},
		{"voucher", "reimbursement.payment.voucher", "payment-voucher", true},
		{"evidence", "reimbursement.claim.evidence", "claim-evidence", true},
		{"foreign-purpose", "reimbursement.invoice.original", "payment-voucher", false},
		{"unknown-purpose", "reimbursement.unknown", "unknown", false},
		{"unknown-equal-purpose", "reimbursement.unknown", "reimbursement.unknown", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := gatewayDocumentDB(t, "READY")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/oauth2/token" {
					w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"download","expires_in":900}`))
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
					"file_id": "file", "status": "READY", "purpose": tc.remote,
				}})
			}))
			defer server.Close()
			f := NewFiles(db, NewClient(server.URL, server.URL, "files", "secret", "download"))
			err := f.Validate(context.Background(), domain.Actor{
				TenantID: "tenant", SubjectID: "applicant",
			}, []string{"file"}, tc.requested)
			if (err == nil) != tc.allowed {
				t.Fatalf("用途校验结果错误: allowed=%v err=%v", tc.allowed, err)
			}
		})
	}
}
