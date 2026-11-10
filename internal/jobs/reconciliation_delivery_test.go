package jobs

import (
	"context"
	core "github.com/J-S-Te/license-core"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/internal/integration"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type recoveryLicense struct{}

// Check 仅在隔离测试放行本地工作处理。
// 参数：上下文及授权操作。
// 返回值：无错误。
func (recoveryLicense) Check(context.Context, core.Operation) error { return nil }

// TestNotificationPersistsAcceptanceAndRejectsStaleLease 验证受理回执可靠保存且不冒充送达。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestNotificationPersistsAcceptanceAndRejectsStaleLease(t *testing.T) {
	db, e := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if e != nil {
		t.Fatal(e)
	}
	if e = db.AutoMigrate(&store.Outbox{}, &store.DeliveryReceipt{}); e != nil {
		t.Fatal(e)
	}
	until := time.Now().UTC().Add(time.Minute)
	row := store.Outbox{ID: "notification", TenantID: "t", EventID: "event", ResourceID: "c", Topic: "notification", PayloadJSON: `{"assignee_subject_id":"user"}`, Status: "PROCESSING", Version: 2, LeaseOwner: "worker", LeaseUntil: &until}
	if e = db.Create(&row).Error; e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth2/token" {
			w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"notification.ingest","expires_in":300}`))
			return
		}
		w.Write([]byte(`{"data":{"ReceiptID":"receipt","EventID":"event","Status":"ACCEPTED"}}`))
	}))
	defer server.Close()
	p := Processor{DB: db, License: recoveryLicense{}, Notifications: integration.NewClient(server.URL, server.URL, "client", "secret", "notification.ingest")}
	record := Record{ID: row.ID, TenantID: row.TenantID, EventID: row.EventID, ResourceID: row.ResourceID, Topic: row.Topic, PayloadJSON: row.PayloadJSON, Version: 2, LeaseOwner: "worker", CreatedAt: row.CreatedAt}
	if e = p.Outbox(context.Background(), record); e != nil {
		t.Fatal(e)
	}
	var receipt store.DeliveryReceipt
	if e = db.First(&receipt, "outbox_id = ?", row.ID).Error; e != nil {
		t.Fatal("remote acceptance was discarded", e)
	}
	if receipt.Status != "ACCEPTED" || receipt.ReceiptJSON == "" {
		t.Fatalf("invalid receipt %+v", receipt)
	}
	record.Version = 1
	if e = p.Outbox(context.Background(), record); e == nil {
		t.Fatal("stale worker persisted receipt")
	}
}
