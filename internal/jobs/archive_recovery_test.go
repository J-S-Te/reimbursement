package jobs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestControlledArchiveRetryCreatesRecoverableSuccessor 验证重解析后通过受控后继包恢复，旧包边界和财务效果不改变。
// 参数：t为隔离应用与归档worker联合测试上下文。
// 返回值：无。
func TestControlledArchiveRetryCreatesRecoverableSuccessor(t *testing.T) {
	f := newArchiveStateFixture(t)
	db := f.processor.DB
	var document domain.Claim
	raw, _ := json.Marshal(f.request["document_snapshot"])
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{
		&store.Claim{ID: "c", TenantID: "t", Applicant: "app", Version: 7, Status: "VOID", ContentJSON: string(raw)},
		&store.Config{ID: "scope-recovery", TenantID: "t", Kind: "scopes", Version: 1, Published: true, ContentJSON: `{"subject_id":"archivist","tenant_all":true}`},
		&store.InvoiceParseRun{ID: "inflight", TenantID: "t", JobID: "reparse-job", Attempt: 1, DocumentID: "payment-voucher", Status: "RUNNING", CreatedAt: f.cutoff.Add(-time.Second)},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&store.Job{}).Where("id = ?", "job").Update("kind", "ARCHIVE").Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.processor.archive(ctx, f.record); err == nil || err.Error() != "ARCHIVE_PARSE_IN_PROGRESS" {
		t.Fatalf("缺少原始阻断: %v", err)
	}
	completed := time.Now().UTC()
	if err := db.Model(&store.InvoiceParseRun{}).Where("id = ?", "inflight").Updates(map[string]any{"status": "COMPLETED", "completed_at": completed, "result_hash": "immutable"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.processor.archive(ctx, f.record); err == nil || err.Error() != "ARCHIVE_PARSE_COMPLETED_AFTER_REQUEST" {
		t.Fatalf("旧包时间边界被放宽: %v", err)
	}
	// 模拟本次worker已结束并退避，受控重试可终结旧待重投任务。
	if err := db.Model(&store.Job{}).Where("id = ?", "job").Updates(map[string]any{"status": "PENDING", "lease_owner": "", "lease_until": nil}).Error; err != nil {
		t.Fatal(err)
	}
	service := application.New(db, application.Dependencies{})
	actor := domain.Actor{TenantID: "t", SubjectID: "archivist", Permissions: []string{"expense.claim.read", "expense.archive.retry", "expense.archive.read"}}
	command := json.RawMessage(`{"command_id":"recover-cutoff","expected_version":7}`)
	result, err := service.Execute(ctx, actor, "archive.retry", "c", command)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		JobID     string `json:"job_id"`
		ArchiveID string `json:"archive_id"`
	}
	if err = json.Unmarshal(result, &receipt); err != nil || receipt.ArchiveID == "" || receipt.ArchiveID == "archive" {
		t.Fatalf("未建立后继包: %s %v", result, err)
	}
	repeated, err := service.Execute(ctx, actor, "archive.retry", "c", command)
	if err != nil || string(repeated) != string(result) {
		t.Fatalf("重试命令不幂等: %s %v", repeated, err)
	}
	var successor store.Archive
	if err = db.Take(&successor, "id = ?", receipt.ArchiveID).Error; err != nil {
		t.Fatal(err)
	}
	if successor.PackageVersion != 6 || !successor.CreatedAt.After(completed) {
		t.Fatal("后继包版本或截止时间不正确")
	}
	var next store.Job
	if err = db.Take(&next, "id = ?", receipt.JobID).Error; err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Minute)
	if err = db.Model(&next).Updates(map[string]any{"status": "PROCESSING", "lease_owner": "worker", "lease_until": until}).Error; err != nil {
		t.Fatal(err)
	}
	if err = f.processor.archive(ctx, Record{ID: next.ID, TenantID: "t", ResourceID: successor.ID, Version: next.Version, LeaseOwner: "worker"}); err != nil {
		t.Fatal(err)
	}
	var old store.Archive
	db.Take(&old, "id = ?", "archive")
	db.Take(&successor, "id = ?", successor.ID)
	var manifest struct {
		Parent  string               `json:"predecessor_package_id"`
		Payment domain.SpendSnapshot `json:"payment_snapshot"`
	}
	if err = json.Unmarshal([]byte(successor.ManifestJSON), &manifest); err != nil {
		t.Fatal(err)
	}
	if successor.Status != "SEALED" || old.Status != "FAILED" || !old.CreatedAt.Equal(f.cutoff) ||
		manifest.Parent != "archive" || manifest.Payment.ProjectionRevision != 1 {
		t.Fatal("恢复改写旧包或财务版本")
	}
}

// TestArchiveRetryAfterPreArchiveFailure 验证许可等前置失败没有进入归档worker时，受控重试仍可建立合法父链。
// 参数：t为隔离应用与任务测试上下文。
// 返回值：无。
func TestArchiveRetryAfterPreArchiveFailure(t *testing.T) {
	f := newArchiveStateFixture(t)
	db := f.processor.DB
	raw, _ := json.Marshal(f.request["document_snapshot"])
	for _, row := range []any{
		&store.Claim{ID: "c", TenantID: "t", Applicant: "app", Version: 7, Status: "VOID", ContentJSON: string(raw)},
		&store.Config{ID: "scope-recovery", TenantID: "t", Kind: "scopes", Version: 1, Published: true, ContentJSON: `{"subject_id":"archivist","tenant_all":true}`},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&store.Job{}).Where("id = ?", "job").Updates(map[string]any{"kind": "ARCHIVE", "status": "FAILED", "lease_owner": "", "lease_until": nil}).Error; err != nil {
		t.Fatal(err)
	}
	service := application.New(db, application.Dependencies{})
	actor := domain.Actor{TenantID: "t", SubjectID: "archivist", Permissions: []string{"expense.claim.read", "expense.archive.retry", "expense.archive.read"}}
	result, err := service.Execute(context.Background(), actor, "archive.retry", "c", json.RawMessage(`{"command_id":"after-preflight-failure","expected_version":7}`))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		JobID     string `json:"job_id"`
		ArchiveID string `json:"archive_id"`
	}
	if err = json.Unmarshal(result, &receipt); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Minute)
	if err = db.Model(&store.Job{}).Where("id = ?", receipt.JobID).Updates(map[string]any{"status": "PROCESSING", "lease_owner": "worker", "lease_until": until}).Error; err != nil {
		t.Fatal(err)
	}
	if err = f.processor.archive(context.Background(), Record{ID: receipt.JobID, TenantID: "t", ResourceID: receipt.ArchiveID, Version: 1, LeaseOwner: "worker"}); err != nil {
		t.Fatal(err)
	}
	var parent store.Archive
	if err = db.Take(&parent, "id = ?", "archive").Error; err != nil {
		t.Fatal(err)
	}
	if parent.Status != "FAILED" || !parent.CreatedAt.Equal(f.cutoff) {
		t.Fatal("旧包状态或截止边界错误")
	}
}
