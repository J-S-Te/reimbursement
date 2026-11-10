package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/application"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestParseAuditPersistenceFailureCannotTerminateJob 验证最后一次解析仍不能跳过失败审计而终结任务。
// 参数：t为隔离数据库测试上下文。
// 返回值：无。
func TestParseAuditPersistenceFailureCannotTerminateJob(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Minute)
	job := store.Job{ID: "parse", TenantID: "t", ResourceID: "doc", Kind: "INVOICE_PARSE", Status: "PROCESSING",
		Version: 1, Attempt: 12, LeaseOwner: "worker", LeaseUntil: &until, PayloadJSON: "{}"}
	for _, item := range []any{&job, &store.Document{ID: "doc", TenantID: "t", Owner: "app", ContentJSON: "{}"}} {
		if err = db.Create(item).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := application.New(db, application.Dependencies{})
	lease := application.ParseLease{JobID: job.ID, Owner: "worker", Version: 1}
	ctx := context.Background()
	if err = s.StartParseAttempt(ctx, "t", "doc", lease); err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`CREATE TRIGGER reject_parse_audit BEFORE INSERT ON exp_outbox WHEN NEW.topic = 'audit' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	p := Processor{DB: db, Service: s}
	row := Record{ID: job.ID, TenantID: "t", ResourceID: "doc", Kind: job.Kind, Version: 1, Attempt: 12, LeaseOwner: "worker"}
	cause := p.recordParseFailure(ctx, row, lease, errors.New("PDF_INVALID"))
	q := Queue{DB: db, Owner: "worker", Lease: time.Minute}
	if err = q.Finish(ctx, "exp_job", row, cause); err != nil {
		t.Fatal(err)
	}
	db.Take(&job, "id = ?", "parse")
	var run store.InvoiceParseRun
	db.Take(&run, "id = ?", "parse-12")
	if job.Status != "PENDING" || run.Status != "RUNNING" || job.LastError != "PARSE_FAILURE_PERSISTENCE_UNAVAILABLE" {
		t.Fatalf("审计失败提前终结: job=%s run=%s cause=%s", job.Status, run.Status, job.LastError)
	}
	if err = db.Exec("DROP TRIGGER reject_parse_audit").Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&store.Job{}).Where("id = ?", job.ID).Update("next_attempt_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	retry, err := q.Take(ctx, "exp_job")
	if err != nil || retry == nil {
		t.Fatalf("恢复领取失败: %v", err)
	}
	nextLease := application.ParseLease{JobID: retry.ID, Owner: retry.LeaseOwner, Version: retry.Version}
	if err = s.StartParseAttempt(ctx, "t", "doc", nextLease); err != nil {
		t.Fatal(err)
	}
	cause = p.recordParseFailure(ctx, *retry, nextLease, errors.New("PDF_INVALID"))
	if err = q.Finish(ctx, "exp_job", *retry, cause); err != nil {
		t.Fatal(err)
	}
	db.Take(&job, "id = ?", "parse")
	run = store.InvoiceParseRun{}
	db.Take(&run, "id = ?", "parse-13")
	var audits int64
	db.Model(&store.Outbox{}).Where("topic = ?", "audit").Count(&audits)
	var previous store.InvoiceParseRun
	if err = db.Take(&previous, "id = ?", "parse-12").Error; err != nil {
		t.Fatal(err)
	}
	if previous.Status != "ABANDONED" || previous.FailureCode != "ATTEMPT_SUPERSEDED" {
		t.Fatal("被新尝试接管不应伪称旧租约必然过期")
	}
	if job.Status != "FAILED" || run.Status != "FAILED" || audits != 1 {
		t.Fatalf("恢复后失败与审计未一起终结: job=%s run=%s audits=%d", job.Status, run.Status, audits)
	}
}
