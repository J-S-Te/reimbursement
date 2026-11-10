package jobs

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"time"

	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// 下载包保留与清理参数集中声明：环境键、默认值、上限均不散落到业务逻辑深处。
const (
	archiveDownloadRetentionHoursKey         = "REIMBURSEMENT_ARCHIVE_DOWNLOAD_RETENTION_HOURS"
	archiveDownloadCleanupBatchKey           = "REIMBURSEMENT_ARCHIVE_DOWNLOAD_CLEANUP_BATCH"
	archiveDownloadCleanupIntervalMinutesKey = "REIMBURSEMENT_ARCHIVE_DOWNLOAD_CLEANUP_INTERVAL_MINUTES"

	defaultArchiveDownloadRetentionHours     = 72
	maxArchiveDownloadRetentionHours         = 24 * 365
	defaultArchiveDownloadCleanupBatch       = 100
	maxArchiveDownloadCleanupBatch           = 5000
	defaultArchiveDownloadCleanupIntervalMin = 30
	maxArchiveDownloadCleanupIntervalMin     = 24 * 60
	// archiveDownloadCleanupMaxBatches 限制单轮清理总量为 Batch×MaxBatches，剩余超期行留待下一轮。
	archiveDownloadCleanupMaxBatches = 20
	archiveDownloadCleanupTimeout    = 2 * time.Minute

	// minArchiveDownloadRetention 是保留期下限，既防误配置误删，也给在途下载留出宽松窗口。
	minArchiveDownloadRetention = time.Hour
)

// ArchiveDownloadRetention 声明一次清理运行的保留期、批次与节奏。
// 参数：TTL 为按创建时间计算的保留期，Batch 为单批上限，Interval 为运行周期，MaxBatches 为单轮批数上限。
// 返回值：无。
type ArchiveDownloadRetention struct {
	TTL        time.Duration
	Batch      int
	Interval   time.Duration
	MaxBatches int
}

// ArchiveDownloadRetentionFromEnv 按既有运行配置风格读取并校验下载包保留参数。
// 参数：get 为环境读取函数，测试可注入隔离映射；键名以报销服务为前缀。
// 返回值：已校验策略；非法值返回错误而不静默退化，缺失可选键使用集中声明的默认值。
func ArchiveDownloadRetentionFromEnv(get func(string) string) (ArchiveDownloadRetention, error) {
	policy := ArchiveDownloadRetention{MaxBatches: archiveDownloadCleanupMaxBatches}
	hours, err := budgetInt(get, archiveDownloadRetentionHoursKey, defaultArchiveDownloadRetentionHours, 1, maxArchiveDownloadRetentionHours)
	if err != nil {
		return policy, err
	}
	policy.TTL = time.Duration(hours) * time.Hour
	batch, err := budgetInt(get, archiveDownloadCleanupBatchKey, defaultArchiveDownloadCleanupBatch, 1, maxArchiveDownloadCleanupBatch)
	if err != nil {
		return policy, err
	}
	policy.Batch = batch
	minutes, err := budgetInt(get, archiveDownloadCleanupIntervalMinutesKey, defaultArchiveDownloadCleanupIntervalMin, 1, maxArchiveDownloadCleanupIntervalMin)
	if err != nil {
		return policy, err
	}
	policy.Interval = time.Duration(minutes) * time.Minute
	return policy, policy.validate()
}

// budgetInt 读取受区间约束的正整数运行参数，空值使用集中声明的默认值。
// 参数：get 为环境读取函数，key 为键名，fallback/minValue/maxValue 为默认值与闭区间。
// 返回值：已校验整数；格式或范围错误只报告键名，不输出敏感值。
func budgetInt(get func(string) string, key string, fallback, minValue, maxValue int) (int, error) {
	raw := get(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		return 0, errors.New("invalid runtime key: " + key)
	}
	return value, nil
}

// validate 拒绝越界或零批次参数，避免无界删除或永远不执行的清理。
// 参数：无。
// 返回值：稳定诊断码或nil。
func (r ArchiveDownloadRetention) validate() error {
	if r.TTL < minArchiveDownloadRetention || r.TTL > time.Duration(maxArchiveDownloadRetentionHours)*time.Hour ||
		r.Batch < 1 || r.Batch > maxArchiveDownloadCleanupBatch ||
		r.Interval < time.Minute || r.Interval > time.Duration(maxArchiveDownloadCleanupIntervalMin)*time.Minute ||
		r.MaxBatches < 1 || r.MaxBatches > archiveDownloadCleanupMaxBatches {
		return errors.New("ARCHIVE_DOWNLOAD_RETENTION_POLICY_INVALID")
	}
	return nil
}

// ArchiveDownloadCleanupReport 记录单轮清理的条数与时间范围，供结构化日志和调用方判断积压。
// 参数：Cutoff 为保留期截止时刻，Batches 为已执行批数，Deleted 为实际删除行数，Oldest/NewestDeleted 为删除范围。
// 返回值：无。
type ArchiveDownloadCleanupReport struct {
	Cutoff        time.Time
	Batches       int
	Deleted       int
	OldestDeleted time.Time
	NewestDeleted time.Time
}

// CleanupArchiveDownloads 按创建时间分批删除超过保留期的下载包，单轮总量受策略上限约束。
// 参数：ctx 为单轮清理边界，policy 为已校验保留策略。
// 返回值：清理条数与删除时间范围；数据库错误原样返回，已提交批次不回滚。
func (p *Processor) CleanupArchiveDownloads(ctx context.Context, policy ArchiveDownloadRetention) (ArchiveDownloadCleanupReport, error) {
	report := ArchiveDownloadCleanupReport{}
	if p.DB == nil {
		return report, errors.New("ARCHIVE_DOWNLOAD_CLEANUP_UNAVAILABLE")
	}
	if err := policy.validate(); err != nil {
		return report, err
	}
	report.Cutoff = time.Now().UTC().Add(-policy.TTL)
	for report.Batches < policy.MaxBatches {
		deleted, oldest, newest, err := p.deleteArchiveDownloadBatch(ctx, report.Cutoff, policy.Batch)
		if err != nil {
			return report, err
		}
		if deleted == 0 {
			break
		}
		report.Batches++
		report.Deleted += deleted
		report.OldestDeleted = earlierTime(report.OldestDeleted, oldest)
		report.NewestDeleted = laterTime(report.NewestDeleted, newest)
		// 不足一批说明本轮已无更早超期行，提前结束避免空查询。
		if deleted < policy.Batch {
			break
		}
	}
	logArchiveDownloadCleanup(report, policy)
	return report, nil
}

// deleteArchiveDownloadBatch 先取有界超期标识再删除，避免无 LIMIT 的大事务清理。
// 参数：ctx 为数据库边界，cutoff 为保留期截止时刻，limit 为单批上限。
// 返回值：实际删除行数、选中行的创建时间范围或数据库错误。
func (p *Processor) deleteArchiveDownloadBatch(ctx context.Context, cutoff time.Time, limit int) (int, time.Time, time.Time, error) {
	// 只按不可变标识删除，不读取包内容；created_at 缺失的旧行不纳入清理范围。
	var expired []struct {
		ID        string
		CreatedAt time.Time
	}
	err := p.DB.WithContext(ctx).Model(&store.ArchiveDownload{}).
		Select("id, created_at").
		Where("created_at IS NOT NULL AND created_at < ?", cutoff).
		Order("created_at ASC").Order("id ASC").Limit(limit).Find(&expired).Error
	if err != nil {
		return 0, time.Time{}, time.Time{}, err
	}
	if len(expired) == 0 {
		return 0, time.Time{}, time.Time{}, nil
	}
	ids := make([]string, 0, len(expired))
	var oldest, newest time.Time
	for _, row := range expired {
		ids = append(ids, row.ID)
		oldest = earlierTime(oldest, row.CreatedAt.UTC())
		newest = laterTime(newest, row.CreatedAt.UTC())
	}
	result := p.DB.WithContext(ctx).Where("id IN ?", ids).Delete(&store.ArchiveDownload{})
	if result.Error != nil {
		return 0, time.Time{}, time.Time{}, result.Error
	}
	return int(result.RowsAffected), oldest, newest, nil
}

// RunArchiveDownloadRetention 按配置周期清理过期下载包，复用worker既有后台循环模式。
// 参数：ctx 为worker生命周期，配置来自既有环境变量机制。
// 返回值：装配/配置错误或进程取消；单轮失败记录告警后继续，不中断任务消费与投递。
func (p *Processor) RunArchiveDownloadRetention(ctx context.Context) error {
	if p.DB == nil {
		return errors.New("ARCHIVE_DOWNLOAD_RETENTION_UNAVAILABLE")
	}
	policy, err := ArchiveDownloadRetentionFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	// 存储保留属于基础设施卫生而非业务读写：许可临时不可用时也必须停止线性累积，
	// 因此这里不追加商业许可门禁，避免积压无限增长。
	slog.Info("archive download retention cleanup scheduled", "retention_hours", policy.TTL.Hours(), "batch", policy.Batch, "interval", policy.Interval.String(), "max_batches", policy.MaxBatches)
	ticker := time.NewTicker(policy.Interval)
	defer ticker.Stop()
	for {
		// 启动即执行一轮，停机期间积压的超期包不必等待第一个周期。
		step, cancel := context.WithTimeout(ctx, archiveDownloadCleanupTimeout)
		report, cleanupErr := p.CleanupArchiveDownloads(step, policy)
		cancel()
		if cleanupErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("archive download retention cleanup failed", "reason", cleanupErr.Error())
		} else if report.Batches == policy.MaxBatches && report.Deleted > 0 {
			// 单轮上限用尽说明可能仍有超期行，提示运维调整批次或周期。
			slog.Warn("archive download retention cleanup reached run cap", "deleted", report.Deleted, "cutoff", formatCleanupTime(report.Cutoff))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// logArchiveDownloadCleanup 只记录聚合条数与删除时间范围，不输出租户、单据或包内容。
// 参数：report 为单轮结果，policy 为生效策略。
// 返回值：无。
func logArchiveDownloadCleanup(report ArchiveDownloadCleanupReport, policy ArchiveDownloadRetention) {
	attributes := []any{"cutoff", formatCleanupTime(report.Cutoff), "retention_hours", policy.TTL.Hours(), "batches", report.Batches, "deleted", report.Deleted, "oldest_deleted", formatCleanupTime(report.OldestDeleted), "newest_deleted", formatCleanupTime(report.NewestDeleted)}
	if report.Deleted == 0 {
		slog.Debug("archive download retention cleanup", attributes...)
		return
	}
	slog.Info("archive download retention cleanup", attributes...)
}

// earlierTime 返回更早的非零时刻，空值不参与比较。
// 参数：current 为已累计最小值，candidate 为候选时刻。
// 返回值：两者中更早的有效时刻。
func earlierTime(current, candidate time.Time) time.Time {
	if candidate.IsZero() {
		return current
	}
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

// laterTime 返回更晚的非零时刻，空值不参与比较。
// 参数：current 为已累计最大值，candidate 为候选时刻。
// 返回值：两者中更晚的有效时刻。
func laterTime(current, candidate time.Time) time.Time {
	if candidate.IsZero() {
		return current
	}
	if current.IsZero() || candidate.After(current) {
		return candidate
	}
	return current
}

// formatCleanupTime 输出UTC毫秒时间，零值保持为空以便日志区分“无删除”。
// 参数：value 为待格式化时刻。
// 返回值：RFC3339毫秒文本或空字符串。
func formatCleanupTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}
