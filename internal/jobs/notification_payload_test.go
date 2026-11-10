package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/internal/integration"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestNotificationKindsReachPlatform 用途：核验真实投递请求的分类、深链和稳定幂等键。
// 参数：t 为通知协议测试上下文。
// 返回值：无；误分类、越界链接或受理回执未保存时失败。
func TestNotificationKindsReachPlatform(t *testing.T) {
	for _, suffix := range []string{"APPROVAL_ASSIGNED", "INVOICE_MISMATCH",
		"INVOICE_CORRECTION_REQUESTED", "INVOICE_RECONFIRM_REQUIRED", "INVOICE_FINANCE_REVIEW_REQUIRED"} {
		t.Run(suffix, func(t *testing.T) {
			kind := suffix
			if kind == "APPROVAL_ASSIGNED" {
				kind = ""
			}
			data := map[string]any{"kind": kind, "assignee_subject_id": "user", "claim_id": "claim & other"}
			payload, _ := json.Marshal(data)
			p, record, db, received := notificationDeliveryFixture(t, string(payload))
			if err := p.Outbox(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			if (*received)["event_type"] != "REIMBURSEMENT_"+suffix ||
				(*received)["target_url"] != "/reimbursement/claims?claim_id=claim+%26+other" ||
				(*received)["idempotency_key"] != record.EventID || (*received)["reference_id"] != record.ResourceID {
				t.Fatalf("incorrect platform event: %+v", *received)
			}
			var receipt store.DeliveryReceipt
			if err := db.First(&receipt, "outbox_id = ?", record.ID).Error; err != nil || receipt.Status != "ACCEPTED" {
				t.Fatalf("acceptance missing: %v", err)
			}
		})
	}
}

// TestNotificationRejectsUnknownKind 用途：未知或类型错误的通知不伪装成审批任务。
// 参数：t 为失败关闭测试上下文。
// 返回值：无；非法通知产生HTTP投递或本地成功回执时失败。
func TestNotificationRejectsUnknownKind(t *testing.T) {
	for _, kind := range []any{"NEW_FINANCIAL_EVENT", 7} {
		payload, _ := json.Marshal(map[string]any{"kind": kind, "assignee_subject_id": "user"})
		p, row, db, received := notificationDeliveryFixture(t, string(payload))
		if err := p.Outbox(context.Background(), row); err == nil {
			t.Fatal("unknown notification accepted")
		}
		var count int64
		if err := db.Model(&store.DeliveryReceipt{}).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if len(*received) != 0 || count != 0 {
			t.Fatal("invalid kind was delivered or recorded")
		}
	}
}

// notificationDeliveryFixture 用途：创建真实HTTP客户端和独立内存投递存储。
// 参数：t 为清理生命周期，payload 为冻结通知载荷。
// 返回值：处理器、有效租约、数据库及实际远端收到的JSON。
func notificationDeliveryFixture(t *testing.T, payload string) (*Processor, Record, *gorm.DB, *map[string]any) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = db.AutoMigrate(&store.Outbox{}, &store.DeliveryReceipt{}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Minute)
	row := store.Outbox{ID: "notification", TenantID: "tenant", EventID: "event", ResourceID: "claim & other",
		Topic: "notification", PayloadJSON: payload, Status: "PROCESSING", Version: 2, LeaseOwner: "worker", LeaseUntil: &until}
	if err = db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	received := map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth2/token" {
			_, _ = w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"notification.ingest","expires_in":300}`))
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"ReceiptID": "receipt", "EventID": received["event_id"], "Status": "ACCEPTED"}})
	}))
	t.Cleanup(server.Close)
	p := &Processor{DB: db, License: recoveryLicense{}, Notifications: integration.NewClient(
		server.URL, server.URL, "client", "secret", "notification.ingest")}
	return p, Record{ID: row.ID, TenantID: row.TenantID, EventID: row.EventID, ResourceID: row.ResourceID,
		Topic: row.Topic, PayloadJSON: payload, Version: row.Version, LeaseOwner: row.LeaseOwner, CreatedAt: row.CreatedAt}, db, &received
}

// TestNumericNotificationPersistsMappedReceipt 用途：真实出站ULID映射的远端回执仍能正确归属本地outbox。
// 参数：t为HTTP投递与租约存储测试上下文。
// 返回值：无；收到错误事件或原始数字ID回执时拒绝，不丢失正确受理结果。
func TestNumericNotificationPersistsMappedReceipt(t *testing.T) {
	p, row, db, received := notificationDeliveryFixture(t, `{"assignee_subject_id":"user"}`)
	row.EventID = "01H00000000000000000000000"
	if err := db.Model(&store.Outbox{}).Where("id = ?", row.ID).Update("event_id", row.EventID).Error; err != nil {
		t.Fatal(err)
	}
	if err := p.Outbox(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if (*received)["event_id"] != "REIMBURSEMENT_01H00000000000000000000000" {
		t.Fatal("wire identity invalid")
	}
	var receipt store.DeliveryReceipt
	if err := db.Where("outbox_id = ?", row.ID).Take(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt.ReceiptJSON, "REIMBURSEMENT_01H00000000000000000000000") {
		t.Fatal("wire receipt lost")
	}
}
