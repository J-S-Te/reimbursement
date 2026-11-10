package jobs

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

const archiveBundleMaxBytes = 20 << 20

type archiveBundleBuffer struct{ bytes.Buffer }

// Write 限制压缩结果的实际大小，防止有界原件集合生成无界缓存。
// 参数：value 为ZIP编码器产生的压缩字节。
// 返回值：写入字节数；超过包上限时拒绝。
func (b *archiveBundleBuffer) Write(value []byte) (int, error) {
	if b.Len()+len(value) > archiveBundleMaxBytes {
		return 0, errors.New("ARCHIVE_PACKAGE_SIZE_LIMIT")
	}
	return b.Buffer.Write(value)
}

// buildArchiveBundle 按不可变清单构造逐文件校验的有界ZIP包。
// 参数：ctx 为取消边界，manifest 为封存清单，read 为按文档标识读取原件的函数。
// 返回值：完整ZIP字节；缺件、哈希变化或超限返回错误。
func buildArchiveBundle(ctx context.Context, manifest []byte, read func(context.Context, string) ([]byte, error)) ([]byte, error) {
	var frozen struct {
		Files []struct {
			FileID     string `json:"file_id"`
			DocumentID string `json:"document_id"`
			SHA256     string `json:"sha256"`
		} `json:"files"`
	}
	if len(manifest) > 20<<20 || json.Unmarshal(manifest, &frozen) != nil || len(frozen.Files) == 0 || len(frozen.Files) > 100 {
		return nil, errors.New("ARCHIVE_PACKAGE_MANIFEST_INVALID")
	}
	buffer := &archiveBundleBuffer{}
	writer := zip.NewWriter(buffer)
	if err := writeArchiveMember(writer, "manifest.json", manifest); err != nil {
		return nil, err
	}
	total := 0
	seen := map[string]bool{}
	for i, file := range frozen.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file.DocumentID == "" || file.FileID == "" || seen[file.FileID] {
			return nil, errors.New("ARCHIVE_PACKAGE_FILE_INVALID")
		}
		seen[file.FileID] = true
		content, err := read(ctx, file.DocumentID)
		if err != nil {
			return nil, err
		}
		total += len(content)
		sum := sha256.Sum256(content)
		if len(content) == 0 || total > 100<<20 || hex.EncodeToString(sum[:]) != file.SHA256 {
			return nil, errors.New("ARCHIVE_PACKAGE_ORIGINAL_INVALID")
		}
		if err = writeArchiveMember(writer, fmt.Sprintf("originals/%03d.bin", i+1), content); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// writeArchiveMember 写入服务端固定名称的ZIP条目，不使用外部路径。
// 参数：writer 为归档编码器，name 为固定路径，content 为已经校验的证据。
// 返回值：ZIP创建或写入错误。
func writeArchiveMember(writer *zip.Writer, name string, content []byte) error {
	entry, err := writer.Create(name)
	if err != nil {
		return err
	}
	_, err = entry.Write(content)
	return err
}

// archivePackage 消费按需建包任务，用封存清单通过文件网关逐份校验原件。
// 参数：ctx 为任务超时；row 的 ResourceID 是归属单据，载荷携带档案版本与请求人。
// 返回值：稳定诊断码；失败不写入半成品下载行。
func (p *Processor) archivePackage(ctx context.Context, row Record) error {
	var input struct {
		Actor        domain.Actor `json:"actor"`
		ArchiveID    string       `json:"archive_id"`
		ManifestHash string       `json:"manifest_hash"`
	}
	if json.Unmarshal([]byte(row.PayloadJSON), &input) != nil || input.ArchiveID == "" || len(input.ManifestHash) != 64 || input.Actor.TenantID != row.TenantID || !input.Actor.Has("expense.archive.export") {
		return errors.New("ARCHIVE_PACKAGE_REQUEST_INVALID")
	}
	if p.Files == nil {
		return errors.New("ARCHIVE_FILE_GATEWAY_UNAVAILABLE")
	}
	if p.Personnel == nil || p.Service == nil {
		return errors.New("ARCHIVE_AUTHORIZATION_UNAVAILABLE")
	}
	current, err := p.Personnel.CurrentExportActor(ctx, input.Actor)
	if err != nil {
		return err
	}
	if _, err = p.Service.ArchivePackageStatus(ctx, current, row.ID); err != nil {
		return err
	}
	tenant := row.TenantID
	return p.generateArchivePackage(ctx, row, input.ArchiveID, input.ManifestHash, func(ctx context.Context, documentID string) ([]byte, error) {
		return p.Files.ReadDocument(ctx, tenant, documentID)
	})
}

// generateArchivePackage 校验封存版本与请求摘要后构造逐项校验的包并原子提交。
// 参数：ctx 为取消边界；row 为租约任务；archiveID/manifestHash 为请求冻结值；read 为原件读取边界。
// 返回值：清单、原件或落库错误；不支持的差异不以空包假成功。
func (p *Processor) generateArchivePackage(ctx context.Context, row Record, archiveID, manifestHash string, read func(context.Context, string) ([]byte, error)) error {
	var archive store.Archive
	if err := p.DB.WithContext(ctx).Where("tenant_id = ? AND id = ? AND claim_id = ?", row.TenantID, archiveID, row.ResourceID).First(&archive).Error; err != nil {
		return errors.New("ARCHIVE_PACKAGE_ARCHIVE_MISSING")
	}
	if archive.Status != "SEALED" || archive.ManifestJSON == "" || archive.ManifestHash == "" {
		return errors.New("ARCHIVE_NOT_SEALED")
	}
	// 建包请求绑定请求时的封存摘要，版本被替换或摘要漂移时拒绝而不是生成错包。
	if archive.ManifestHash != manifestHash {
		return errors.New("ARCHIVE_PACKAGE_MANIFEST_CHANGED")
	}
	// 文件网关把缺件、归属撤销和远端故障统一收敛为不可用，这里只落稳定可重试诊断码。
	content, err := buildArchiveBundle(ctx, []byte(archive.ManifestJSON), func(ctx context.Context, documentID string) ([]byte, error) {
		original, err := read(ctx, documentID)
		if err != nil {
			return nil, errors.New("ARCHIVE_PACKAGE_ORIGINAL_UNAVAILABLE")
		}
		return original, nil
	})
	if err != nil {
		return err
	}
	return p.commitArchivePackage(ctx, row, archive, content)
}

// commitArchivePackage 在任务租约内原子写入下载包与系统审计，重投只补结束状态。
// 参数：ctx 为持久化边界；row 为租约凭证；archive 为封存版本；content 为已逐项校验的ZIP。
// 返回值：租约失效或落库错误；已有同任务包时不重复生成、不重复审计。
func (p *Processor) commitArchivePackage(ctx context.Context, row Record, archive store.Archive, content []byte) error {
	sum := sha256.Sum256(content)
	return p.withLease(ctx, row, func(tx *gorm.DB) error {
		var committed int64
		if err := tx.Model(&store.ArchiveDownload{}).Where("tenant_id = ? AND job_id = ?", row.TenantID, row.ID).Count(&committed).Error; err != nil {
			return err
		}
		if committed > 0 {
			return nil
		}
		id, err := archivePackageID()
		if err != nil {
			return err
		}
		download := store.ArchiveDownload{ID: id, TenantID: row.TenantID, JobID: row.ID, ClaimID: archive.ClaimID, ArchiveID: archive.ID, ManifestHash: archive.ManifestHash, SHA256: hex.EncodeToString(sum[:]), Content: content, CreatedAt: time.Now().UTC()}
		if err = tx.Create(&download).Error; err != nil {
			return err
		}
		return application.AppendWorkerAudit(tx, row.TenantID, archive.ClaimID, "archive.package.completed", row.ID, "SUCCESS", "")
	})
}

// recordArchivePackageFailure 在任务终结的同一事务写入一条系统失败审计。
// 参数：tx为任务终结事务；row为已领取任务；reason为稳定诊断码。
// 返回值：审计落库错误；非建包任务或无法归属的损坏任务不写入。
func recordArchivePackageFailure(tx *gorm.DB, row Record, reason string) error {
	if row.Kind != "ARCHIVE_PACKAGE" || row.TenantID == "" || row.ResourceID == "" {
		return nil
	}
	// 与 archive.package.completed 同族：对外交付物失败只留终态一条记录。
	return application.AppendWorkerAudit(tx, row.TenantID, row.ResourceID, "archive.package.failed", row.ID, "FAILURE", reason)
}

// archivePackageID 生成不暴露业务序号的下载标识，文件名与审计只引用该随机值。
// 参数：无。
// 返回值：随机十六进制标识或熵源错误。
func archivePackageID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
