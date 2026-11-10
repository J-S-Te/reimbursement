package jobs

import (
	"context"
	"errors"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"testing"
	"time"
)

// TestExpiredLeaseCanBeRecoveredWithoutStaleCompletion 防止失联任务永远卡死或旧worker覆盖新租约。
// 参数：t为测试上下文。
// 返回值：无。
func TestExpiredLeaseCanBeRecoveredWithoutStaleCompletion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&store.Job{}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if err := db.Create(&store.Job{ID: "j1", TenantID: "t1", Kind: "INVOICE_PARSE", Status: "PROCESSING", Version: 2, LeaseOwner: "lost", LeaseUntil: &past, NextAttemptAt: past, PayloadJSON: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	q := Queue{DB: db, Owner: "new", Lease: time.Minute}
	row, err := q.Take(context.Background(), "exp_job")
	if err != nil || row == nil {
		t.Fatalf("recover: %v", err)
	}
	if row.Version != 3 || row.LeaseOwner != "new" {
		t.Fatalf("wrong lease %+v", row)
	}
	old := *row
	old.LeaseOwner = "lost"
	old.Version = 2
	if err = q.Finish(context.Background(), "exp_job", old, nil); err == nil {
		t.Fatal("stale worker completed current job")
	}
	if err = q.Finish(context.Background(), "exp_job", *row, nil); err != nil {
		t.Fatal(err)
	}
	var got store.Job
	db.First(&got, "id = ?", "j1")
	if got.Status != "DONE" {
		t.Fatal(got.Status)
	}
}

// TestMalformedDocumentDoesNotRetryForever 防止永久损坏票据反复消耗解析资源且丢失可处理原因。
// 参数：t为测试上下文。
// 返回值：无。
func TestMalformedDocumentDoesNotRetryForever(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&store.Job{}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Minute)
	row := store.Job{ID: "bad", TenantID: "t", Kind: "INVOICE_PARSE", Status: "PROCESSING", Version: 1, LeaseOwner: "worker", LeaseUntil: &until, PayloadJSON: "{}"}
	if err = db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	q := Queue{DB: db, Owner: "worker", Lease: time.Minute}
	if err = q.Finish(context.Background(), "exp_job", Record{ID: "bad", Version: 1, LeaseOwner: "worker", Attempt: 1}, errors.New("NO_USABLE_TEXT_LAYER")); err != nil {
		t.Fatal(err)
	}
	db.First(&row, "id = ?", "bad")
	if row.Status != "FAILED" || row.LastError != "NO_USABLE_TEXT_LAYER" {
		t.Fatalf("permanent parse failure hidden/retried: %+v", row)
	}
}

// TestExpiredArchiveLeaseCannotSeal 防止已被接管的归档worker发布完成结果。
// 参数：t为测试上下文。
// 返回值：无。
func TestExpiredArchiveLeaseCannotSeal(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&store.Job{}, &store.Archive{}); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().UTC().Add(-time.Second)
	for _, record := range []any{&store.Job{ID: "job", TenantID: "t", Status: "PROCESSING", Version: 2, LeaseOwner: "old", LeaseUntil: &expired, PayloadJSON: "{}"}, &store.Archive{ID: "archive", TenantID: "t", ClaimID: "claim", PackageVersion: 1, Status: "PENDING", ManifestJSON: "{}"}} {
		if err = db.Create(record).Error; err != nil {
			t.Fatal(err)
		}
	}
	p := Processor{DB: db}
	row := Record{ID: "job", TenantID: "t", Version: 2, LeaseOwner: "old"}
	err = p.withLease(context.Background(), row, func(tx *gorm.DB) error {
		return tx.Model(&store.Archive{}).Where("id = ?", "archive").Update("status", "SEALED").Error
	})
	if err == nil {
		t.Fatal("expired worker sealed archive")
	}
	var archive store.Archive
	db.First(&archive, "id = ?", "archive")
	if archive.Status != "PENDING" {
		t.Fatal("archive changed despite lease rejection")
	}
}
