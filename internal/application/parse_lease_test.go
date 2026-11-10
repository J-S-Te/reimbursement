package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestExpiredParseCannotCommitCandidates 防止旧worker在租约过期后写入票据或覆盖新解析结果。
// 参数：t为测试上下文。
// 返回值：无。
func TestExpiredParseCannotCommitCandidates(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(-time.Second)
	for _, row := range []any{&store.Document{ID: "doc", TenantID: "t", Owner: "applicant", FileID: "file", Status: "READY", Version: 1, ContentJSON: "{}"}, &store.Job{ID: "job", TenantID: "t", ResourceID: "doc", Status: "PROCESSING", Version: 2, LeaseOwner: "worker", LeaseUntil: &until, PayloadJSON: "{}"}} {
		if err = db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := New(db, Dependencies{})
	lease := ParseLease{JobID: "job", Owner: "worker", Version: 2}
	candidates := []domain.Invoice{{InvoiceNumber: "number", FileIDs: []string{"file"}}}
	if err = s.CompleteParseLeased(context.Background(), "t", "doc", candidates, nil, json.RawMessage(`{"status":"PARTIAL"}`), lease); err == nil {
		t.Fatal("expired parser committed")
	}
	var count int64
	db.Model(&store.Invoice{}).Count(&count)
	if count != 0 {
		t.Fatal("expired parser created invoice")
	}
	future := time.Now().UTC().Add(time.Minute)
	db.Model(&store.Job{}).Where("id = ?", "job").Update("lease_until", future)
	if err = s.CompleteParseLeased(context.Background(), "t", "doc", candidates, nil, json.RawMessage(`{"status":"PARTIAL"}`), lease); err != nil {
		t.Fatalf("valid parser failed: %v", err)
	}
	db.Model(&store.Invoice{}).Count(&count)
	if count != 1 {
		t.Fatal("valid parser did not create single candidate")
	}
	if err = s.CompleteParseLeased(context.Background(), "t", "doc", candidates, nil,
		json.RawMessage(`{"status":"PARTIAL"}`), lease); err != nil {
		t.Fatal(err)
	}
	var audits []store.Outbox
	db.Where("topic = ?", "audit").Find(&audits)
	if len(audits) != 1 {
		t.Fatalf("重投应仅保留一次后台完成审计，实际%d", len(audits))
	}
	var payload map[string]any
	if err = json.Unmarshal([]byte(audits[0].PayloadJSON), &payload); err != nil ||
		payload["actor_type"] != "SYSTEM" || payload["action"] != "invoice.parse.completed" {
		t.Fatalf("后台审计主体或动作不正确: %s", audits[0].PayloadJSON)
	}
}
