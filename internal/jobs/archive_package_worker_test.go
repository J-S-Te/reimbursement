package jobs

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// archivePackageFixture 组装隔离库、封存档案与已领取建包任务。
// 参数：t 为测试上下文，manifest 为封存清单，jobID 为任务标识。
// 返回值：临时库、档案行、任务行与租约凭证；不代表真实文件网关。
func archivePackageFixture(t *testing.T, manifest []byte, jobID string) (*gorm.DB, store.Archive, store.Job, Record) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "archive.sqlite")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = db.AutoMigrate(&store.Job{}, &store.Archive{}, &store.ArchiveDownload{}, &store.Outbox{}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifest)
	archive := store.Archive{ID: "archive", TenantID: "t", ClaimID: "claim", PackageVersion: 1, Status: "SEALED", ManifestHash: hex.EncodeToString(sum[:]), ManifestJSON: string(manifest)}
	if err = db.Create(&archive).Error; err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Minute)
	payload, _ := json.Marshal(map[string]any{"archive_id": archive.ID, "manifest_hash": archive.ManifestHash, "claim_ids": []string{archive.ClaimID}})
	job := store.Job{ID: jobID, TenantID: "t", Kind: "ARCHIVE_PACKAGE", ResourceID: archive.ClaimID, Status: "PROCESSING", Version: 1, Attempt: 1, LeaseOwner: "worker", LeaseUntil: &until, PayloadJSON: string(payload)}
	if err = db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	return db, archive, job, Record{ID: job.ID, TenantID: job.TenantID, Kind: job.Kind, ResourceID: job.ResourceID, Version: job.Version, Attempt: job.Attempt, LeaseOwner: job.LeaseOwner, PayloadJSON: job.PayloadJSON}
}

// archiveOriginal 描述测试原件及其本地文档标识。
type archiveOriginal struct {
	document string
	content  []byte
}

// archiveManifestFixture 构造与原件摘要一致的封存清单。
// 参数：files 为 file_id 到原件及文档标识的映射。
// 返回值：不可变清单字节。
func archiveManifestFixture(t *testing.T, files map[string]archiveOriginal) []byte {
	t.Helper()
	items := []map[string]string{}
	for fileID, value := range files {
		sum := sha256.Sum256(value.content)
		items = append(items, map[string]string{"file_id": fileID, "document_id": value.document, "sha256": hex.EncodeToString(sum[:])})
	}
	manifest, err := json.Marshal(map[string]any{"package_version": 1, "files": items})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// TestArchivePackageWorkerCommitsOneVerifiedDownload 验证建包任务生成逐项校验的包、唯一下载行与系统审计。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackageWorkerCommitsOneVerifiedDownload(t *testing.T) {
	original := []byte("sealed invoice original")
	manifest := archiveManifestFixture(t, map[string]archiveOriginal{"file": {document: "doc", content: original}})
	db, archive, _, row := archivePackageFixture(t, manifest, "package")
	p := Processor{DB: db}
	read := func(context.Context, string) ([]byte, error) { return original, nil }
	for range 2 {
		if err := p.generateArchivePackage(context.Background(), row, archive.ID, archive.ManifestHash, read); err != nil {
			t.Fatal(err)
		}
	}
	var downloads []store.ArchiveDownload
	if err := db.Find(&downloads).Error; err != nil {
		t.Fatal(err)
	}
	if len(downloads) != 1 {
		t.Fatalf("同一任务重复生成下载包: %d", len(downloads))
	}
	download := downloads[0]
	sum := sha256.Sum256(download.Content)
	if download.JobID != row.ID || download.ClaimID != archive.ClaimID || download.ArchiveID != archive.ID ||
		download.ManifestHash != archive.ManifestHash || download.SHA256 != hex.EncodeToString(sum[:]) || len(download.SHA256) != 64 || download.TenantID != "t" {
		t.Fatalf("下载行字段不完整: %+v", download)
	}
	reader, err := zip.NewReader(bytes.NewReader(download.Content), int64(len(download.Content)))
	if err != nil || len(reader.File) != 2 {
		t.Fatalf("归档包内容不完整: %v", err)
	}
	for index, name := range []string{"manifest.json", "originals/001.bin"} {
		entry, e := reader.File[index].Open()
		if e != nil {
			t.Fatal(e)
		}
		content, e := io.ReadAll(entry)
		entry.Close()
		if e != nil {
			t.Fatal(e)
		}
		want := original
		if name == "manifest.json" {
			want = manifest
		}
		if !bytes.Equal(content, want) {
			t.Fatalf("%s 内容被修改", name)
		}
	}
	var audits []store.Outbox
	if err = db.Where("topic = ?", "audit").Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 {
		t.Fatalf("建包审计条数=%d", len(audits))
	}
	var payload map[string]any
	if err = json.Unmarshal([]byte(audits[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["action"] != "archive.package.completed" || payload["actor_type"] != "SYSTEM" || payload["result"] != "SUCCESS" || payload["command_id"] != row.ID {
		t.Fatalf("审计载荷错误: %v", payload)
	}
}

// TestArchivePackageWorkerKeepsFailuresDiagnosable 验证缺件、哈希漂移与超限不写半成品且失败可诊断。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackageWorkerKeepsFailuresDiagnosable(t *testing.T) {
	original := []byte("sealed invoice original")
	manifest := archiveManifestFixture(t, map[string]archiveOriginal{"file": {document: "doc", content: original}})
	cases := []struct {
		name       string
		read       func(context.Context, string) ([]byte, error)
		changed    []byte
		manifest   []byte
		manifestOK bool
		want       string
	}{
		{name: "missing-original", read: func(context.Context, string) ([]byte, error) { return nil, errors.New("integration unavailable") }, want: "ARCHIVE_PACKAGE_ORIGINAL_UNAVAILABLE"},
		{name: "changed-original", read: func(context.Context, string) ([]byte, error) { return []byte("changed"), nil }, want: "ARCHIVE_PACKAGE_ORIGINAL_INVALID"},
		{name: "file-limit", manifest: archiveManifestManyFiles(t, 101), manifestOK: true, want: "ARCHIVE_PACKAGE_MANIFEST_INVALID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := manifest
			if c.manifestOK {
				base = c.manifest
			}
			db, archive, job, row := archivePackageFixture(t, base, "package")
			p := Processor{DB: db}
			read := c.read
			if read == nil {
				read = func(context.Context, string) ([]byte, error) { return original, nil }
			}
			cause := p.generateArchivePackage(context.Background(), row, archive.ID, archive.ManifestHash, read)
			if cause == nil || cause.Error() != c.want {
				t.Fatalf("失败诊断=%v，期望 %s", cause, c.want)
			}
			var downloads int64
			if err := db.Model(&store.ArchiveDownload{}).Count(&downloads).Error; err != nil {
				t.Fatal(err)
			}
			if downloads != 0 {
				t.Fatalf("失败仍写入半成品下载行: %d", downloads)
			}
			queue := Queue{DB: db, Owner: "worker", Lease: time.Minute}
			if err := queue.Finish(context.Background(), "exp_job", row, cause); err != nil {
				t.Fatal(err)
			}
			var current store.Job
			if err := db.Take(&current, "id = ?", job.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.Status != "PENDING" || current.NextAttemptAt.Before(time.Now().UTC().Add(-time.Second)) || current.LastError == "" {
				t.Fatalf("失败任务不可重试或不可诊断: %+v", current)
			}
			if current.LastError != c.want {
				t.Fatalf("稳定诊断码被丢弃: %s", current.LastError)
			}
			// 重试上限后终结：另起租约以复核既有次数上限语义。
			until := time.Now().UTC().Add(time.Minute)
			if err := db.Model(&store.Job{}).Where("id = ?", job.ID).Updates(map[string]any{"status": "PROCESSING", "lease_owner": "worker", "lease_until": until, "attempt": 12, "version": current.Version + 1}).Error; err != nil {
				t.Fatal(err)
			}
			row.Attempt = 12
			row.Version = current.Version + 1
			if err := queue.Finish(context.Background(), "exp_job", row, cause); err != nil {
				t.Fatal(err)
			}
			if err := db.Take(&current, "id = ?", job.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.Status != "FAILED" {
				t.Fatalf("重试上限后仍非终结: %+v", current)
			}
		})
	}
}

// archiveManifestManyFiles 构造超过受控件数的清单，不读取任何原件。
// 参数：t 为测试上下文，count 为文件条目数。
// 返回值：超限清单字节。
func archiveManifestManyFiles(t *testing.T, count int) []byte {
	t.Helper()
	items := make([]map[string]string, 0, count)
	for index := range count {
		items = append(items, map[string]string{"file_id": "file-" + string(rune('a'+index%26)) + "-" + string(rune('a'+index/26)), "document_id": "doc", "sha256": hex.EncodeToString(make([]byte, 32))})
	}
	manifest, err := json.Marshal(map[string]any{"package_version": 1, "files": items})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// TestArchivePackageWorkerEnforcesSizeAndState 验证压缩上限、封存状态、摘要绑定与租约失效。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchivePackageWorkerEnforcesSizeAndState(t *testing.T) {
	t.Run("size-limit", func(t *testing.T) {
		first := make([]byte, 11<<20)
		second := make([]byte, 11<<20)
		if _, err := rand.Read(first); err != nil {
			t.Fatal(err)
		}
		if _, err := rand.Read(second); err != nil {
			t.Fatal(err)
		}
		manifest := archiveManifestFixture(t, map[string]archiveOriginal{"a": {document: "doc-a", content: first}, "b": {document: "doc-b", content: second}})
		db, archive, _, row := archivePackageFixture(t, manifest, "package")
		p := Processor{DB: db}
		read := func(_ context.Context, id string) ([]byte, error) {
			if id == "doc-a" {
				return first, nil
			}
			return second, nil
		}
		cause := p.generateArchivePackage(context.Background(), row, archive.ID, archive.ManifestHash, read)
		if cause == nil || cause.Error() != "ARCHIVE_PACKAGE_SIZE_LIMIT" {
			t.Fatalf("超限未拒绝: %v", cause)
		}
		var downloads int64
		db.Model(&store.ArchiveDownload{}).Count(&downloads)
		if downloads != 0 {
			t.Fatal("超限仍写入下载行")
		}
	})
	t.Run("manifest-binding", func(t *testing.T) {
		original := []byte("sealed original")
		manifest := archiveManifestFixture(t, map[string]archiveOriginal{"file": {document: "doc", content: original}})
		db, archive, _, row := archivePackageFixture(t, manifest, "package")
		p := Processor{DB: db}
		read := func(context.Context, string) ([]byte, error) { return original, nil }
		if err := p.generateArchivePackage(context.Background(), row, archive.ID, hex.EncodeToString(make([]byte, 32)), read); err == nil || err.Error() != "ARCHIVE_PACKAGE_MANIFEST_CHANGED" {
			t.Fatalf("摘要漂移未拒绝: %v", err)
		}
		if err := p.generateArchivePackage(context.Background(), row, "other-archive", archive.ManifestHash, read); err == nil || err.Error() != "ARCHIVE_PACKAGE_ARCHIVE_MISSING" {
			t.Fatalf("跨单据档案未隐藏: %v", err)
		}
		if err := db.Model(&store.Archive{}).Where("id = ?", archive.ID).Update("status", "FAILED").Error; err != nil {
			t.Fatal(err)
		}
		if err := p.generateArchivePackage(context.Background(), row, archive.ID, archive.ManifestHash, read); err == nil || err.Error() != "ARCHIVE_NOT_SEALED" {
			t.Fatalf("未封存版本仍建包: %v", err)
		}
	})
	t.Run("lease-lost", func(t *testing.T) {
		original := []byte("sealed original")
		manifest := archiveManifestFixture(t, map[string]archiveOriginal{"file": {document: "doc", content: original}})
		db, archive, _, row := archivePackageFixture(t, manifest, "package")
		p := Processor{DB: db}
		row.Version = 0
		if err := p.generateArchivePackage(context.Background(), row, archive.ID, archive.ManifestHash, func(context.Context, string) ([]byte, error) { return original, nil }); err == nil || err.Error() != "JOB_LEASE_LOST" {
			t.Fatalf("旧worker仍发布结果: %v", err)
		}
		var downloads int64
		db.Model(&store.ArchiveDownload{}).Count(&downloads)
		if downloads != 0 {
			t.Fatal("租约失效仍写入下载行")
		}
	})
}

// TestArchivePackageWorkerValidatesPayload 验证载荷与文件网关缺失时不静默成功。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchivePackageWorkerValidatesPayload(t *testing.T) {
	p := Processor{}
	for _, payload := range []string{`{}`, `{"archive_id":"archive"}`, `{"archive_id":"archive","manifest_hash":"` + hex.EncodeToString(make([]byte, 32)) + `","actor":{"tenant_id":"other"}}`} {
		row := Record{TenantID: "t", ResourceID: "claim", PayloadJSON: payload}
		if err := p.archivePackage(context.Background(), row); err == nil || err.Error() != "ARCHIVE_PACKAGE_REQUEST_INVALID" {
			t.Fatalf("非法载荷被接受: %s %v", payload, err)
		}
	}
	valid, _ := json.Marshal(map[string]any{"archive_id": "archive", "manifest_hash": hex.EncodeToString(make([]byte, 32)), "actor": map[string]any{"tenant_id": "t", "subject_id": "exporter", "permissions": []string{"expense.archive.export"}}})
	if err := p.archivePackage(context.Background(), Record{TenantID: "t", ResourceID: "claim", PayloadJSON: string(valid)}); err == nil || err.Error() != "ARCHIVE_FILE_GATEWAY_UNAVAILABLE" {
		t.Fatalf("文件网关缺失未拒绝: %v", err)
	}
}
