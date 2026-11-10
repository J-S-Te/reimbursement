package jobs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/internal/invoiceparser"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestExportResultAndAuditCommitTogether 验证导出结果与审计原子提交及重投幂等。
// 参数：t为隔离数据库测试上下文。
// 返回值：无。
func TestExportResultAndAuditCommitTogether(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&store.Job{}, &store.Outbox{}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Minute)
	job := store.Job{ID: "export", TenantID: "t", ResourceID: "export", Kind: "EXPORT", Status: "PROCESSING",
		Version: 1, LeaseOwner: "worker", LeaseUntil: &until, PayloadJSON: "{}"}
	if err = db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	row := Record{ID: job.ID, TenantID: job.TenantID, ResourceID: job.ResourceID, Version: 1, LeaseOwner: "worker"}
	p := Processor{DB: db}
	body := []byte(`{"result":{"file_id":"file","sha256":"hash"}}`)
	if err = db.Exec(`CREATE TRIGGER reject_export_audit BEFORE INSERT ON exp_outbox WHEN NEW.topic = 'audit' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err = p.commitExport(context.Background(), row, body); err == nil {
		t.Fatal("审计失败未阻断结果发布")
	}
	db.Take(&job, "id = ?", "export")
	if job.PayloadJSON != "{}" {
		t.Fatal("导出事务未回滚")
	}
	if err = db.Exec("DROP TRIGGER reject_export_audit").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = p.commitExport(context.Background(), row, body); err != nil {
			t.Fatal(err)
		}
	}
	var audits []store.Outbox
	db.Find(&audits)
	if len(audits) != 1 {
		t.Fatalf("重复审计: %d", len(audits))
	}
	var payload map[string]any
	if err = json.Unmarshal([]byte(audits[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if auditActorType(payload) != "SYSTEM" || auditResult(payload) != "SUCCESS" {
		t.Fatal("后台主体或结果错误")
	}
	row.Version = 0
	if err = p.commitExport(context.Background(), row, body); err == nil {
		t.Fatal("旧worker提交成功")
	}
}

// TestParseIssuesKeepCandidateDiagnostics 验证人工复核所需候选问题不会被总体警告覆盖。
// 参数：t为测试上下文。
// 返回值：无。
func TestParseIssuesKeepCandidateDiagnostics(t *testing.T) {
	got := parseIssues(invoiceparser.Result{Warnings: []string{"WARNING"}, Documents: []invoiceparser.Candidate{
		{Issues: []string{"AMOUNT_MISSING", "WARNING"}}, {Issues: []string{"AMBIGUOUS_DOCUMENT"}},
	}})
	if len(got) != 3 || got[1] != "AMOUNT_MISSING" || got[2] != "AMBIGUOUS_DOCUMENT" {
		t.Fatalf("诊断丢失: %v", got)
	}
	if auditActorType(map[string]any{"actor_type": "ADMIN"}) != "USER" ||
		auditResult(map[string]any{"result": "arbitrary"}) != "SUCCESS" {
		t.Fatal("未知审计类型被透传")
	}
}
