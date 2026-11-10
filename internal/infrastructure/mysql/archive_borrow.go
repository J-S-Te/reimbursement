package mysql

import "time"

type ArchiveBorrow struct {
	ID                 string     `gorm:"primaryKey;size:64" json:"id"`
	TenantID           string     `gorm:"size:64;not null;index:idx_archive_borrow_inbox,priority:1" json:"-"`
	ArchiveID          string     `gorm:"size:64;not null;index:idx_archive_borrow_archive" json:"archive_id"`
	ClaimID            string     `gorm:"size:64;not null;index:idx_archive_borrow_claim" json:"claim_id"`
	ArchiveVersion     int64      `gorm:"not null" json:"archive_version"`
	ManifestHash       string     `gorm:"size:64;not null" json:"manifest_hash"`
	Requester          string     `gorm:"size:128;not null;index:idx_archive_borrow_inbox,priority:2" json:"requester_subject_id"`
	RequesterTenantAll bool       `gorm:"not null" json:"-"`
	Reason             string     `gorm:"size:2000;not null" json:"reason"`
	RequestedDays      int        `gorm:"not null" json:"requested_days"`
	Status             string     `gorm:"size:20;not null;index:idx_archive_borrow_inbox,priority:3" json:"status"`
	Version            int64      `gorm:"not null" json:"version"`
	Approver           string     `gorm:"size:128" json:"approver_subject_id,omitempty"`
	DecisionReason     string     `gorm:"size:2000" json:"decision_reason,omitempty"`
	JobID              string     `gorm:"size:64;index:idx_archive_borrow_job" json:"job_id,omitempty"`
	ApprovedAt         *time.Time `json:"approved_at,omitempty"`
	ExpiresAt          *time.Time `gorm:"index:idx_archive_borrow_expiry" json:"expires_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// TableName 指定绑定不可变档案版本的受控借阅表。
// 参数：无。
// 返回值：借阅申请与独立审批事实表名。
func (ArchiveBorrow) TableName() string { return "exp_archive_borrow" }
