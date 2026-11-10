package observability

import (
	"context"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"strings"
	"testing"
	"time"
)

// TestMetricsExposeFailedQueueAndOldestAge 验证业务队列故障和积压年龄能被监控读取且不含业务标识。
// 参数：t 为临时SQLite监控场景。
// 返回值：无。
func TestMetricsExposeFailedQueueAndOldestAge(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err = db.Create(&store.Outbox{ID: "secret-object-id", TenantID: "private-tenant", Topic: "project", EventID: "event", Status: "FAILED", CreatedAt: now.Add(-2 * time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	body, err := Metrics(context.Background(), db, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `reimbursement_queue_records{queue="outbox",kind="project",status="FAILED"} 1`) || !strings.Contains(body, `reimbursement_queue_oldest_age_seconds{queue="outbox",kind="project",status="FAILED"} 7200`) {
		t.Fatalf("缺少故障指标: %s", body)
	}
	if strings.Contains(body, "secret-object-id") || strings.Contains(body, "private-tenant") {
		t.Fatal("技术指标泄露业务标识")
	}
}
