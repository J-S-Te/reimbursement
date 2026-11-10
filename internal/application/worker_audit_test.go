package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestParseFailureAuditIsAtomicAndIdempotent 验证失败运行与系统审计同时落库且不记录任意错误原文。
// 参数：t为隔离数据库测试上下文。
// 返回值：无。
func TestParseFailureAuditIsAtomicAndIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Minute)
	for _, row := range []any{
		&store.Document{ID: "doc", TenantID: "t", Owner: "app", ContentJSON: "{}"},
		&store.Job{ID: "job", TenantID: "t", ResourceID: "doc", Kind: "INVOICE_PARSE",
			Status: "PROCESSING", Version: 1, Attempt: 1, LeaseOwner: "worker", LeaseUntil: &future, PayloadJSON: "{}"},
	} {
		if err = db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := New(db, Dependencies{})
	lease := ParseLease{JobID: "job", Owner: "worker", Version: 1}
	ctx := context.Background()
	if err = s.StartParseAttempt(ctx, "t", "doc", lease); err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`CREATE TRIGGER reject_worker_audit BEFORE INSERT ON exp_outbox WHEN NEW.topic = 'audit' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.FailParseAttempt(ctx, "t", "doc", lease, "PDF_INVALID"); err == nil {
		t.Fatal("审计落库失败必须回滚结果")
	}
	var run store.InvoiceParseRun
	db.Take(&run, "id = ?", "job-1")
	if run.Status != "RUNNING" {
		t.Fatal("失败审计事务未回滚")
	}
	if err = db.Exec("DROP TRIGGER reject_worker_audit").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.FailParseAttempt(ctx, "t", "doc", lease, "arbitrary-sensitive-value"); err != nil {
			t.Fatal(err)
		}
	}
	var audits []store.Outbox
	db.Where("topic = ?", "audit").Find(&audits)
	if len(audits) != 1 {
		t.Fatalf("后台失败审计条数=%d", len(audits))
	}
	var payload map[string]any
	if err = json.Unmarshal([]byte(audits[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["actor_type"] != "SYSTEM" || payload["result"] != "FAILURE" ||
		payload["reason_code"] != "PARSER_FAILED" {
		t.Fatalf("错误审计载荷: %v", payload)
	}
}
