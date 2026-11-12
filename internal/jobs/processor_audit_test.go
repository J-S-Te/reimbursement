package jobs

import (
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestAuditEventIDIsStableULID 验证平台审计要求的 26 位 ULID 由 outbox 事件稳定派生，
// 重试不会生成新事件，避免重复审计或重放漂移。
// 参数：t 为测试上下文。
// 返回值：无；非 ULID、非稳定或不同事件相同 ID 时失败。
func TestAuditEventIDIsStableULID(t *testing.T) {
	first := auditEventID("outbox-event-1")
	if _, err := ulid.ParseStrict(first); err != nil {
		t.Fatalf("audit event id %q is not a ULID: %v", first, err)
	}
	if again := auditEventID("outbox-event-1"); again != first {
		t.Fatalf("audit event id changed across retries: %q != %q", again, first)
	}
	if other := auditEventID("outbox-event-2"); other == first {
		t.Fatalf("different outbox events produced the same audit id: %q", other)
	}
}
