package mysql

import "time"

type InvoiceCorrection struct {
	ID             string    `gorm:"primaryKey;size:64" json:"id"`
	TenantID       string    `gorm:"size:64;not null;index" json:"tenant_id"`
	InvoiceID      string    `gorm:"size:64;not null;index" json:"invoice_id"`
	Requester      string    `gorm:"size:128;not null" json:"requester_subject_id"`
	Approver       string    `gorm:"size:128" json:"approver_subject_id,omitempty"`
	Status         string    `gorm:"size:30;not null" json:"status"`
	Version        int64     `json:"version"`
	InvoiceVersion int64     `json:"invoice_version"`
	ContentJSON    string    `gorm:"type:longtext;not null" json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName 指定重大票据更正申请及独立决定表。
// 参数：无。
// 返回值：更正案件表名。
func (InvoiceCorrection) TableName() string { return "exp_invoice_correction" }

type InvoiceControlImpact struct {
	ID             string    `gorm:"primaryKey;size:64" json:"id"`
	TenantID       string    `gorm:"size:64;not null;index" json:"tenant_id"`
	InvoiceID      string    `gorm:"size:64;not null;index" json:"invoice_id"`
	ControlID      string    `gorm:"size:64;not null;uniqueIndex:uq_invoice_control_claim,priority:1" json:"control_id"`
	ClaimID        string    `gorm:"size:64;not null;uniqueIndex:uq_invoice_control_claim,priority:2;index" json:"claim_id"`
	Kind           string    `gorm:"size:30;not null" json:"kind"`
	Status         string    `gorm:"size:40;not null" json:"status"`
	Version        int64     `json:"version"`
	OriginalRound  int       `json:"original_round"`
	InvoiceVersion int64     `json:"invoice_version"`
	ConfirmedBy    string    `gorm:"size:128" json:"confirmed_by,omitempty"`
	ContentJSON    string    `gorm:"type:longtext;not null" json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName 指定票据更正和风险对单据的独立影响状态表。
// 参数：无。
// 返回值：受影响单据表名。
func (InvoiceControlImpact) TableName() string { return "exp_invoice_control_impact" }

type InvoiceRisk struct {
	ID                string    `gorm:"primaryKey;size:64" json:"id"`
	TenantID          string    `gorm:"size:64;not null;index" json:"tenant_id"`
	InvoiceID         string    `gorm:"size:64;not null;index" json:"invoice_id"`
	OriginalInvoiceID string    `gorm:"size:64;not null;index" json:"original_invoice_id"`
	Recorder          string    `gorm:"size:128;not null" json:"recorder_subject_id"`
	Reviewer          string    `gorm:"size:128" json:"reviewer_subject_id,omitempty"`
	Kind              string    `gorm:"size:30;not null" json:"risk_kind"`
	Status            string    `gorm:"size:30;not null" json:"status"`
	Resolution        string    `gorm:"size:40" json:"resolution,omitempty"`
	Version           int64     `json:"version"`
	ContentJSON       string    `gorm:"type:longtext;not null" json:"-"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// TableName 指定红字、作废、退款及已付更正的财务复核案件表。
// 参数：无。
// 返回值：票据风险表名。
func (InvoiceRisk) TableName() string { return "exp_invoice_risk" }
