package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// workerAuditRows 返回后台审计投递记录，按标识稳定排序。
// 参数：t 为测试上下文，db 为隔离库。
// 返回值：审计主题的outbox行；不代表已送达平台。
func workerAuditRows(t *testing.T, db *gorm.DB) []store.Outbox {
	t.Helper()
	var rows []store.Outbox
	if err := db.Where("topic = ?", "audit").Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

// archivePackageJobFixture 组装含封存清单的最小建包任务。
// 参数：t 为测试上下文。
// 返回值：隔离库、档案行、任务行与租约凭证。
func archivePackageJobFixture(t *testing.T) (*gorm.DB, store.Archive, store.Job, Record) {
	t.Helper()
	manifest := archiveManifestFixture(t, map[string]archiveOriginal{"file": {document: "doc", content: []byte("sealed original")}})
	return archivePackageFixture(t, manifest, "package")
}

// TestArchivePackageTerminalFailureWritesSingleAudit 验证重试耗尽只写一条FAILURE审计且退避期间不写。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackageTerminalFailureWritesSingleAudit(t *testing.T) {
	db, archive, job, row := archivePackageJobFixture(t)
	queue := Queue{DB: db, Owner: "worker", Lease: time.Minute}
	ctx := context.Background()
	cause := errors.New("ARCHIVE_PACKAGE_ORIGINAL_UNAVAILABLE")
	for range 2 {
		if err := queue.Finish(ctx, "exp_job", row, cause); err != nil {
			t.Fatal(err)
		}
		var retried store.Job
		if err := db.Take(&retried, "id = ?", job.ID).Error; err != nil {
			t.Fatal(err)
		}
		if retried.Status != "PENDING" {
			t.Fatalf("退避重试未保持待处理: %s", retried.Status)
		}
		if audits := workerAuditRows(t, db); len(audits) != 0 {
			t.Fatalf("退避重试期间重复写审计: %d", len(audits))
		}
		// 让任务到期并重新领取，attempt随Take递增，走真实队列路径。
		if err := db.Model(&store.Job{}).Where("id = ?", job.ID).Update("next_attempt_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		next, err := queue.Take(ctx, "exp_job")
		if err != nil || next == nil {
			t.Fatalf("重新领取任务失败: %v %v", next, err)
		}
		row = *next
	}
	// 模拟重试耗尽：下一次终结判定按attempt>=12进入FAILED。
	if err := db.Model(&store.Job{}).Where("id = ?", job.ID).Updates(map[string]any{"attempt": 12, "version": row.Version + 1}).Error; err != nil {
		t.Fatal(err)
	}
	row.Attempt, row.Version = 12, row.Version+1
	if err := queue.Finish(ctx, "exp_job", row, cause); err != nil {
		t.Fatal(err)
	}
	var terminal store.Job
	if err := db.Take(&terminal, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if terminal.Status != "FAILED" {
		t.Fatalf("重试耗尽后任务仍非终结: %s", terminal.Status)
	}
	audits := workerAuditRows(t, db)
	if len(audits) != 1 {
		t.Fatalf("终态失败审计条数=%d，期望1条", len(audits))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(audits[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["action"] != "archive.package.failed" || payload["result"] != "FAILURE" || payload["actor_type"] != "SYSTEM" ||
		payload["command_id"] != job.ID || payload["request_id"] != job.ID || payload["resource_id"] != archive.ClaimID ||
		payload["reason_code"] != "ARCHIVE_PACKAGE_ORIGINAL_UNAVAILABLE" {
		t.Fatalf("终态失败审计载荷错误: %v", payload)
	}
	// 迟到的同结果提交必须被租约CAS拒绝，不能再写第二条审计。
	if err := queue.Finish(ctx, "exp_job", row, cause); err == nil || err.Error() != "lease no longer owned" {
		t.Fatalf("终态后重复提交未被拒绝: %v", err)
	}
	if audits = workerAuditRows(t, db); len(audits) != 1 {
		t.Fatalf("终态后重复写审计: %d", len(audits))
	}
}

// TestArchivePackagePermanentFailureWritesAudit 验证确定不可恢复错误首次失败即留痕。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackagePermanentFailureWritesAudit(t *testing.T) {
	db, archive, job, row := archivePackageJobFixture(t)
	queue := Queue{DB: db, Owner: "worker", Lease: time.Minute}
	cause := errors.New("INVALID_ARCHIVE_PACKAGE_REQUEST")
	if err := queue.Finish(context.Background(), "exp_job", row, cause); err != nil {
		t.Fatal(err)
	}
	var terminal store.Job
	if err := db.Take(&terminal, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if terminal.Status != "FAILED" || terminal.LastError != "INVALID_ARCHIVE_PACKAGE_REQUEST" {
		t.Fatalf("不可恢复错误未终结或诊断码丢失: %+v", terminal)
	}
	audits := workerAuditRows(t, db)
	if len(audits) != 1 {
		t.Fatalf("不可恢复失败审计条数=%d", len(audits))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(audits[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["action"] != "archive.package.failed" || payload["result"] != "FAILURE" ||
		payload["resource_id"] != archive.ClaimID || payload["reason_code"] != "INVALID_ARCHIVE_PACKAGE_REQUEST" {
		t.Fatalf("不可恢复失败审计载荷错误: %v", payload)
	}
}

// TestNonArchivePackageTerminalFailureKeepsExistingAuditSemantics 验证ARCHIVE/EXPORT终态失败语义未被改动。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestNonArchivePackageTerminalFailureKeepsExistingAuditSemantics(t *testing.T) {
	db, _, job, row := archivePackageJobFixture(t)
	if err := db.Model(&store.Job{}).Where("id = ?", job.ID).Update("kind", "EXPORT").Error; err != nil {
		t.Fatal(err)
	}
	row.Kind = "EXPORT"
	row.Attempt = 12
	queue := Queue{DB: db, Owner: "worker", Lease: time.Minute}
	if err := queue.Finish(context.Background(), "exp_job", row, errors.New("ARCHIVE_FILE_MISSING")); err != nil {
		t.Fatal(err)
	}
	var terminal store.Job
	if err := db.Take(&terminal, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if terminal.Status != "FAILED" {
		t.Fatalf("导出任务未按既有语义终结: %+v", terminal)
	}
	if audits := workerAuditRows(t, db); len(audits) != 0 {
		t.Fatalf("非建包任务新增了失败审计: %d", len(audits))
	}
}
