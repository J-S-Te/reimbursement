package mysql

import "time"

// PaymentBinding 保存当前受占用的渠道流水，付款事实仍保留在独立不可变历史表。
type PaymentBinding struct {
	ID               string `gorm:"primaryKey;size:64"`
	TenantID         string `gorm:"size:64;not null;uniqueIndex:uq_payment_binding_transaction,priority:1"`
	Method           string `gorm:"size:30;not null;uniqueIndex:uq_payment_binding_transaction,priority:2"`
	SourceAccountRef string `gorm:"size:128;not null;uniqueIndex:uq_payment_binding_transaction,priority:3"`
	TransactionNo    string `gorm:"size:128;not null;uniqueIndex:uq_payment_binding_transaction,priority:4"`
	PaymentID        string `gorm:"size:64;not null;uniqueIndex:uq_payment_binding_payment"`
	ClaimID          string `gorm:"size:64;not null;index"`
	CreatedAt        time.Time
}

// TableName 返回当前流水占用的独立物理表名。
// 参数：无。
// 返回值：付款当前绑定表名。
func (PaymentBinding) TableName() string { return "exp_payment_binding" }
