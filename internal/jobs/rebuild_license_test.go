package jobs

import (
	"context"
	"testing"
	"time"

	core "github.com/J-S-Te/license-core"
)

type expiredRebuildLicense struct{ now time.Time }

// Check 用途：使用真实许可规则模拟已过期的报销授权。
// 参数：ctx 为取消边界，operation 为待执行的许可操作。
// 返回值：业务写被拒绝，技术服务按正式规则判定。
func (g expiredRebuildLicense) Check(_ context.Context, operation core.Operation) error {
	l := core.License{
		ProtocolVersion: 1, Issuer: core.Issuer, ProductID: core.Product,
		ID: "test-expired-rebuild", CustomerID: "test-customer", InstanceID: "test-instance",
		Environment: "production", Version: 1, IssuedAt: g.now.Unix() - 100,
		NotBefore: g.now.Unix() - 100,
		Applications: []core.Application{{Code: "reimbursement", Kind: "FULL",
			NotBefore: g.now.Unix() - 100, ExpiresAt: g.now.Unix() - 1}},
	}
	return core.Evaluate(l, "reimbursement", operation, g.now, l.InstanceID, l.Environment)
}

// TestExpiredLicenseRejectsRebuildBeforePayload 用途：过期许可必须在重建副作用前拒绝任务。
// 参数：t 为隔离单元测试上下文，不连接数据库和外部服务。
// 返回值：无；重建或未知任务能越过业务许可检查时失败。
func TestExpiredLicenseRejectsRebuildBeforePayload(t *testing.T) {
	p := Processor{License: expiredRebuildLicense{now: time.Now().UTC()}}
	for _, topic := range []string{"project_rebuild", "unreviewed_business"} {
		err := p.Outbox(context.Background(), Record{Topic: topic, PayloadJSON: "invalid"})
		if err == nil || err.Error() != "LICENSE_UNAVAILABLE" {
			t.Errorf("%s crossed expired business gate: %v", topic, err)
		}
	}
	for _, topic := range []string{"project", "project_spend", "audit", "notification"} {
		err := p.Outbox(context.Background(), Record{Topic: topic, PayloadJSON: "invalid"})
		if err == nil || err.Error() != "INVALID_OUTBOX_PAYLOAD" {
			t.Errorf("existing immutable %s delivery lost essential policy: %v", topic, err)
		}
	}
}
