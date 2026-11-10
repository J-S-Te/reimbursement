package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// parseRunID 稳定标识每次已领取解析尝试，不覆盖旧运行历史。
// 参数：job为持久化任务及领取次数。
// 返回值：本次运行ID。
func parseRunID(job store.Job) string { return job.ID + "-" + strconv.Itoa(job.Attempt) }

// lockParseJob 检查当前解析任务租约并锁住提交边界。
// 参数：tx为当前事务；tenant/documentID为绑定资源；lease为领取凭证。
// 返回值：有效任务；旧worker或过期任务返回冲突。
func lockParseJob(tx *gorm.DB, tenant, documentID string, lease ParseLease) (store.Job, error) {
	var job store.Job
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND tenant_id = ? AND resource_id = ? AND version = ? AND lease_owner = ? AND status = ? AND lease_until > ?", lease.JobID, tenant, documentID, lease.Version, lease.Owner, "PROCESSING", time.Now().UTC()).Take(&job).Error
	if err != nil {
		return job, domain.Fail("PARSE_LEASE_LOST", "解析任务已被接管或租约失效", 409)
	}
	return job, nil
}

// ensureParseRun 在本次实际尝试开始时建立独立运行记录，已终结记录保持不可变。
// 参数：tx为有效租约事务；job为持久任务；doc为固定原件。
// 返回值：运行记录及存储错误。
func ensureParseRun(tx *gorm.DB, job store.Job, doc store.Document) (store.InvoiceParseRun, error) {
	var run store.InvoiceParseRun
	err := tx.Where("tenant_id = ? AND id = ?", job.TenantID, parseRunID(job)).Take(&run).Error
	if err == nil {
		return run, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return run, err
	}
	var metadata struct {
		SHA256 string `json:"sha256"`
	}
	if err = json.Unmarshal([]byte(doc.ContentJSON), &metadata); err != nil {
		return run, err
	}
	if err = tx.Model(&store.InvoiceParseRun{}).Where("tenant_id = ? AND job_id = ? AND attempt < ? AND status = ?", job.TenantID, job.ID, job.Attempt, "RUNNING").Updates(map[string]any{"status": "ABANDONED", "failure_code": "ATTEMPT_SUPERSEDED", "completed_at": time.Now().UTC()}).Error; err != nil {
		return run, err
	}
	run = store.InvoiceParseRun{ID: parseRunID(job), TenantID: job.TenantID, DocumentID: doc.ID, JobID: job.ID, Attempt: job.Attempt, ParserVersion: "pending", RulesVersion: "expense-v1", InputSHA256: metadata.SHA256, Status: "RUNNING", ResultJSON: "{}", CreatedAt: time.Now().UTC()}
	return run, tx.Create(&run).Error
}

// StartParseAttempt 将本次任务尝试与原件摘要关联，故障也留下独立可追溯运行。
// 参数：ctx为任务边界；tenant/documentID为原件；lease为当前领取凭证。
// 返回值：租约或持久化错误。
func (s *Service) StartParseAttempt(ctx context.Context, tenant, documentID string, lease ParseLease) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := lockParseJob(tx, tenant, documentID, lease)
		if err != nil {
			return err
		}
		var doc store.Document
		if err = tx.Where("tenant_id = ? AND id = ?", tenant, documentID).Take(&doc).Error; err != nil {
			return err
		}
		_, err = ensureParseRun(tx, job, doc)
		return err
	})
}

// FailParseAttempt 仅由仍持有效租约的worker终结本次失败运行。
// 参数：ctx为持久化超时；tenant/documentID为资源；lease为凭证；code为脱敏稳定原因码。
// 返回值：租约失效或存储错误，不覆盖旧成功运行。
func (s *Service) FailParseAttempt(ctx context.Context, tenant, documentID string, lease ParseLease, code string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := lockParseJob(tx, tenant, documentID, lease)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if !workerReasonCode.MatchString(code) {
			code = "PARSER_FAILED"
		}
		result := tx.Model(&store.InvoiceParseRun{}).Where("tenant_id = ? AND id = ? AND status = ?", tenant, parseRunID(job), "RUNNING").Updates(map[string]any{"status": "FAILED", "failure_code": code, "completed_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		return AppendWorkerAudit(tx, tenant, documentID, "invoice.parse.failed", job.ID, "FAILURE", code)
	})
}

// persistParseResult 将本次运行结果与新建候选来源写入同一解析事务。
// 参数：tx为有效租约事务；job/doc为任务及原件；raw为完整结果；indexes绑定过滤后候选与原解析文档索引。
// 返回值：运行或来源持久化失败错误。
func persistParseResult(tx *gorm.DB, job store.Job, doc store.Document, raw json.RawMessage, indexes []int) error {
	run, err := ensureParseRun(tx, job, doc)
	if err != nil {
		return err
	}
	if run.Status != "RUNNING" {
		return nil
	}
	var result struct {
		Status        string `json:"status"`
		ParserVersion string `json:"parser_version"`
		RulesVersion  string `json:"rules_version"`
		Documents     []struct {
			Sources json.RawMessage `json:"sources"`
		} `json:"documents"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.Status == "" {
		result.Status = "PARTIAL"
	}
	if result.ParserVersion == "" {
		result.ParserVersion = "unknown"
	}
	if result.RulesVersion == "" {
		result.RulesVersion = "expense-v1"
	}
	hash := sha256.Sum256(raw)
	now := time.Now().UTC()
	if err = tx.Model(&store.InvoiceParseRun{}).Where("tenant_id = ? AND id = ? AND status = ?", job.TenantID, run.ID, "RUNNING").Updates(map[string]any{"status": result.Status, "parser_version": result.ParserVersion, "rules_version": result.RulesVersion, "result_hash": hex.EncodeToString(hash[:]), "result_json": string(raw), "completed_at": now}).Error; err != nil {
		return err
	}
	var metadata struct {
		InvoiceIDs []string `json:"invoice_ids"`
	}
	if err = json.Unmarshal([]byte(doc.ContentJSON), &metadata); err != nil {
		return err
	}
	for position, invoiceID := range metadata.InvoiceIDs {
		candidateIndex := position
		if position < len(indexes) {
			candidateIndex = indexes[position]
		}
		sources := "[]"
		if candidateIndex < len(result.Documents) && len(result.Documents[candidateIndex].Sources) > 0 {
			sources = string(result.Documents[candidateIndex].Sources)
		}
		id, err := newID()
		if err != nil {
			return err
		}
		source := store.InvoiceSource{ID: id, TenantID: job.TenantID, InvoiceID: invoiceID, DocumentID: doc.ID, ParseRunID: run.ID, CandidateIndex: candidateIndex, SourcesHash: parseSourceHash(sources), SourcesJSON: sources, CreatedAt: now}
		if err = tx.Create(&source).Error; err != nil {
			return err
		}
	}
	return nil
}

// ParseAlreadyCommitted 区分响应丢失的原任务重投与用户主动创建的新解析运行。
// 参数：ctx为查询边界；tenant/documentID/jobID为持久任务固定归属。
// 返回值：原任务结果已经原子提交时为真，不再次产生候选。
func (s *Service) ParseAlreadyCommitted(ctx context.Context, tenant, documentID, jobID string) (bool, error) {
	var doc store.Document
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenant, documentID).Take(&doc).Error; err != nil {
		return false, err
	}
	var metadata struct {
		Complete bool   `json:"parse_complete"`
		JobID    string `json:"parse_job_id"`
	}
	if err := json.Unmarshal([]byte(doc.ContentJSON), &metadata); err != nil {
		return false, err
	}
	return metadata.Complete && metadata.JobID == jobID, nil
}

// LinkManualInvoiceSources 只为已验证归属的原件补充显式人工来源，不伪造解析运行。
// 参数：tx为票据命令事务；invoice为当前已确认业务发票；reason为人工关联理由。
// 返回值：来源持久化错误；已存在的来源关系保持不可变。
func LinkManualInvoiceSources(tx *gorm.DB, invoice domain.Invoice, reason string) error {
	for _, fileID := range invoice.FileIDs {
		var docs []store.Document
		if err := tx.Where("tenant_id = ? AND file_id = ? AND owner = ?", invoice.TenantID, fileID, invoice.Owner).Limit(101).Find(&docs).Error; err != nil {
			return err
		}
		if len(docs) == 0 || len(docs) > 100 {
			return domain.Fail("INVOICE_SOURCE_INVALID", "票据原件来源缺失或超过受控上限", 422)
		}
		for _, doc := range docs {
			var count int64
			if err := tx.Model(&store.InvoiceSource{}).Where("tenant_id = ? AND invoice_id = ? AND document_id = ?", invoice.TenantID, invoice.ID, doc.ID).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				continue
			}
			raw, err := json.Marshal(map[string]any{"method": "MANUAL_LINK", "reason": reason, "invoice_version": invoice.Version})
			if err != nil {
				return err
			}
			id, err := newID()
			if err != nil {
				return err
			}
			if err = tx.Create(&store.InvoiceSource{ID: id, TenantID: invoice.TenantID, InvoiceID: invoice.ID, DocumentID: doc.ID, ParseRunID: "", CandidateIndex: -1, SourcesHash: parseSourceHash(string(raw)), SourcesJSON: string(raw), CreatedAt: time.Now().UTC()}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// CopyInvoiceSources 为同一业务发票归并追加来源关系，保留原候选的历史关系。
// 参数：tx为归并事务；tenant为可信租户；from/to为同归属同票面候选及权威发票ID。
// 返回值：追加失败错误；相同目标关系幂等保留。
func CopyInvoiceSources(tx *gorm.DB, tenant, from, to string) error {
	var sources []store.InvoiceSource
	if err := tx.Where("tenant_id = ? AND invoice_id = ?", tenant, from).Limit(1001).Find(&sources).Error; err != nil {
		return err
	}
	if len(sources) > 1000 {
		return domain.Fail("INVOICE_SOURCE_LIMIT", "票据来源超过受控上限", 422)
	}
	for _, source := range sources {
		var count int64
		if err := tx.Model(&store.InvoiceSource{}).Where("tenant_id = ? AND invoice_id = ? AND document_id = ? AND parse_run_id = ? AND candidate_index = ?", tenant, to, source.DocumentID, source.ParseRunID, source.CandidateIndex).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		id, err := newID()
		if err != nil {
			return err
		}
		if source.SourcesHash == "" {
			source.SourcesHash = parseSourceHash(source.SourcesJSON)
		}
		source.ID = id
		source.InvoiceID = to
		source.CreatedAt = time.Now().UTC()
		if err = tx.Create(&source).Error; err != nil {
			return err
		}
	}
	return nil
}

// parseSourceHash 固定来源区域内容摘要，档案可引用而无需复制大体积原文。
// 参数：raw为已保存的来源JSON。
// 返回值：小写SHA256摘要。
func parseSourceHash(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}
