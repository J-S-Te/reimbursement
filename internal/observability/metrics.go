package observability

import (
	"context"
	"fmt"
	"gorm.io/gorm"
	"strconv"
	"strings"
	"time"
)

// Metrics 提供无业务标识的持久队列数量和最老积压年龄。
// 参数：ctx 为查询边界，db 为报销专库，now 为指标采样时刻。
// 返回值：Prometheus文本；查询失败不输出伪造零值。
func Metrics(ctx context.Context, db *gorm.DB, now time.Time) (string, error) {
	var body strings.Builder
	body.WriteString("# TYPE reimbursement_queue_records gauge\n# TYPE reimbursement_queue_oldest_age_seconds gauge\n")
	for _, queue := range []struct{ table, kind, name string }{{"exp_outbox", "topic", "outbox"}, {"exp_job", "kind", "job"}} {
		rows, err := db.WithContext(ctx).Table(queue.table).Select(queue.kind + ", status, COUNT(*), MIN(created_at)").Group(queue.kind + ",status").Rows()
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var kind, status string
			var count int64
			var oldest any
			if err = rows.Scan(&kind, &status, &count, &oldest); err != nil {
				rows.Close()
				return "", err
			}
			labels := fmt.Sprintf("queue=%s,kind=%s,status=%s", strconv.Quote(queue.name), strconv.Quote(metricKind(kind)), strconv.Quote(metricStatus(status)))
			fmt.Fprintf(&body, "reimbursement_queue_records{%s} %d\n", labels, count)
			if status == "PENDING" || status == "PROCESSING" || status == "FAILED" {
				stamp := metricTimestamp(oldest)
				age := 0.0
				if !stamp.IsZero() && now.After(stamp) {
					age = now.Sub(stamp).Seconds()
				}
				fmt.Fprintf(&body, "reimbursement_queue_oldest_age_seconds{%s} %.0f\n", labels, age)
			}
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return body.String(), nil
}

// metricKind 将指标维度限制为系统固定投递器和任务类型。
// 参数：kind 为持久队列类型。
// 返回值：有限枚举，不输出未知业务内容。
func metricKind(kind string) string {
	switch kind {
	case "project", "project_spend", "project_rebuild", "audit", "notification", "INVOICE_PARSE", "ARCHIVE", "EXPORT", "ARCHIVE_PACKAGE":
		return kind
	}
	return "UNKNOWN"
}

// metricStatus 限制指标状态枚举，防止错误内容成为标签。
// 参数：status 为队列持久状态。
// 返回值：固定状态或UNKNOWN。
func metricStatus(status string) string {
	switch status {
	case "PENDING", "PROCESSING", "DONE", "FAILED":
		return status
	}
	return "UNKNOWN"
}

// metricTimestamp 兼容MySQL和隔离SQLite聚合时间的驱动表示。
// 参数：raw 为MIN(created_at)的数据库值。
// 返回值：UTC时间；缺失或不认识的旧数据返回零值。
func metricTimestamp(raw any) time.Time {
	if value, ok := raw.(time.Time); ok {
		return value.UTC()
	}
	text := ""
	switch value := raw.(type) {
	case string:
		text = value
	case []byte:
		text = string(value)
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999"} {
		if value, err := time.Parse(layout, text); err == nil {
			return value.UTC()
		}
	}
	return time.Time{}
}
