package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"time"
)

// requireArchivePackage 校验包生成和每次下载的当前整单授权。
// 参数：db 为当前数据库，a 为可信主体，claimID 为归属单据。
// 返回值：读取或范围错误，不以任务发起时权限替代当前权限。
func (s *Service) requireArchivePackage(db *gorm.DB, a domain.Actor, claimID string) error {
	for _, permission := range []string{"expense.archive.read", "expense.archive.export", "expense.claim.read"} {
		if err := require(a, permission); err != nil {
			return err
		}
	}
	claim, _, err := loadClaim(db, a.TenantID, claimID, false)
	if err != nil {
		return err
	}
	full, err := s.fullRead(db, a, claim)
	if err != nil {
		return err
	}
	if !full {
		return domain.Fail("FORBIDDEN", "当前无完整档案授权", 403)
	}
	return nil
}

// queueArchivePackage 为不可变封存版本生成独立异步下载任务。
// 参数：tx 为命令事务，cc 为包含目标档案及版本的命令。
// 返回值：排队回执、归属单据及错误。
func (s *Service) queueArchivePackage(tx *gorm.DB, cc *commandContext) (any, string, error) {
	var archive store.Archive
	if err := tx.Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&archive).Error; err != nil {
		return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "归档版本不存在", 404)
	}
	if err := s.requireArchivePackage(tx, cc.actor, archive.ClaimID); err != nil {
		return nil, "", err
	}
	if archive.Status != "SEALED" || archive.ManifestHash == "" {
		return nil, "", domain.Fail("ARCHIVE_NOT_SEALED", "仅完整封存版本可生成包", 409)
	}
	if err := verifyVersion(archive.PackageVersion, cc.input.ExpectedVersion); err != nil {
		return nil, "", err
	}
	id, err := newID()
	if err != nil {
		return nil, "", err
	}
	raw, err := encode(map[string]any{"actor": cc.actor, "archive_id": archive.ID, "manifest_hash": archive.ManifestHash, "claim_ids": []string{archive.ClaimID}})
	if err != nil {
		return nil, "", err
	}
	job := store.Job{ID: id, TenantID: cc.actor.TenantID, Kind: "ARCHIVE_PACKAGE", ResourceID: archive.ClaimID, Status: "PENDING", Version: 1, PayloadJSON: raw, NextAttemptAt: time.Now().UTC()}
	if err = tx.Create(&job).Error; err != nil {
		return nil, "", err
	}
	return map[string]any{"id": id, "job_id": id, "status": "PENDING"}, archive.ClaimID, nil
}

// archivePackageJob 读取本人下载任务并重新校验当前整单授权。
// 参数：ctx 为请求边界，a 为主体，jobID 为任务标识。
// 返回值：已授权任务，越权或任务类型不匹配时隐藏对象。
func (s *Service) archivePackageJob(ctx context.Context, a domain.Actor, jobID string) (store.Job, error) {
	db := s.db.WithContext(ctx)
	var job store.Job
	if err := db.Where("tenant_id = ? AND id = ? AND kind = ?", a.TenantID, jobID, "ARCHIVE_PACKAGE").First(&job).Error; err != nil {
		return job, domain.Fail("RESOURCE_NOT_FOUND", "档案下载任务不存在", 404)
	}
	var input struct {
		Actor domain.Actor `json:"actor"`
	}
	if json.Unmarshal([]byte(job.PayloadJSON), &input) != nil {
		return job, errors.New("invalid archive package request")
	}
	if input.Actor.SubjectID != a.SubjectID {
		return job, domain.Fail("RESOURCE_NOT_FOUND", "档案下载任务不存在", 404)
	}
	return job, s.requireArchivePackage(db, a, job.ResourceID)
}

// ArchivePackages 按档案版本列出本人可恢复的下载包元数据，不读取也不返回文件内容。
// 参数：ctx 为请求边界，a 为当前主体，archiveID 为封存档案版本，params 为分页参数。
// 返回值：仅含当前租户、当前主体建包任务的包页；跨租户或越权统一隐藏为不存在。
func (s *Service) ArchivePackages(ctx context.Context, a domain.Actor, archiveID string, params map[string]string) (any, error) {
	db := s.db.WithContext(ctx)
	var archive store.Archive
	if err := db.Where("tenant_id = ? AND id = ?", a.TenantID, archiveID).First(&archive).Error; err != nil {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "归档版本不存在", 404)
	}
	// 与建包、状态、下载共用同一当前整单授权助手，不新增权限码。
	if err := s.requireArchivePackage(db, a, archive.ClaimID); err != nil {
		return nil, err
	}
	// 建包任务以受信任的租户、任务类型和归属单据收敛，再按冻结载荷确认档案版本和建包主体。
	var jobs []store.Job
	if err := db.Where("tenant_id = ? AND kind = ? AND resource_id = ?", a.TenantID, "ARCHIVE_PACKAGE", archive.ClaimID).Order("created_at DESC, id DESC").Find(&jobs).Error; err != nil {
		return nil, err
	}
	matched := make([]store.Job, 0, len(jobs))
	for _, job := range jobs {
		var input struct {
			Actor     domain.Actor `json:"actor"`
			ArchiveID string       `json:"archive_id"`
		}
		if err := json.Unmarshal([]byte(job.PayloadJSON), &input); err != nil {
			return nil, errors.New("invalid archive package request")
		}
		// 任务标识不是授权：他人建包回执不进列表，避免给出无法查询也无法下载的死链。
		if input.ArchiveID != archive.ID || input.Actor.SubjectID != a.SubjectID {
			continue
		}
		matched = append(matched, job)
	}
	page, size, offset := pageArgs(params)
	total := len(matched)
	if offset > total {
		offset = total
	}
	end := offset + size
	if end > total {
		end = total
	}
	// 只读取摘要列，显式避免把 longblob 包内容读入内存或带出接口。
	hashes := map[string]store.ArchiveDownload{}
	done := []string{}
	for _, job := range matched[offset:end] {
		if job.Status == "DONE" {
			done = append(done, job.ID)
		}
	}
	if len(done) > 0 {
		var rows []store.ArchiveDownload
		if err := db.Model(&store.ArchiveDownload{}).Select("job_id, sha256, manifest_hash").Where("tenant_id = ? AND job_id IN ?", a.TenantID, done).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			hashes[row.JobID] = row
		}
	}
	items := []any{}
	for _, job := range matched[offset:end] {
		item := map[string]any{"id": job.ID, "job_id": job.ID, "archive_id": archive.ID, "status": job.Status, "last_error": job.LastError, "created_at": job.CreatedAt, "sha256": "", "manifest_hash": ""}
		// 只有已完成封装的包才有摘要；其余状态返回空值而不是报错。
		if job.Status == "DONE" {
			if row, ok := hashes[job.ID]; ok {
				item["sha256"] = row.SHA256
				item["manifest_hash"] = row.ManifestHash
			}
		}
		items = append(items, item)
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}, nil
}

// ArchivePackageStatus 返回实际异步状态及失败原因，不暴露文件内容。
// 参数：ctx 为请求边界，a 为当前主体，jobID 为原生成任务。
// 返回值：当前任务状态，完成时包含包摘要；保留期清理后返回 EXPIRED 而不是错误。
func (s *Service) ArchivePackageStatus(ctx context.Context, a domain.Actor, jobID string) (any, error) {
	job, err := s.archivePackageJob(ctx, a, jobID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"id": job.ID, "status": job.Status, "last_error": job.LastError}
	if job.Status == "DONE" {
		var result store.ArchiveDownload
		if err = s.db.WithContext(ctx).Where("tenant_id = ? AND job_id = ?", a.TenantID, job.ID).Omit("content").First(&result).Error; err != nil {
			// 下载包被保留期清理删除后任务仍是 DONE：状态查询是只读轮询，
			// 以 200 + EXPIRED 收敛，缺失摘要为空，避免轮询端进入 404/500 错误分支。
			// 只有“行不存在”收敛为过期，其它数据库错误原样上抛。
			if errors.Is(err, gorm.ErrRecordNotFound) {
				out["status"] = "EXPIRED"
				out["sha256"] = ""
				out["manifest_hash"] = ""
				return out, nil
			}
			return nil, err
		}
		out["sha256"] = result.SHA256
		out["manifest_hash"] = result.ManifestHash
		out["archive_id"] = result.ArchiveID
	}
	return out, nil
}

// ReadArchivePackage 在下载时再次校验权限和生成物哈希并记录审计。
// 参数：ctx 为请求边界，a 为当前身份，jobID 为已完成生成任务。
// 返回值：安全固定文件名和ZIP字节，状态或授权失效时拒绝。
func (s *Service) ReadArchivePackage(ctx context.Context, a domain.Actor, jobID string) (string, []byte, error) {
	job, err := s.archivePackageJob(ctx, a, jobID)
	if err != nil {
		return "", nil, err
	}
	if job.Status != "DONE" {
		return "", nil, domain.Fail("ARCHIVE_PACKAGE_NOT_READY", "档案包尚未完成", 409)
	}
	var result store.ArchiveDownload
	if err = s.db.WithContext(ctx).Where("tenant_id = ? AND job_id = ?", a.TenantID, job.ID).First(&result).Error; err != nil {
		// 任务仍是 DONE 但下载行已被保留期清理：下载是实际取物动作，以受控 409 提示重新生成；
		// 只有“行不存在”收敛为过期，其它数据库错误原样上抛，避免把故障伪装成过期。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil, domain.Fail("ARCHIVE_PACKAGE_EXPIRED", "归档包已超过保留期，请重新生成", 409)
		}
		return "", nil, err
	}
	sum := sha256.Sum256(result.Content)
	if hex.EncodeToString(sum[:]) != result.SHA256 {
		return "", nil, domain.Fail("ARCHIVE_PACKAGE_HASH_MISMATCH", "档案包校验失败，请重新生成", 409)
	}
	err = appendOutbox(s.db.WithContext(ctx), a.TenantID, "audit", job.ResourceID, map[string]any{"actor_subject_id": a.SubjectID, "request_id": a.RequestID, "action": "archive.package.download", "manifest_hash": result.ManifestHash, "package_sha256": result.SHA256, "archive_id": result.ArchiveID})
	if err != nil {
		return "", nil, err
	}
	return "reimbursement-archive-" + result.ID + ".zip", result.Content, nil
}
