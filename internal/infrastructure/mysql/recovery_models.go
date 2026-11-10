package mysql

import "time"

type RecoveryCommand struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;uniqueIndex:uq_recovery_command,priority:1"`
	Actor       string `gorm:"size:128;uniqueIndex:uq_recovery_command,priority:2"`
	CommandID   string `gorm:"size:128;uniqueIndex:uq_recovery_command,priority:3"`
	ResourceID  string `gorm:"size:64"`
	Kind        string `gorm:"size:30"`
	Reason      string `gorm:"size:1000"`
	RequestHash string `gorm:"size:64"`
	ResultJSON  string `gorm:"type:longtext"`
	CreatedAt   time.Time
}

// TableName 指定受控重投不可变命令账本。
// 参数：无。
// 返回值：恢复命令表名。
func (RecoveryCommand) TableName() string { return "exp_recovery_command" }

type DeliveryReceipt struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null;index"`
	OutboxID    string `gorm:"size:64;not null;uniqueIndex"`
	Topic       string `gorm:"size:80"`
	Status      string `gorm:"size:30"`
	ReceiptJSON string `gorm:"type:longtext"`
	AcceptedAt  time.Time
}

// TableName 指定远端受理回执表，受理不代表通知送达。
// 参数：无。
// 返回值：受理回执表名。
func (DeliveryReceipt) TableName() string { return "exp_delivery_receipt" }

type ReconciliationCursor struct {
	TenantID        string `gorm:"primaryKey;size:64"`
	RunID           string `gorm:"size:64"`
	Mode            string `gorm:"size:20"`
	Phase           string `gorm:"size:20"`
	RemoteCursor    string `gorm:"size:512"`
	SourceUpdatedAt time.Time
	SourceClaimID   string `gorm:"size:64"`
	Watermark       time.Time
	RunStartedAt    time.Time
	LastCompletedAt *time.Time
	LastFullAt      *time.Time
	LastAttemptAt   *time.Time
	LastError       string `gorm:"size:100"`
	Version         int64
	UpdatedAt       time.Time
}

// TableName 指定逐租户持久对账游标。
// 参数：无。
// 返回值：对账游标表名。
func (ReconciliationCursor) TableName() string { return "exp_reconciliation_cursor" }

type ReconciliationFinding struct {
	ID               string `gorm:"primaryKey;size:64"`
	TenantID         string `gorm:"size:64;uniqueIndex:uq_reconciliation_claim,priority:1"`
	ClaimID          string `gorm:"size:64;uniqueIndex:uq_reconciliation_claim,priority:2"`
	RunID            string `gorm:"size:64"`
	SourceRevision   uint64
	SourceHash       string `gorm:"size:64"`
	RemoteRevision   uint64
	RemoteHash       string `gorm:"size:64"`
	RemoteConsistent bool
	Status           string `gorm:"size:40"`
	Version          int64
	UpdatedAt        time.Time
}

// TableName 指定当前差异索引，历史修复由不可变恢复命令保存。
// 参数：无。
// 返回值：对账差异表名。
func (ReconciliationFinding) TableName() string { return "exp_reconciliation_finding" }
