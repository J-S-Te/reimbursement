package application

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"testing"
)

// TestUploadAuditFailureRollsBackLocalResource 验证新文档审计失败时不落本地资源也不调用网关。
// 参数：t 为测试上下文。
// 返回值：无。
func TestUploadAuditFailureRollsBackLocalResource(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	gateway := &repeatUploadGateway{}
	s.deps.Files = gateway
	if e := db.Exec(`CREATE TRIGGER reject_new_document_audit BEFORE INSERT ON exp_outbox WHEN NEW.topic = 'audit' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; e != nil {
		t.Fatal(e)
	}
	a := domain.Actor{TenantID: "t", SubjectID: "app", RequestID: "first-upload", Permissions: []string{"expense.invoice.upload"}}
	raw := json.RawMessage(`{"purpose":"reimbursement.invoice.original","idempotency_key":"new-audited","filename":"a.pdf"}`)
	if _, e := s.CreateUpload(context.Background(), a, raw); e == nil {
		t.Fatal("新资源审计失败仍报告上传会话成功")
	}
	id, _ := digest([]string{"t", "app", "new-audited"})
	var count int64
	db.Model(&store.Document{}).Where("id = ?", id[:32]).Count(&count)
	if count != 0 || gateway.calls != 0 {
		t.Fatal("审计失败留下文档或已调用远端")
	}
}

// TestCompleteUploadAuditsOnceAndReplaysWithoutRemote 验证完成上传的回放不再改版本、排任务或造新审计。
// 参数：t 为测试上下文。
// 返回值：无。
func TestCompleteUploadAuditsOnceAndReplaysWithoutRemote(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	gateway := &repeatUploadGateway{}
	s.deps.Files = gateway
	a := domain.Actor{TenantID: "t", SubjectID: "app", RequestID: "upload-request", Permissions: []string{"expense.invoice.upload"}}
	created, e := s.CreateUpload(context.Background(), a, json.RawMessage(`{"purpose":"reimbursement.invoice.original","idempotency_key":"audited-completion","filename":"a.pdf"}`))
	if e != nil {
		t.Fatal(e)
	}
	var creation struct {
		ID string `json:"document_id"`
	}
	_ = json.Unmarshal(created, &creation)
	a.RequestID = "first-completion"
	first, e := s.CompleteUpload(context.Background(), a, creation.ID)
	if e != nil {
		t.Fatal(e)
	}
	a.RequestID = "retry-completion"
	second, e := s.CompleteUpload(context.Background(), a, creation.ID)
	if e != nil {
		t.Fatal(e)
	}
	if string(first) != string(second) {
		t.Fatal("同一文档完成回放没有返回原回执")
	}
	var audit, jobs int64
	db.Model(&store.Outbox{}).Where("topic = ?", "audit").Count(&audit)
	db.Model(&store.Job{}).Where("resource_id = ?", creation.ID).Count(&jobs)
	if audit != 2 || jobs != 1 {
		t.Fatalf("首次创建/完成应各一审计且仅一解析任务:audit=%d jobs=%d", audit, jobs)
	}
	var doc store.Document
	db.First(&doc, "id = ?", creation.ID)
	if doc.Version != 2 {
		t.Fatal("完成回放又改变了文档版本")
	}
	var rows []store.Outbox
	db.Where("topic = ?", "audit").Find(&rows)
	traces := map[string]bool{}
	for _, row := range rows {
		var data map[string]any
		_ = json.Unmarshal([]byte(row.PayloadJSON), &data)
		if trace, ok := data["request_id"].(string); ok {
			traces[trace] = true
		}
	}
	if !traces["upload-request"] || !traces["first-completion"] || traces["retry-completion"] {
		t.Fatal("生命周期审计没有保留各首次请求的可信追踪")
	}
}

// TestCompletionAuditFailureRollsBackLocalJobOnly 验证远端已就绪事实不被伪回滚，本地失败仍可安全重试。
// 参数：t 为测试上下文。
// 返回值：无。
func TestCompletionAuditFailureRollsBackLocalJobOnly(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	gateway := &repeatUploadGateway{}
	s.deps.Files = gateway
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload"}}
	raw, e := s.CreateUpload(context.Background(), a, json.RawMessage(`{"purpose":"reimbursement.invoice.original","idempotency_key":"complete-audit-fail","filename":"a.pdf"}`))
	if e != nil {
		t.Fatal(e)
	}
	var created struct {
		ID string `json:"document_id"`
	}
	_ = json.Unmarshal(raw, &created)
	if e = db.Exec(`CREATE TRIGGER reject_completion_audit BEFORE INSERT ON exp_outbox WHEN NEW.topic = 'audit' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; e != nil {
		t.Fatal(e)
	}
	if _, e = s.CompleteUpload(context.Background(), a, created.ID); e == nil {
		t.Fatal("本地完成审计失败仍报告成功")
	}
	var doc store.Document
	db.First(&doc, "id = ?", created.ID)
	var jobs int64
	db.Model(&store.Job{}).Where("resource_id = ?", created.ID).Count(&jobs)
	if doc.Status != "UPLOAD_PENDING" || doc.Version != 1 || jobs != 0 {
		t.Fatal("审计失败留下本地READY或解析任务")
	}
	if e = db.Exec("DROP TRIGGER reject_completion_audit").Error; e != nil {
		t.Fatal(e)
	}
	if _, e = s.CompleteUpload(context.Background(), a, created.ID); e != nil {
		t.Fatal("远端已READY时重试未恢复本地提交", e)
	}
	db.Model(&store.Job{}).Where("resource_id = ?", created.ID).Count(&jobs)
	if jobs != 1 {
		t.Fatal("重试未恰好创建一次任务")
	}
}
