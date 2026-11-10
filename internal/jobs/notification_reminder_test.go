package jobs

import "testing"

// TestNotificationReminderKind 用途：催办事件不伪装成首次审批分配。
// 参数：t 为平台通知DTO测试上下文。
// 返回值：无；种类丢失或固定业务坐标发生变化时失败。
func TestNotificationReminderKind(t *testing.T) {
	value, err := notificationPayload(Record{ResourceID: "claim", EventID: "stable-event"}, map[string]any{
		"kind": "APPROVAL_REMINDER", "assignee_subject_id": "reviewer", "claim_id": "claim"})
	if err != nil {
		t.Fatal(err)
	}
	if value["event_type"] != "REIMBURSEMENT_APPROVAL_REMINDER" || value["idempotency_key"] != "stable-event" {
		t.Fatalf("invalid reminder payload %+v", value)
	}
}

// TestNotificationNumericEventID 用途：真实ULID及数字开头摘要满足平台字母开头的事件契约。
// 参数：t为投递DTO测试上下文。
// 返回值：无；事件坐标不稳定或仍被平台格式拒绝时失败。
func TestNotificationNumericEventID(t *testing.T) {
	const event = "01H00000000000000000000000"
	value, err := notificationPayload(Record{ResourceID: "claim", EventID: event}, map[string]any{"assignee_subject_id": "user"})
	if err != nil {
		t.Fatal(err)
	}
	if value["event_id"] != "REIMBURSEMENT_"+event || value["idempotency_key"] != event {
		t.Fatalf("invalid event contract: %+v", value)
	}
}
