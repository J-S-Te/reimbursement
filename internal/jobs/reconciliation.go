package jobs

import (
	"context"
	"encoding/json"
	"errors"
	core "github.com/J-S-Te/license-core"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/internal/integration"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// RunReconciliation 逐页发现源端租户并运行持久对账，单次只处理一个最小页。
// 参数：ctx 为worker生命周期；依赖复用Processor既有专用客户端和授权门禁。
// 返回值：取消或配置错误；可恢复远端故障保留游标并在下一轮重试。
func (p *Processor) RunReconciliation(ctx context.Context) error {
	if p.Service == nil || p.Project == nil || p.License == nil {
		return errors.New("RECONCILIATION_UNAVAILABLE")
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	after := ""
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if err := p.License.Check(ctx, core.ESSENTIAL_SERVICE); err != nil {
			continue
		}
		tenants, err := p.Service.ReconciliationTenants(ctx, after, 20)
		if err != nil {
			return err
		}
		if len(tenants) == 0 {
			after = ""
			continue
		}
		for _, tenant := range tenants {
			step, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err = p.Service.ReconcileStep(step, tenant, &integration.ProjectReconciliation{Client: p.Project}, 20)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if failure := p.Service.RecordReconciliationFailure(ctx, tenant, err); failure != nil {
					return failure
				}
				continue
			}
		}
		after = tenants[len(tenants)-1]
	}
}

// persistAcceptance 在当前outbox租约内保存受理回执，不据此声明通知已送达。
// 参数：ctx 为数据库边界，row 为投递租约，raw 为有界远端响应。
// 返回值：回执或租约校验失败，失败会使队列保留可重试状态。
func (p *Processor) persistAcceptance(ctx context.Context, row Record, raw json.RawMessage) error {
	if p.DB == nil {
		return errors.New("DELIVERY_RECEIPT_UNAVAILABLE")
	}
	if !json.Valid(raw) || len(raw) > 4<<20 {
		return errors.New("INVALID_DELIVERY_RECEIPT")
	}
	return p.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current store.Outbox
		q := tx.Where("tenant_id = ? AND id = ? AND version = ? AND lease_owner = ? AND status = ? AND lease_until > ?", row.TenantID, row.ID, row.Version, row.LeaseOwner, "PROCESSING", time.Now().UTC())
		if tx.Dialector.Name() == "mysql" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if e := q.First(&current).Error; e != nil {
			return errors.New("OUTBOX_LEASE_LOST")
		}
		receipt := store.DeliveryReceipt{ID: row.ID, TenantID: row.TenantID, OutboxID: row.ID, Topic: row.Topic, Status: "ACCEPTED", ReceiptJSON: string(raw), AcceptedAt: time.Now().UTC()}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "outbox_id"}}, DoUpdates: clause.Assignments(map[string]any{"receipt_json": string(raw), "status": "ACCEPTED", "accepted_at": receipt.AcceptedAt})}).Create(&receipt).Error
	})
}

// notificationAcceptance 校验平台受理标识与事件归属，不将消费完成等同收件人送达。
// 参数：raw 为平台当前PascalCase或兼容蛇形回执，event 为冻结事件ID。
// 返回值：无效或不属于此事件时返回明确错误。
func notificationAcceptance(raw json.RawMessage, event string) error {
	var receipt struct {
		ReceiptID, EventID, Status string
		SnakeReceiptID             string `json:"receipt_id"`
		SnakeEventID               string `json:"event_id"`
	}
	if json.Unmarshal(raw, &receipt) != nil {
		return errors.New("INVALID_NOTIFICATION_RECEIPT")
	}
	if receipt.ReceiptID == "" {
		receipt.ReceiptID = receipt.SnakeReceiptID
	}
	if receipt.EventID == "" {
		receipt.EventID = receipt.SnakeEventID
	}
	if receipt.ReceiptID == "" || receipt.EventID != event {
		return errors.New("INVALID_NOTIFICATION_RECEIPT")
	}
	switch receipt.Status {
	case "ACCEPTED", "PROCESSING", "COMPLETED", "RETRY", "DEAD", "CANCELLED":
		return nil
	default:
		return errors.New("INVALID_NOTIFICATION_RECEIPT")
	}
}
