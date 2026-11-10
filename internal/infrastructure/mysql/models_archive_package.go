package mysql

import "time"

type ArchiveDownload struct {
	ID           string `gorm:"primaryKey;size:64"`
	TenantID     string `gorm:"size:64;not null;index"`
	JobID        string `gorm:"size:64;not null;uniqueIndex"`
	ClaimID      string `gorm:"size:64;not null;index"`
	ArchiveID    string `gorm:"size:64;not null"`
	ManifestHash string `gorm:"size:64;not null"`
	SHA256       string `gorm:"size:64;not null"`
	Content      []byte `gorm:"type:longblob;not null"`
	CreatedAt    time.Time
}

// TableName 指定不可变下载包持久表，原件仍由文件网关管理。
// 参数：无。
// 返回值：按需生成且可逐次授权下载的包表名。
func (ArchiveDownload) TableName() string { return "exp_archive_download" }
