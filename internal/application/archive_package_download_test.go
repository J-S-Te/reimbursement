package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// archivePackageClaim 建立可完整读取的本人单据及封存档案版本。
// 参数：t 为测试上下文，db 为隔离库。
// 返回值：单据标识与档案行。
func archivePackageClaim(t *testing.T, db *gorm.DB) (string, store.Archive) {
	t.Helper()
	raw, err := encode(domain.Claim{ID: "claim", TenantID: "t", Applicant: "exporter", OrgID: "org"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&store.Claim{ID: "claim", TenantID: "t", Applicant: "exporter", OrgID: "org", ContentJSON: raw, Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
	archive := store.Archive{ID: "archive", TenantID: "t", ClaimID: "claim", PackageVersion: 3, Status: "SEALED", ManifestHash: hex.EncodeToString(bytes.Repeat([]byte{7}, 32)), ManifestJSON: `{"package_version":3,"files":[{"file_id":"file","document_id":"doc","sha256":"` + hex.EncodeToString(bytes.Repeat([]byte{9}, 32)) + `"}]}`}
	if err = db.Create(&archive).Error; err != nil {
		t.Fatal(err)
	}
	return archive.ClaimID, archive
}

// archivePackageActor 只授予归档导出所需的精确权限。
// 参数：subject 为请求主体。
// 返回值：可信身份。
func archivePackageActor(subject string) domain.Actor {
	return domain.Actor{TenantID: "t", SubjectID: subject, Permissions: []string{"expense.archive.read", "expense.archive.export", "expense.claim.read"}}
}

// TestArchivePackageCommandQueuesJobAndFrozenReplay 验证建包命令排队、载荷冻结与相同命令重放不重复建任务。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackageCommandQueuesJobAndFrozenReplay(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	claimID, archive := archivePackageClaim(t, db)
	actor := archivePackageActor("exporter")
	body := map[string]any{"command_id": "package-1", "expected_version": archive.PackageVersion}
	first := runCommand(t, s, actor, "archive.package.create", archive.ID, body)
	if first["status"] != "PENDING" || first["job_id"] == nil || first["job_id"] != first["id"] {
		t.Fatalf("排队回执不完整: %v", first)
	}
	jobID, _ := first["job_id"].(string)
	var job store.Job
	if err := db.Take(&job, "id = ?", jobID).Error; err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Actor        domain.Actor `json:"actor"`
		ArchiveID    string       `json:"archive_id"`
		ManifestHash string       `json:"manifest_hash"`
		ClaimIDs     []string     `json:"claim_ids"`
	}
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if job.Kind != "ARCHIVE_PACKAGE" || job.ResourceID != claimID || job.Status != "PENDING" || job.Version != 1 ||
		payload.ArchiveID != archive.ID || payload.ManifestHash != archive.ManifestHash ||
		payload.Actor.SubjectID != actor.SubjectID || len(payload.ClaimIDs) != 1 || payload.ClaimIDs[0] != claimID {
		t.Fatalf("任务载荷未冻结: %+v %s", job, job.PayloadJSON)
	}
	replay := runCommand(t, s, actor, "archive.package.create", archive.ID, body)
	if replay["job_id"] != first["job_id"] {
		t.Fatalf("相同命令产生新任务: %v %v", first, replay)
	}
	var count int64
	if err := db.Model(&store.Job{}).Where("tenant_id = ? AND kind = ?", "t", "ARCHIVE_PACKAGE").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("命令重放重复建任务: %d", count)
	}
	// 撤权后连冻结结果也不得重放，避免以历史回执继续使用档案导出能力。
	revoked := actor
	revoked.Permissions = []string{"expense.archive.read", "expense.claim.read"}
	_, err := s.Execute(context.Background(), revoked, "archive.package.create", archive.ID, archivePackageJSON(t, body))
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "FORBIDDEN" || problem.Status != 403 {
		t.Fatalf("撤权后重放未被拒绝: %v", err)
	}
}

// TestArchivePackageCommandRejectsUnsealedOrForeignArchive 验证仅封存版本可按版本建包且越界隐藏。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackageCommandRejectsUnsealedOrForeignArchive(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	_, archive := archivePackageClaim(t, db)
	actor := archivePackageActor("exporter")
	expect := func(name string, use domain.Actor, archiveID string, body map[string]any, code string, status int) {
		t.Helper()
		_, err := s.Execute(context.Background(), use, "archive.package.create", archiveID, archivePackageJSON(t, body))
		var problem *domain.Problem
		if !errors.As(err, &problem) || problem.Code != code || problem.Status != status {
			t.Fatalf("%s: 期望 %s/%d，实际 %v", name, code, status, err)
		}
	}
	expect("版本冲突", actor, archive.ID, map[string]any{"command_id": "pkg-version", "expected_version": archive.PackageVersion + 1}, "VERSION_CONFLICT", 409)
	expect("未知档案", actor, "missing-archive", map[string]any{"command_id": "pkg-missing", "expected_version": archive.PackageVersion}, "RESOURCE_NOT_FOUND", 404)
	noExport := actor
	noExport.Permissions = []string{"expense.archive.read", "expense.claim.read"}
	expect("缺少导出权限", noExport, archive.ID, map[string]any{"command_id": "pkg-forbidden", "expected_version": archive.PackageVersion}, "FORBIDDEN", 403)
	if err := db.Model(&store.Archive{}).Where("id = ?", archive.ID).Update("status", "FAILED").Error; err != nil {
		t.Fatal(err)
	}
	expect("未封存版本", actor, archive.ID, map[string]any{"command_id": "pkg-unsealed", "expected_version": archive.PackageVersion}, "ARCHIVE_NOT_SEALED", 409)
	var count int64
	if err := db.Model(&store.Job{}).Where("tenant_id = ? AND kind = ?", "t", "ARCHIVE_PACKAGE").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("被拒命令仍排队: %d", count)
	}
}

// TestArchivePackageStatusAndDownloadReauthorize 验证状态只读摘要、下载逐次鉴权并记录审计。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackageStatusAndDownloadReauthorize(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	claimID, archive := archivePackageClaim(t, db)
	actor := archivePackageActor("exporter")
	queued := runCommand(t, s, actor, "archive.package.create", archive.ID, map[string]any{"command_id": "package-download", "expected_version": archive.PackageVersion})
	jobID, _ := queued["job_id"].(string)
	content := []byte("PK sealed archive bytes")
	sum := sha256.Sum256(content)
	if err := db.Model(&store.Job{}).Where("id = ?", jobID).Update("status", "PROCESSING").Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := s.ReadArchivePackage(ctx, actor, jobID); err == nil {
		t.Fatal("未完成任务仍可下载")
	}
	if err := db.Model(&store.Job{}).Where("id = ?", jobID).Update("status", "DONE").Error; err != nil {
		t.Fatal(err)
	}
	download := store.ArchiveDownload{ID: "download", TenantID: "t", JobID: jobID, ClaimID: claimID, ArchiveID: archive.ID, ManifestHash: archive.ManifestHash, SHA256: hex.EncodeToString(sum[:]), Content: content, CreatedAt: time.Now().UTC()}
	if err := db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	status, err := s.ArchivePackageStatus(ctx, actor, jobID)
	if err != nil {
		t.Fatal(err)
	}
	view := status.(map[string]any)
	if view["status"] != "DONE" || view["sha256"] != download.SHA256 || view["manifest_hash"] != archive.ManifestHash || view["archive_id"] != archive.ID {
		t.Fatalf("状态未返回完成摘要: %v", view)
	}
	if _, held := view["content"]; held {
		t.Fatal("状态接口泄露文件内容")
	}
	for range 2 {
		name, body, err := s.ReadArchivePackage(ctx, actor, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if name != "reimbursement-archive-download.zip" || !bytes.Equal(body, content) {
			t.Fatalf("下载结果不符: %s", name)
		}
	}
	var audits []store.Outbox
	if err := db.Where("topic = ?", "audit").Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	downloads := 0
	for _, row := range audits {
		var payload map[string]any
		if err := json.Unmarshal([]byte(row.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["action"] == "archive.package.download" {
			downloads++
			if payload["actor_subject_id"] != actor.SubjectID || payload["package_sha256"] != download.SHA256 || payload["manifest_hash"] != archive.ManifestHash || payload["archive_id"] != archive.ID {
				t.Fatalf("下载审计载荷错误: %v", payload)
			}
		}
	}
	if downloads != 2 {
		t.Fatalf("每次下载都应留审计: %d", downloads)
	}
	// 撤权后状态与下载都按当前权限拒绝，不认可历史创建时权限。
	revoked := actor
	revoked.Permissions = []string{"expense.archive.read", "expense.claim.read"}
	var problem *domain.Problem
	if _, _, err = s.ReadArchivePackage(ctx, revoked, jobID); !errors.As(err, &problem) || problem.Code != "FORBIDDEN" || problem.Status != 403 {
		t.Fatalf("撤权后仍可下载: %v", err)
	}
	if _, err = s.ArchivePackageStatus(ctx, revoked, jobID); !errors.As(err, &problem) || problem.Code != "FORBIDDEN" || problem.Status != 403 {
		t.Fatalf("撤权后仍可查询: %v", err)
	}
	// 他人任务与越权范围统一隐藏为 404，不暴露存在性。
	other := archivePackageActor("other-exporter")
	if _, _, err = s.ReadArchivePackage(ctx, other, jobID); !errors.As(err, &problem) || problem.Code != "RESOURCE_NOT_FOUND" || problem.Status != 404 {
		t.Fatalf("他人下载任务未隐藏: %v", err)
	}
	// 内容被篡改时以摘要失败拒绝，不返回错误字节。
	if err = db.Model(&store.ArchiveDownload{}).Where("id = ?", download.ID).Update("content", []byte("tampered")).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReadArchivePackage(ctx, actor, jobID); !errors.As(err, &problem) || problem.Code != "ARCHIVE_PACKAGE_HASH_MISMATCH" || problem.Status != 409 {
		t.Fatalf("篡改内容未拒绝: %v", err)
	}
}

// mustJSON 在测试中编码固定请求，编码失败立即终止。
// 参数：t 为测试上下文，value 为请求体。
// 返回值：JSON 字节。
func archivePackageJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestArchivePackagesListsOwnPackageMetadata 验证按档案列包只返回本人包元数据、分页收敛且绝不返回文件内容。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackagesListsOwnPackageMetadata(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	claimID, archive := archivePackageClaim(t, db)
	actor := archivePackageActor("exporter")
	ctx := context.Background()
	jobID := func(name string) string {
		t.Helper()
		queued := runCommand(t, s, actor, "archive.package.create", archive.ID, map[string]any{"command_id": name, "expected_version": archive.PackageVersion})
		id, _ := queued["job_id"].(string)
		if id == "" {
			t.Fatalf("建包回执缺少任务标识: %v", queued)
		}
		return id
	}
	pending := jobID("list-pending")
	processing := jobID("list-processing")
	done := jobID("list-done")
	if err := db.Model(&store.Job{}).Where("id = ?", processing).Update("status", "PROCESSING").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&store.Job{}).Where("id = ?", done).Update("status", "DONE").Error; err != nil {
		t.Fatal(err)
	}
	content := []byte("PK listed archive bytes")
	sum := sha256.Sum256(content)
	download := store.ArchiveDownload{ID: "list-download", TenantID: "t", JobID: done, ClaimID: claimID, ArchiveID: archive.ID, ManifestHash: archive.ManifestHash, SHA256: hex.EncodeToString(sum[:]), Content: content, CreatedAt: time.Now().UTC()}
	if err := db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	// 分两页取完，验证页码、页大小、总数与按任务标识可达的元数据。
	items := []map[string]any{}
	for _, params := range []map[string]string{{"page": "1", "page_size": "2"}, {"page": "2", "page_size": "2"}} {
		value, err := s.ArchivePackages(ctx, actor, archive.ID, params)
		if err != nil {
			t.Fatal(err)
		}
		out := value.(map[string]any)
		if out["total"] != 3 || out["page_size"] != 2 {
			t.Fatalf("分页摘要不符: %v", out)
		}
		page, _ := strconv.Atoi(params["page"])
		if out["page"] != page {
			t.Fatalf("页码不符: %v", out)
		}
		for _, item := range out["items"].([]any) {
			items = append(items, item.(map[string]any))
		}
	}
	if len(items) != 3 {
		t.Fatalf("分页未覆盖全部包: %d", len(items))
	}
	seen := map[string]map[string]any{}
	for _, item := range items {
		id, _ := item["job_id"].(string)
		seen[id] = item
		if item["archive_id"] != archive.ID || item["id"] != id || item["created_at"] == nil {
			t.Fatalf("包元数据不完整: %v", item)
		}
		if _, held := item["content"]; held {
			t.Fatal("列表泄露文件内容")
		}
	}
	for _, id := range []string{pending, processing} {
		item, ok := seen[id]
		if !ok {
			t.Fatalf("缺少未完成包 %s", id)
		}
		if item["sha256"] != "" || item["manifest_hash"] != "" {
			t.Fatalf("未完成包不应返回摘要: %v", item)
		}
	}
	if item := seen[done]; item["status"] != "DONE" || item["sha256"] != download.SHA256 || item["manifest_hash"] != archive.ManifestHash {
		t.Fatalf("已完成包摘要不符: %v", item)
	}
	// 页大小按仓库既有约定收敛到 100，页码缺省为 1。
	value, err := s.ArchivePackages(ctx, actor, archive.ID, map[string]string{"page_size": "500"})
	if err != nil {
		t.Fatal(err)
	}
	if out := value.(map[string]any); out["page_size"] != 100 || out["page"] != 1 || out["total"] != 3 {
		t.Fatalf("分页上限或默认值不符: %v", out)
	}
}

// TestArchivePackagesHidesForeignTenantAndOtherSubjects 验证按档案列包不泄露跨租户单据、他人包与未授权结果。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackagesHidesForeignTenantAndOtherSubjects(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	claimID, archive := archivePackageClaim(t, db)
	actor := archivePackageActor("exporter")
	ctx := context.Background()
	own := runCommand(t, s, actor, "archive.package.create", archive.ID, map[string]any{"command_id": "hide-own", "expected_version": archive.PackageVersion})["job_id"].(string)
	// 同租户具备完整范围的另一位主体也可建包，但其任务标识不是他人可读对象。
	auditor := archivePackageActor("auditor")
	auditor.Permissions = append(auditor.Permissions, "expense.scope.tenant_all")
	foreignSubjectJob := runCommand(t, s, auditor, "archive.package.create", archive.ID, map[string]any{"command_id": "hide-auditor", "expected_version": archive.PackageVersion})["job_id"].(string)
	// 跨租户任务即使指向同一单据和档案版本也不得进入本租户列表。
	raw, err := encode(map[string]any{"actor": domain.Actor{TenantID: "other", SubjectID: "exporter"}, "archive_id": archive.ID, "claim_ids": []string{claimID}})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&store.Job{ID: "foreign-tenant-job", TenantID: "other", Kind: "ARCHIVE_PACKAGE", ResourceID: claimID, PayloadJSON: raw, Status: "DONE", Version: 1, NextAttemptAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	value, err := s.ArchivePackages(ctx, actor, archive.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := value.(map[string]any)
	if out["total"] != 1 {
		t.Fatalf("跨租户或他人任务进入列表: %v", out)
	}
	item := out["items"].([]any)[0].(map[string]any)
	if item["job_id"] != own {
		t.Fatalf("列表返回了非本人包: %v", item)
	}
	if item["job_id"] == foreignSubjectJob {
		t.Fatal("列表返回了他人建包回执")
	}
	// 他人跨租户档案统一按不存在隐藏，不泄露存在性。
	foreign := store.Archive{ID: "foreign-archive", TenantID: "other", ClaimID: "foreign-claim", PackageVersion: 1, Status: "SEALED", ManifestHash: strings.Repeat("c", 64), ManifestJSON: "{}"}
	if err = db.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	var problem *domain.Problem
	if _, err = s.ArchivePackages(ctx, actor, foreign.ID, nil); !errors.As(err, &problem) || problem.Code != "RESOURCE_NOT_FOUND" || problem.Status != 404 {
		t.Fatalf("跨租户档案未隐藏: %v", err)
	}
	// 撤权后列表与建包、状态、下载同口径拒绝，不认可历史建包身份。
	revoked := actor
	revoked.Permissions = []string{"expense.archive.read", "expense.claim.read"}
	if _, err = s.ArchivePackages(ctx, revoked, archive.ID, nil); !errors.As(err, &problem) || problem.Code != "FORBIDDEN" || problem.Status != 403 {
		t.Fatalf("撤权后仍可列包: %v", err)
	}
}
