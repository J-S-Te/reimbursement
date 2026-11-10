package mysql

import "time"

type InvoiceParseRun struct {
	ID            string `gorm:"primaryKey;size:64"`
	TenantID      string `gorm:"size:64;not null;uniqueIndex:uq_parse_attempt,priority:1"`
	DocumentID    string `gorm:"size:64;not null;index"`
	JobID         string `gorm:"size:64;not null;uniqueIndex:uq_parse_attempt,priority:2"`
	Attempt       int    `gorm:"not null;uniqueIndex:uq_parse_attempt,priority:3"`
	ParserVersion string `gorm:"size:128;not null"`
	RulesVersion  string `gorm:"size:64;not null"`
	InputSHA256   string `gorm:"size:64;not null"`
	Status        string `gorm:"size:30;not null"`
	ResultHash    string `gorm:"size:64;not null"`
	ResultJSON    string `gorm:"type:longtext;not null"`
	FailureCode   string `gorm:"size:100;not null"`
	CreatedAt     time.Time
	CompletedAt   *time.Time
}

type InvoiceSource struct {
	ID             string `gorm:"primaryKey;size:64"`
	TenantID       string `gorm:"size:64;not null;uniqueIndex:uq_invoice_source,priority:1"`
	InvoiceID      string `gorm:"size:64;not null;uniqueIndex:uq_invoice_source,priority:2;index"`
	DocumentID     string `gorm:"size:64;not null;uniqueIndex:uq_invoice_source,priority:3;index"`
	ParseRunID     string `gorm:"size:64;not null;uniqueIndex:uq_invoice_source,priority:4"`
	CandidateIndex int    `gorm:"not null;uniqueIndex:uq_invoice_source,priority:5"`
	SourcesHash    string `gorm:"size:64;not null"`
	SourcesJSON    string `gorm:"type:longtext;not null"`
	CreatedAt      time.Time
}

// TableName 指定不可变解析运行表。
// 参数：无。
// 返回值：报销独立数据库内的表名。
func (InvoiceParseRun) TableName() string { return "exp_invoice_parse_run" }

// TableName 指定发票原始来源关系表。
// 参数：无。
// 返回值：报销独立数据库内的表名。
func (InvoiceSource) TableName() string { return "exp_invoice_source" }
