package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

type uploadReplayGateway struct {
	mu                             sync.Mutex
	status, media, purpose, digest string
	responseDigest                 string
	content                        []byte
	ticketCalls                    int
	ticketFailure                  bool
}

// serveHTTP 模拟实际网关会话、单次票据和字节摘要驱动的状态推进协议。
// 参数：w/r 为隔离 HTTP 请求；仅匹配字节真实上传后才推进 READY。
// 返回值：无；非 CREATED 签票、错误声明或摘要不符均拒绝。
func (g *uploadReplayGateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/oauth2/token":
		w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"upload","expires_in":900}`))
	case "/api/v2/upload-sessions":
		var input Upload
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.MediaType != g.media || input.SHA256 != g.digest {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		g.sessionResponse(w)
	case "/api/v2/upload-sessions/upload/tickets":
		g.ticketCalls++
		if g.status != "CREATED" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if g.ticketFailure {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"data":{"upload_id":"upload","file_id":"file","ticket":"once","upload_url":"/file-gateway/api/v2/upload-sessions/upload/content"}}`))
	case "/api/v2/upload-sessions/upload/content":
		content, _ := io.ReadAll(r.Body)
		digest := sha256.Sum256(content)
		if r.Method != http.MethodPut || g.status != "CREATED" || r.Header.Get("Authorization") != "UploadTicket once" ||
			!bytes.Equal(content, g.content) || hex.EncodeToString(digest[:]) != g.digest {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		g.status = "READY"
		w.WriteHeader(http.StatusCreated)
		g.sessionResponse(w)
	case "/api/v2/upload-sessions/upload/complete":
		g.sessionResponse(w)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// sessionResponse 返回当前真实协议状态，不在查询或创建回放中推进成功。
// 参数：w 为响应；会话状态只由上传处理或测试明确的远端状态夹具决定。
// 返回值：无。
func (g *uploadReplayGateway) sessionResponse(w http.ResponseWriter) {
	digest := g.digest
	if g.responseDigest != "" {
		digest = g.responseDigest
	}
	json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
		"upload_id": "upload", "file_id": "file", "status": g.status, "purpose": g.purpose,
		"sha256": digest, "media_type": g.media, "size_bytes": len(g.content),
	}})
}

// TestUploadReadyReplayRejectsDifferentDigest 验证远端 READY 不能掩盖原文件摘要不一致。
// 参数：t 为测试上下文。
// 返回值：无；不同摘要被返回成功或错误覆盖本地追踪资料时失败。
func TestUploadReadyReplayRejectsDifferentDigest(t *testing.T) {
	db := gatewayDocumentDB(t, "UPLOAD_PENDING")
	content := []byte("original immutable bytes")
	digest := sha256.Sum256(content)
	gateway := &uploadReplayGateway{status: "READY", media: "application/pdf", purpose: "invoice-original",
		content: content, digest: hex.EncodeToString(digest[:]), responseDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveHTTP))
	defer server.Close()
	f := NewFiles(db, NewClient(server.URL, server.URL, "files", "secret", "upload"))
	_, err := f.CreateUpload(context.Background(), domain.Actor{TenantID: "tenant", SubjectID: "applicant"},
		"document", replayUploadPayload(gateway, "reimbursement.invoice.original", "application/pdf"))
	var failure *domain.Problem
	if !errors.As(err, &failure) || failure.Code != "FILE_NOT_READY" || gateway.ticketCalls != 0 {
		t.Fatalf("错误摘要未失败关闭: %v", err)
	}
	var doc store.Document
	db.First(&doc, "id = ?", "document")
	if doc.ContentJSON != `{"request_hash":"original"}` || doc.Status != "UPLOAD_PENDING" {
		t.Fatal("错误远端元数据覆盖了原文档追踪")
	}
}

// replayUploadPayload 创建与实际测试字节绑定的上传声明，避免固定假摘要冒充上传成功。
// 参数：g 为隔离网关事实；purpose/media 为客户端业务声明。
// 返回值：当前同一幂等键的真实大小及 SHA256 声明。
func replayUploadPayload(g *uploadReplayGateway, purpose, media string) json.RawMessage {
	data, _ := json.Marshal(Upload{Purpose: purpose, MediaType: media, OriginalName: "original.ofd",
		SizeBytes: int64(len(g.content)), SHA256: g.digest, IdempotencyKey: "same-key"})
	return data
}

// TestUploadReadyReplayDoesNotIssueAnotherTicket 验证字节已上传后的创建回放只返回远端完成事实。
// 参数：t 为测试上下文。
// 返回值：无；READY 重签票、再次传字节或提前改变本地完成状态时失败。
func TestUploadReadyReplayDoesNotIssueAnotherTicket(t *testing.T) {
	db := gatewayDocumentDB(t, "UPLOAD_PENDING")
	content := []byte("same immutable upload bytes")
	digest := sha256.Sum256(content)
	gateway := &uploadReplayGateway{status: "CREATED", media: "application/pdf", purpose: "invoice-original", content: content, digest: hex.EncodeToString(digest[:])}
	server := httptest.NewServer(http.HandlerFunc(gateway.serveHTTP))
	defer server.Close()
	f := NewFiles(db, NewClient(server.URL, server.URL, "files", "secret", "upload"))
	actor := domain.Actor{TenantID: "tenant", SubjectID: "applicant"}
	payload := replayUploadPayload(gateway, "reimbursement.invoice.original", "application/pdf")
	if _, err := f.CreateUpload(context.Background(), actor, "document", payload); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/api/v2/upload-sessions/upload/content", bytes.NewReader(content))
	req.Header.Set("Authorization", "UploadTicket once")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatal("实际测试字节尚未完成上传")
	}
	data, err := f.CreateUpload(context.Background(), actor, "document", payload)
	if err != nil {
		t.Fatalf("READY 回放不应再签票: %v", err)
	}
	var result map[string]any
	json.Unmarshal(data, &result)
	if result["status"] != "READY" || result["sha256"] != gateway.digest || result["document_id"] != "document" || result["purpose"] != "reimbursement.invoice.original" {
		t.Fatalf("未返回真实远端 READY 事实: %s", data)
	}
	if _, exists := result["ticket"]; exists {
		t.Fatal("READY 不得返回伪造票据")
	}
	if gateway.ticketCalls != 1 {
		t.Fatal("READY 回放重签了票据")
	}
	var doc store.Document
	db.First(&doc, "id = ?", "document")
	if doc.Status != "UPLOAD_PENDING" {
		t.Fatal("创建回放不应代替本地完成业务")
	}
	if _, err = f.CompleteUpload(context.Background(), actor, "document"); err != nil {
		t.Fatal(err)
	}
}

// TestUploadPreservesPurposeAndCanonicalMediaBeforeTicketFailure 验证签票失败窗口仍保存正确身份用途和媒体。
// 参数：t 为测试上下文。
// 返回值：无；短用途覆盖本地身份分支或 OFD 别名未规范时失败。
func TestUploadPreservesPurposeAndCanonicalMediaBeforeTicketFailure(t *testing.T) {
	for _, purpose := range []string{"reimbursement.invoice.original", "reimbursement.payment.voucher", "reimbursement.claim.evidence"} {
		t.Run(purpose, func(t *testing.T) {
			db := gatewayDocumentDB(t, "UPLOAD_PENDING")
			content := []byte("unchanged OFD container declaration")
			digest := sha256.Sum256(content)
			media, declared, stored := "application/pdf", "application/pdf", "claim-evidence"
			if purpose == "reimbursement.invoice.original" {
				media, declared, stored = "application/ofd", "application/vnd.ofd", "invoice-original"
			}
			if purpose == "reimbursement.payment.voucher" {
				stored = "payment-voucher"
			}
			gateway := &uploadReplayGateway{status: "CREATED", media: media, purpose: stored, content: content, digest: hex.EncodeToString(digest[:]), ticketFailure: true}
			server := httptest.NewServer(http.HandlerFunc(gateway.serveHTTP))
			defer server.Close()
			f := NewFiles(db, NewClient(server.URL, server.URL, "files", "secret", "upload"))
			_, err := f.CreateUpload(context.Background(), domain.Actor{TenantID: "tenant", SubjectID: "applicant"}, "document", replayUploadPayload(gateway, purpose, declared))
			if err == nil {
				t.Fatal("真实签票失败不得返回成功")
			}
			var doc store.Document
			db.First(&doc, "id = ?", "document")
			var metadata fileMetadata
			json.Unmarshal([]byte(doc.ContentJSON), &metadata)
			if metadata.Purpose != purpose || metadata.MediaType != media || metadata.SHA256 != gateway.digest || metadata.UploadID != "upload" || gateway.ticketCalls != 1 {
				t.Fatalf("失败窗口丢失业务身份或真实摘要: %s", doc.ContentJSON)
			}
		})
	}
}

// TestUploadReplayNonReadyStatesFailClosed 验证进行中和未知终态不重签票、不伪造完成。
// 参数：t 为测试上下文。
// 返回值：无；UPLOADING 缺少可恢复错误或其它状态未失败关闭时失败。
func TestUploadReplayNonReadyStatesFailClosed(t *testing.T) {
	for _, status := range []string{"UPLOADING", "FAILED", "REJECTED", "UNKNOWN", ""} {
		t.Run(status, func(t *testing.T) {
			db := gatewayDocumentDB(t, "UPLOAD_PENDING")
			content := []byte("incomplete upload bytes")
			digest := sha256.Sum256(content)
			gateway := &uploadReplayGateway{status: status, media: "application/pdf", purpose: "invoice-original", content: content, digest: hex.EncodeToString(digest[:])}
			server := httptest.NewServer(http.HandlerFunc(gateway.serveHTTP))
			defer server.Close()
			f := NewFiles(db, NewClient(server.URL, server.URL, "files", "secret", "upload"))
			_, err := f.CreateUpload(context.Background(), domain.Actor{TenantID: "tenant", SubjectID: "applicant"}, "document", replayUploadPayload(gateway, "reimbursement.invoice.original", "application/pdf"))
			if err == nil || gateway.ticketCalls != 0 {
				t.Fatalf("状态%s被重签票或伪装成功: %v", status, err)
			}
			if status == "UPLOADING" {
				var failure *domain.Problem
				if !errors.As(err, &failure) || failure.Code != "FILE_UPLOAD_IN_PROGRESS" || failure.Status != 503 {
					t.Fatalf("进行中状态不明确: %v", err)
				}
			}
		})
	}
}
