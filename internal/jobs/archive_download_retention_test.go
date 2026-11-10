package jobs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newArchiveDownloadRetentionDB 组装只含下载包表的隔离库，不依赖真实MySQL。
// 参数：t 为测试上下文。
// 返回值：已迁移的临时库；连接数限制为1避免SQLite并发写冲突。
func newArchiveDownloadRetentionDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "retention.sqlite")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = db.AutoMigrate(&store.ArchiveDownload{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// createArchiveDownload 写入指定创建时间的下载包，任务标识保持唯一。
// 参数：t 为测试上下文，db 为隔离库，id 为包标识，createdAt 为创建时间。
// 返回值：无；写入失败即终止测试。
func createArchiveDownload(t *testing.T, db *gorm.DB, id string, createdAt time.Time) {
	t.Helper()
	row := store.ArchiveDownload{ID: id, TenantID: "t", JobID: "job-" + id, ClaimID: "claim", ArchiveID: "archive", ManifestHash: strings.Repeat("a", 64), SHA256: strings.Repeat("b", 64), Content: []byte("package-" + id), CreatedAt: createdAt.UTC()}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

// remainingArchiveDownloads 返回仍存在的下载包标识集合。
// 参数：t 为测试上下文，db 为隔离库。
// 返回值：包标识到存在性的映射，不读取包内容。
func remainingArchiveDownloads(t *testing.T, db *gorm.DB) map[string]bool {
	t.Helper()
	var rows []store.ArchiveDownload
	if err := db.Omit("content").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	for _, row := range rows {
		remaining[row.ID] = true
	}
	return remaining
}

// TestArchiveDownloadRetentionDeletesOnlyExpired 验证超期下载包被清理且未超期行保留。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchiveDownloadRetentionDeletesOnlyExpired(t *testing.T) {
	db := newArchiveDownloadRetentionDB(t)
	now := time.Now().UTC()
	createArchiveDownload(t, db, "expired-old", now.Add(-100*time.Hour))
	createArchiveDownload(t, db, "expired-recent", now.Add(-80*time.Hour))
	createArchiveDownload(t, db, "kept-fresh", now.Add(-time.Hour))
	createArchiveDownload(t, db, "kept-outside", now.Add(-71*time.Hour))
	policy := ArchiveDownloadRetention{TTL: 72 * time.Hour, Batch: 10, Interval: time.Minute, MaxBatches: 2}
	report, err := (&Processor{DB: db}).CleanupArchiveDownloads(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Deleted != 2 || report.Batches != 1 {
		t.Fatalf("清理条数=%d 批数=%d，期望2条1批", report.Deleted, report.Batches)
	}
	remaining := remainingArchiveDownloads(t, db)
	if remaining["expired-old"] || remaining["expired-recent"] {
		t.Fatalf("超期行未清理: %v", remaining)
	}
	if !remaining["kept-fresh"] || !remaining["kept-outside"] || len(remaining) != 2 {
		t.Fatalf("未超期行被误删: %v", remaining)
	}
	if report.OldestDeleted.IsZero() || report.NewestDeleted.IsZero() || report.OldestDeleted.After(report.NewestDeleted) {
		t.Fatalf("删除范围不可观测: %+v", report)
	}
	if report.NewestDeleted.After(report.Cutoff) {
		t.Fatalf("删除范围越过截止时刻: %+v", report)
	}
	if delta := report.Cutoff.Sub(now.Add(-72 * time.Hour)); delta < -time.Minute || delta > time.Minute {
		t.Fatalf("保留期截止时刻偏差过大: %s", delta)
	}
}

// TestArchiveDownloadRetentionHonorsRunCap 验证单轮批次上限生效且下一轮继续清理剩余超期行。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestArchiveDownloadRetentionHonorsRunCap(t *testing.T) {
	db := newArchiveDownloadRetentionDB(t)
	now := time.Now().UTC()
	for index := range 5 {
		createArchiveDownload(t, db, fmt.Sprintf("expired-%d", index), now.Add(-time.Duration(100+index)*time.Hour))
	}
	createArchiveDownload(t, db, "kept", now.Add(-time.Hour))
	policy := ArchiveDownloadRetention{TTL: 72 * time.Hour, Batch: 2, Interval: time.Minute, MaxBatches: 2}
	processor := &Processor{DB: db}
	report, err := processor.CleanupArchiveDownloads(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Deleted != 4 || report.Batches != 2 {
		t.Fatalf("单轮上限未生效: 删除%d条/%d批", report.Deleted, report.Batches)
	}
	remaining := remainingArchiveDownloads(t, db)
	if !remaining["kept"] || !remaining["expired-0"] || len(remaining) != 2 {
		t.Fatalf("上限外行未按创建时间保留: %v", remaining)
	}
	// 下一轮继续清理剩余超期行，未超期行始终保留。
	next, err := processor.CleanupArchiveDownloads(context.Background(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if next.Deleted != 1 {
		t.Fatalf("下一轮未继续清理: %d", next.Deleted)
	}
	if remaining = remainingArchiveDownloads(t, db); !remaining["kept"] || len(remaining) != 1 {
		t.Fatalf("下一轮误删未超期行: %v", remaining)
	}
}

// TestArchiveDownloadRetentionPolicyConfiguration 验证保留参数来自运行配置且非法值失败关闭。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchiveDownloadRetentionPolicyConfiguration(t *testing.T) {
	defaults, err := ArchiveDownloadRetentionFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if defaults.TTL != 72*time.Hour || defaults.Batch != 100 || defaults.Interval != 30*time.Minute || defaults.MaxBatches != 20 {
		t.Fatalf("默认策略不符合集中声明: %+v", defaults)
	}
	overrides := map[string]string{archiveDownloadRetentionHoursKey: "6", archiveDownloadCleanupBatchKey: "50", archiveDownloadCleanupIntervalMinutesKey: "10"}
	custom, err := ArchiveDownloadRetentionFromEnv(func(key string) string { return overrides[key] })
	if err != nil {
		t.Fatal(err)
	}
	if custom.TTL != 6*time.Hour || custom.Batch != 50 || custom.Interval != 10*time.Minute {
		t.Fatalf("运行配置未被采纳: %+v", custom)
	}
	for _, env := range []map[string]string{
		{archiveDownloadRetentionHoursKey: "0"},
		{archiveDownloadRetentionHoursKey: "not-a-number"},
		{archiveDownloadRetentionHoursKey: "8761"},
		{archiveDownloadCleanupBatchKey: "0"},
		{archiveDownloadCleanupBatchKey: "5001"},
		{archiveDownloadCleanupIntervalMinutesKey: "0"},
	} {
		if _, err = ArchiveDownloadRetentionFromEnv(func(key string) string { return env[key] }); err == nil {
			t.Fatalf("非法运行配置被接受: %v", env)
		}
	}
	db := newArchiveDownloadRetentionDB(t)
	for _, invalid := range []ArchiveDownloadRetention{
		{TTL: 30 * time.Minute, Batch: 10, Interval: time.Minute, MaxBatches: 1},
		{TTL: 72 * time.Hour, Batch: 0, Interval: time.Minute, MaxBatches: 1},
		{TTL: 72 * time.Hour, Batch: 10, Interval: 0, MaxBatches: 1},
		{TTL: 72 * time.Hour, Batch: 10, Interval: time.Minute, MaxBatches: 0},
	} {
		if _, err = (&Processor{DB: db}).CleanupArchiveDownloads(context.Background(), invalid); err == nil || err.Error() != "ARCHIVE_DOWNLOAD_RETENTION_POLICY_INVALID" {
			t.Fatalf("非法策略未失败关闭: %+v %v", invalid, err)
		}
	}
	if _, err = (&Processor{}).CleanupArchiveDownloads(context.Background(), defaults); err == nil || err.Error() != "ARCHIVE_DOWNLOAD_CLEANUP_UNAVAILABLE" {
		t.Fatalf("缺失数据库未失败关闭: %v", err)
	}
}

// TestRunArchiveDownloadRetentionCleansOnStartup 验证既有入口触发的后台循环启动即清理且可取消。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestRunArchiveDownloadRetentionCleansOnStartup(t *testing.T) {
	db := newArchiveDownloadRetentionDB(t)
	createArchiveDownload(t, db, "expired", time.Now().UTC().Add(-200*time.Hour))
	createArchiveDownload(t, db, "kept", time.Now().UTC().Add(-time.Hour))
	t.Setenv(archiveDownloadRetentionHoursKey, "72")
	t.Setenv(archiveDownloadCleanupBatchKey, "10")
	t.Setenv(archiveDownloadCleanupIntervalMinutesKey, "60")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (&Processor{DB: db}).RunArchiveDownloadRetention(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for remainingArchiveDownloads(t, db)["expired"] {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("启动后未立即清理超期下载包")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("后台循环未按取消退出: %v", err)
	}
	if remaining := remainingArchiveDownloads(t, db); !remaining["kept"] || remaining["expired"] {
		t.Fatalf("循环清理结果错误: %v", remaining)
	}
}
