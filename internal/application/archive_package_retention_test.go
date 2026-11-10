package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// doneArchivePackage 建包后把任务置为完成并写入真实下载行，模拟封装成功但尚未清理的包。
// 参数：t 为测试上下文，s 为服务，db 为隔离库，actor 为建包主体，archive 为封存档案，name 为命令与行标识，content 为包字节。
// 返回值：已完成任务标识与下载行。
func doneArchivePackage(t *testing.T, s *Service, db *gorm.DB, actor domain.Actor, archive store.Archive, claimID, name string, content []byte) (string, store.ArchiveDownload) {
	t.Helper()
	queued := runCommand(t, s, actor, "archive.package.create", archive.ID, map[string]any{"command_id": name, "expected_version": archive.PackageVersion})
	jobID, _ := queued["job_id"].(string)
	if jobID == "" {
		t.Fatalf("%s: 建包回执缺少任务标识: %v", name, queued)
	}
	if err := db.Model(&store.Job{}).Where("id = ?", jobID).Update("status", "DONE").Error; err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	download := store.ArchiveDownload{ID: "download-" + name, TenantID: actor.TenantID, JobID: jobID, ClaimID: claimID, ArchiveID: archive.ID, ManifestHash: archive.ManifestHash, SHA256: hex.EncodeToString(sum[:]), Content: content, CreatedAt: time.Now().UTC()}
	if err := db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	return jobID, download
}

// TestArchivePackageStatusAndDownloadAfterRetentionCleanup 验证保留期清理删除下载行后状态不再是500而是EXPIRED，下载返回受控409。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackageStatusAndDownloadAfterRetentionCleanup(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	claimID, archive := archivePackageClaim(t, db)
	actor := archivePackageActor("exporter")
	ctx := context.Background()
	keptJob, kept := doneArchivePackage(t, s, db, actor, archive, claimID, "retained", []byte("PK retained archive bytes"))
	expiredJob, _ := doneArchivePackage(t, s, db, actor, archive, claimID, "purged", []byte("PK purged archive bytes"))
	// 与保留期清理等价：只删除下载行，任务本身保持 DONE。
	if err := db.Where("tenant_id = ? AND job_id = ?", "t", expiredJob).Delete(&store.ArchiveDownload{}).Error; err != nil {
		t.Fatal(err)
	}
	var remaining int64
	if err := db.Model(&store.ArchiveDownload{}).Where("job_id = ?", expiredJob).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("清理夹具未删除下载行: %d", remaining)
	}

	// 未过期的包状态与下载行为必须完全不变。
	status, err := s.ArchivePackageStatus(ctx, actor, keptJob)
	if err != nil {
		t.Fatal(err)
	}
	if view := status.(map[string]any); view["status"] != "DONE" || view["sha256"] != kept.SHA256 || view["manifest_hash"] != archive.ManifestHash || view["archive_id"] != archive.ID {
		t.Fatalf("未过期包状态被改变: %v", view)
	}
	name, body, err := s.ReadArchivePackage(ctx, actor, keptJob)
	if err != nil {
		t.Fatal(err)
	}
	if name != "reimbursement-archive-"+kept.ID+".zip" || !bytes.Equal(body, kept.Content) {
		t.Fatalf("未过期包下载结果不符: %s", name)
	}

	// 状态查询是只读轮询：返回 200 语义（无错误）且 status 指示已过期，摘要为空。
	status, err = s.ArchivePackageStatus(ctx, actor, expiredJob)
	if err != nil {
		t.Fatalf("清理后状态查询不应该是错误: %v", err)
	}
	view := status.(map[string]any)
	if view["status"] != "EXPIRED" || view["sha256"] != "" || view["manifest_hash"] != "" {
		t.Fatalf("清理后状态未指示过期: %v", view)
	}
	if view["id"] != expiredJob {
		t.Fatalf("清理后状态丢失任务标识: %v", view)
	}
	if _, held := view["content"]; held {
		t.Fatal("状态接口泄露文件内容")
	}

	// 下载是实际取物动作：受控 409，错误码稳定且可提示重新生成。
	var problem *domain.Problem
	_, _, err = s.ReadArchivePackage(ctx, actor, expiredJob)
	if !errors.As(err, &problem) || problem.Code != "ARCHIVE_PACKAGE_EXPIRED" || problem.Status != 409 {
		t.Fatalf("清理后下载未返回受控过期错误: %v", err)
	}
	if problem.Message == "" {
		t.Fatal("过期错误缺少可提示文案")
	}
	// 过期下载不写入审计：没有实际取物就不留下下载成功痕迹。
	var audits []store.Outbox
	if err := db.Where("topic = ?", "audit").Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	downloads := 0
	for _, row := range audits {
		if bytes.Contains([]byte(row.PayloadJSON), []byte("archive.package.download")) {
			downloads++
		}
	}
	if downloads != 1 {
		t.Fatalf("只有未过期下载留审计: %d", downloads)
	}
}

// TestArchivePackageExpiredOnlyForMissingDownloadRow 验证只有下载行缺失才收敛为过期，其它数据库错误原样上抛。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackageExpiredOnlyForMissingDownloadRow(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	claimID, archive := archivePackageClaim(t, db)
	actor := archivePackageActor("exporter")
	ctx := context.Background()
	jobID, _ := doneArchivePackage(t, s, db, actor, archive, claimID, "db-error", []byte("PK unreachable archive bytes"))
	// 删除整张下载表制造“非行不存在”的数据库错误，任务与权限链路仍完好。
	if err := db.Migrator().DropTable(&store.ArchiveDownload{}); err != nil {
		t.Fatal(err)
	}
	var problem *domain.Problem
	_, _, err := s.ReadArchivePackage(ctx, actor, jobID)
	if err == nil {
		t.Fatal("数据库故障下仍可下载")
	}
	if errors.As(err, &problem) {
		t.Fatalf("数据库故障被误判为业务错误: %v", problem)
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("数据库故障被误判为行不存在: %v", err)
	}
	status, err := s.ArchivePackageStatus(ctx, actor, jobID)
	if err == nil {
		t.Fatalf("数据库故障下状态查询被吞成成功: %v", status)
	}
	if errors.As(err, &problem) {
		t.Fatalf("状态查询的数据库故障被误判为业务错误: %v", problem)
	}
}
