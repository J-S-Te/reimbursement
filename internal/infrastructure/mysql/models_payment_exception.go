package mysql

import "time"

type PaymentExceptionFact struct {
	ID               string `gorm:"primaryKey;size:64"`
	TenantID         string `gorm:"size:64;not null;uniqueIndex:uq_exception_transaction,priority:1"`
	ClaimID          string `gorm:"size:64;not null;index"`
	Recorder         string `gorm:"size:128;not null"`
	Method           string `gorm:"size:30;not null;uniqueIndex:uq_exception_transaction,priority:2"`
	SourceAccountRef string `gorm:"size:128;not null;uniqueIndex:uq_exception_transaction,priority:3"`
	TransactionNo    string `gorm:"size:128;not null;uniqueIndex:uq_exception_transaction,priority:4"`
	ContentHash      string `gorm:"size:64;not null"`
	ContentJSON      string `gorm:"type:longtext;not null"`
	CreatedAt        time.Time
}

// TableName 指定外部付款异常的不可变原始事实表。
// 参数：无。
// 返回值：报销异常付款事实表名。
func (PaymentExceptionFact) TableName() string { return "exp_payment_exception_fact" }

type PaymentExceptionCursor struct {
	ID           string `gorm:"primaryKey;size:64"`
	TenantID     string `gorm:"size:64;not null;index"`
	ClaimID      string `gorm:"size:64;not null;index"`
	Status       string `gorm:"size:30;not null"`
	Version      int64  `gorm:"not null"`
	LastReviewID string `gorm:"size:64"`
	UpdatedAt    time.Time
}

// TableName 指定异常核实流程的版本游标，不覆盖不可变事实或决定。
// 参数：无。
// 返回值：付款异常核实游标表名。
func (PaymentExceptionCursor) TableName() string { return "exp_payment_exception_cursor" }

type PaymentExceptionReview struct {
	ID          string `gorm:"primaryKey;size:64"`
	TenantID    string `gorm:"size:64;not null;uniqueIndex:uq_exception_review_version,priority:1"`
	ExceptionID string `gorm:"size:64;not null;uniqueIndex:uq_exception_review_version,priority:2"`
	ClaimID     string `gorm:"size:64;not null;index"`
	Actor       string `gorm:"size:128;not null"`
	Version     int64  `gorm:"not null;uniqueIndex:uq_exception_review_version,priority:3"`
	Decision    string `gorm:"size:40;not null"`
	ContentHash string `gorm:"size:64;not null"`
	ContentJSON string `gorm:"type:longtext;not null"`
	CreatedAt   time.Time
}

// TableName 指定独立财务核实的不可变逐次决定表。
// 参数：无。
// 返回值：付款异常核实决定表名。
func (PaymentExceptionReview) TableName() string { return "exp_payment_exception_review" }
