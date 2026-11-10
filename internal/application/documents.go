package application

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// CreateUpload 在调用文件网关前持久化本人的上传资源，失败仍可追踪。
// 参数：ctx 为上下文，a 为可信上传人，payload 为受控用途及文件元数据。
// 返回值：真实上传会话或依赖错误。
func (s *Service) CreateUpload(ctx context.Context, a domain.Actor, payload json.RawMessage) (json.RawMessage, error) {

	gateway, ok := s.deps.Files.(UploadGateway)
	if !ok {
		return nil, domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件上传尚未配置", 503)
	}
	var input struct {
		CommandInput
		Purpose        string `json:"purpose"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, e
	}
	if input.Purpose != "reimbursement.invoice.original" && input.Purpose != "reimbursement.payment.voucher" && input.Purpose != "reimbursement.claim.evidence" {
		return nil, domain.Fail("FILE_PURPOSE_INVALID", "此入口仅接收票据原件、付款凭证或费用证据", 422)
	}
	if input.Purpose == "reimbursement.claim.evidence" {
		if e := authorizeClaimEvidenceUpload(a); e != nil {
			return nil, e
		}
	}
	if input.Purpose == "reimbursement.invoice.original" {
		if e := require(a, "expense.invoice.upload"); e != nil {
			return nil, e
		}
	}
	if input.Purpose == "reimbursement.payment.voucher" {
		if e := authorizeVoucherUpload(a); e != nil {
			return nil, e
		}
	}
	if input.IdempotencyKey == "" {
		return nil, domain.Fail("COMMAND_ID_REQUIRED", "上传需要幂等标识", 422)
	}
	requestHash, e := digest(payload)
	if e != nil {
		return nil, e
	}
	idHash, e := digest([]string{a.TenantID, a.SubjectID, input.IdempotencyKey})
	if e != nil {
		return nil, e
	}
	id := idHash[:32]
	db := s.db.WithContext(ctx)
	raw, e := encode(map[string]any{"request": json.RawMessage(payload), "request_hash": requestHash, "purpose": input.Purpose})
	if e != nil {
		return nil, e
	}
	var row store.Document
	e = db.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("tenant_id = ? AND id = ? AND owner = ?", a.TenantID, id, a.SubjectID).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			row = store.Document{ID: id, TenantID: a.TenantID, Owner: a.SubjectID, Status: "UPLOAD_PENDING", Version: 1, ContentJSON: raw}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			return appendOutbox(tx, a.TenantID, "audit", row.ID, map[string]any{"actor_subject_id": a.SubjectID, "request_id": a.RequestID, "action": "invoice_document.create", "resource_id": row.ID, "command_id": input.CommandID, "purpose": input.Purpose, "version": row.Version})
		}
		if err != nil {
			return err
		}
		var original map[string]json.RawMessage
		if err = json.Unmarshal([]byte(row.ContentJSON), &original); err != nil {
			return err
		}
		var previous string
		if err = json.Unmarshal(original["request_hash"], &previous); err != nil {
			return err
		}
		if previous != requestHash {
			return domain.Fail("COMMAND_PAYLOAD_CONFLICT", "上传幂等标识对应的文件元数据已改变", 409)
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	if row.Status != "UPLOAD_PENDING" {
		var metadata map[string]json.RawMessage
		if e = json.Unmarshal([]byte(row.ContentJSON), &metadata); e != nil {
			return nil, e
		}
		var jobID string
		_ = json.Unmarshal(metadata["parse_job_id"], &jobID)
		return json.Marshal(map[string]any{"document_id": row.ID, "file_id": row.FileID, "status": row.Status, "version": row.Version, "parse_job_id": jobID, "parse_run_id": jobID})
	}
	response, e := gateway.CreateUpload(ctx, a, id, payload)
	if e != nil {
		return nil, e
	}
	var data map[string]any
	if e = json.Unmarshal(response, &data); e != nil {
		return nil, e
	}
	if inner, exists := data["data"].(map[string]any); exists {
		data = inner
	}
	data["request_hash"] = requestHash
	data["request"] = json.RawMessage(payload)
	data["purpose"] = input.Purpose
	data["document_id"] = id
	stored := map[string]any{}
	for key, value := range data {
		if key != "ticket" && key != "upload_url" && key != "expires_at" {
			stored[key] = value
		}
	}
	data["version"] = row.Version
	e = db.Transaction(func(tx *gorm.DB) error {
		var current store.Document
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", a.TenantID, id).First(&current).Error; err != nil {
			return err
		}
		var merged map[string]any
		if err := json.Unmarshal([]byte(current.ContentJSON), &merged); err != nil {
			return err
		}
		for key, value := range stored {
			if key != "parse_job_id" && key != "parse_complete" && key != "parse_result" && key != "invoice_ids" && key != "parse_issues" {
				merged[key] = value
			}
		}
		content, err := encode(merged)
		if err != nil {
			return err
		}
		fileID, _ := merged["file_id"].(string)
		return tx.Model(&store.Document{}).Where("tenant_id = ? AND id = ? AND version = ?", a.TenantID, id, current.Version).Updates(map[string]any{"content_json": content, "file_id": fileID}).Error
	})
	if e != nil {
		return nil, e
	}
	return json.Marshal(data)
}

// CompleteUpload 从网关读取完成事实并原子创建一次解析任务。
// 参数：ctx 为上下文，a 为当前上传人，documentID 为预分配文档标识。
// 返回值：文档状态和真实解析任务标识。
func (s *Service) CompleteUpload(ctx context.Context, a domain.Actor, documentID string) (json.RawMessage, error) {

	var existing store.Document
	if e := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND owner = ?", a.TenantID, documentID, a.SubjectID).First(&existing).Error; e != nil {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "上传资源不存在", 404)
	}
	var authorization struct {
		Purpose string `json:"purpose"`
	}
	if e := json.Unmarshal([]byte(existing.ContentJSON), &authorization); e != nil {
		return nil, e
	}
	if authorization.Purpose == "reimbursement.claim.evidence" {
		if e := authorizeClaimEvidenceUpload(a); e != nil {
			return nil, e
		}
	} else if authorization.Purpose == "reimbursement.payment.voucher" {
		if e := authorizeVoucherUpload(a); e != nil {
			return nil, e
		}
	} else if e := require(a, "expense.invoice.upload"); e != nil {
		return nil, e
	}
	if receipt, done, err := completedDocumentReceipt(existing); done || err != nil {
		return receipt, err
	}
	gateway, ok := s.deps.Files.(UploadGateway)
	if !ok {
		return nil, domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件服务尚未配置", 503)
	}
	response, e := gateway.CompleteUpload(ctx, a, documentID)
	if e != nil {
		return nil, e
	}
	var result any
	e = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row store.Document
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ? AND owner = ?", a.TenantID, documentID, a.SubjectID).First(&row).Error; err != nil {
			return err
		}
		if receipt, done, err := completedDocumentReceipt(row); done || err != nil {
			result = receipt
			return err
		}
		var metadata map[string]any
		if err := json.Unmarshal([]byte(row.ContentJSON), &metadata); err != nil {
			return err
		}
		var completion map[string]any
		if err := json.Unmarshal(response, &completion); err != nil {
			return err
		}
		if nested, ok := completion["data"].(map[string]any); ok {
			completion = nested
		}
		metadata["completion"] = completion
		for _, key := range []string{"file_id", "upload_id", "media_type", "sha256"} {
			if value, ok := completion[key]; ok {
				metadata[key] = value
			}
		}
		fileID, _ := metadata["file_id"].(string)
		if fileID == "" {
			return domain.Fail("FILE_NOT_READY", "文件网关未返回完成后的文件标识", 503)
		}
		jobID := ""
		if old, ok := metadata["parse_job_id"].(string); ok {
			jobID = old
		}
		purpose, _ := metadata["purpose"].(string)
		if purpose == "reimbursement.invoice.original" && jobID == "" {
			var err error
			jobID, err = newID()
			if err != nil {
				return err
			}
			payload, err := encode(map[string]any{"document_id": row.ID, "file_id": fileID, "owner_subject_id": a.SubjectID})
			if err != nil {
				return err
			}
			if err = tx.Create(&store.Job{ID: jobID, TenantID: a.TenantID, Kind: "INVOICE_PARSE", ResourceID: row.ID, PayloadJSON: payload, Status: "PENDING", Version: 1, NextAttemptAt: time.Now().UTC()}).Error; err != nil {
				return err
			}
			metadata["parse_job_id"] = jobID
		}
		result = map[string]any{"document_id": row.ID, "file_id": fileID, "status": "READY", "parse_job_id": jobID, "parse_run_id": jobID, "version": row.Version + 1}
		metadata["complete_receipt"] = result
		raw, err := encode(metadata)
		if err != nil {
			return err
		}
		if err = tx.Model(&store.Document{}).Where("id = ? AND version = ?", row.ID, row.Version).Updates(map[string]any{"status": "READY", "file_id": fileID, "version": row.Version + 1, "content_json": raw}).Error; err != nil {
			return err
		}
		return appendOutbox(tx, a.TenantID, "audit", row.ID, map[string]any{"actor_subject_id": a.SubjectID, "request_id": a.RequestID, "action": "invoice_document.complete", "resource_id": row.ID, "parse_job_id": jobID, "version": row.Version + 1})
	})
	if e != nil {
		return nil, e
	}
	return json.Marshal(result)
}

// CompleteParse 将解析候选保存为待人工复核，不升级为税务验真。
// 参数：ctx 为任务上下文，tenant/documentID 为受控资源，candidates 为解析候选，issues 为诊断。
// 返回值：落库或文档状态错误。
func (s *Service) CompleteParse(ctx context.Context, tenant, documentID string, candidates []domain.Invoice, issues []string) error {
	return s.CompleteParseResult(ctx, tenant, documentID, candidates, issues, nil)
}

// CompleteParseResult 原子保存完整解析来源及候选，重复任务不产生重复票据。
// 参数：ctx/tenant/documentID 为任务边界，candidates/issues 为结果，rawResult 为原始受控解析信息。
// 返回值：持久化错误。
func (s *Service) CompleteParseResult(ctx context.Context, tenant, documentID string, candidates []domain.Invoice, issues []string, rawResult json.RawMessage) error {
	if len(candidates) > 100 {
		return domain.Fail("PARSE_LIMIT", "解析候选超过受控上限", 422)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row store.Document
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", tenant, documentID).First(&row).Error; e != nil {
			return e
		}
		var metadata map[string]any
		if e := json.Unmarshal([]byte(row.ContentJSON), &metadata); e != nil {
			return e
		}
		if metadata["parse_complete"] == true {
			return nil
		}
		ids := []string{}
		for _, candidate := range candidates {
			id, e := newID()
			if e != nil {
				return e
			}
			candidate.ID = id
			candidate.TenantID = tenant
			candidate.Owner = row.Owner
			candidate.Version = 1
			candidate.ReviewStatus = "PENDING"
			candidate.VerificationStatus = "NOT_CHECKED"
			candidate.IdentityKey = ""
			candidate.ReservationState = "AVAILABLE"
			candidate.ReservationClaimID = ""
			candidate.FileIDs = []string{row.FileID}
			raw, e := encode(candidate)
			if e != nil {
				return e
			}
			if e = tx.Create(&store.Invoice{ID: id, TenantID: tenant, Owner: row.Owner, Version: 1, ReservationState: "AVAILABLE", ContentJSON: raw}).Error; e != nil {
				return e
			}
			ids = append(ids, id)
		}
		metadata["parse_complete"] = true
		metadata["invoice_ids"] = ids
		metadata["parse_issues"] = issues
		if len(rawResult) > 0 {
			metadata["parse_result"] = rawResult
		}
		raw, e := encode(metadata)
		if e != nil {
			return e
		}
		status := "PARSED"
		if len(candidates) == 0 {
			status = "REVIEW_REQUIRED"
		}
		return tx.Model(&store.Document{}).Where("id = ? AND version = ?", row.ID, row.Version).Updates(map[string]any{"status": status, "version": row.Version + 1, "content_json": raw}).Error
	})
}

// StartExport 保存导出请求的权限快照与选择范围，返回真实排队任务。
// 参数：ctx 为请求上下文，a 为当前主体，payload 为幂等键与受控过滤条件。
// 返回值：异步任务回执或权限错误。
func (s *Service) StartExport(ctx context.Context, a domain.Actor, payload json.RawMessage) (json.RawMessage, error) {
	if e := require(a, "expense.archive.export"); e != nil {
		return nil, e
	}
	var input struct {
		CommandInput
		ClaimIDs  []string `json:"claim_ids"`
		ProjectID string   `json:"project_id"`
	}
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, e
	}
	if input.CommandID == "" || len(input.ClaimIDs) > 100 {
		return nil, domain.Fail("INVALID_EXPORT", "导出需要命令标识且最多指定100张单据", 422)
	}
	for _, id := range input.ClaimIDs {
		if _, e := s.Query(ctx, a, "claim", id, nil); e != nil {
			return nil, e
		}
	}
	hash, e := digest(payload)
	if e != nil {
		return nil, e
	}
	cc := commandContext{actor: a, operation: "export.create", input: input.CommandInput, hash: hash}
	db := s.db.WithContext(ctx)
	if r, found, e := s.replay(db, cc); found || e != nil {
		return r, e
	}
	var result json.RawMessage
	e = db.Transaction(func(tx *gorm.DB) error {
		id, e := newID()
		if e != nil {
			return e
		}
		raw, e := encode(map[string]any{"actor": a, "filters": map[string]string{"page": "1", "page_size": "100", "project_id": input.ProjectID}, "claim_ids": input.ClaimIDs})
		if e != nil {
			return e
		}
		if e = tx.Create(&store.Job{ID: id, TenantID: a.TenantID, Kind: "EXPORT", ResourceID: id, PayloadJSON: raw, Status: "PENDING", Version: 1, NextAttemptAt: time.Now().UTC()}).Error; e != nil {
			return e
		}
		body, e := encode(map[string]any{"id": id, "job_id": id, "status": "PENDING"})
		if e != nil {
			return e
		}
		commandID, e := newID()
		if e != nil {
			return e
		}
		if e = tx.Create(&store.Command{ID: commandID, TenantID: a.TenantID, Actor: a.SubjectID, Operation: "export.create", CommandID: input.CommandID, RequestHash: hash, ResourceID: id, ResultJSON: body}).Error; e != nil {
			return e
		}
		result = json.RawMessage(body)
		return appendOutbox(tx, a.TenantID, "audit", id, map[string]any{"actor_subject_id": a.SubjectID, "request_id": a.RequestID, "action": "export.create", "resource_id": id})
	})
	if e != nil {
		if r, found, err := s.replay(db, cc); found {
			return r, err
		}
	}
	return result, e
}

// ParseDocument 使用固定命令和文档版本请求独立重解析，旧运行始终保留。
// 参数：ctx 为上下文，a 为当前上传人，documentID 为本人文档，payload 为命令请求。
// 返回值：新任务或既有活跃任务的冻结回执。
func (s *Service) ParseDocument(ctx context.Context, a domain.Actor, documentID string, payload ...json.RawMessage) (json.RawMessage, error) {
	if len(payload) != 1 {
		return nil, domain.Fail("COMMAND_ID_REQUIRED", "重新解析必须携带命令标识及当前文档版本", 422)
	}
	return s.Execute(ctx, a, "invoice.reparse", documentID, payload[0])
}

// queueReparse 原子关联新解析任务，仅修改文档最新投影，不覆盖旧job或run。
// 参数：tx 为本地命令事务，cc 为当前本人文档与版本。
// 返回值：真实排队任务回执、文档标识及错误。
func (s *Service) queueReparse(tx *gorm.DB, cc *commandContext) (any, string, error) {
	var doc store.Document
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ? AND owner = ?", cc.actor.TenantID, cc.resourceID, cc.actor.SubjectID).First(&doc).Error; e != nil {
		return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "文档不存在或无权重解析", 404)
	}
	if e := verifyVersion(doc.Version, cc.input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	var metadata map[string]any
	if e := json.Unmarshal([]byte(doc.ContentJSON), &metadata); e != nil {
		return nil, "", e
	}
	if metadata["purpose"] != "reimbursement.invoice.original" || doc.Status == "UPLOAD_PENDING" || doc.FileID == "" {
		return nil, "", domain.Fail("FILE_NOT_READY", "请先完成票据原件上传", 409)
	}
	oldJobID, _ := metadata["parse_job_id"].(string)
	if oldJobID != "" {
		var active store.Job
		err := tx.Where("tenant_id = ? AND id = ? AND resource_id = ?", cc.actor.TenantID, oldJobID, doc.ID).First(&active).Error
		if err != nil {
			return nil, "", err
		}
		if active.Status == "PENDING" || active.Status == "PROCESSING" {
			return map[string]any{"id": active.ID, "parse_job_id": active.ID, "parse_run_id": active.ID, "document_id": doc.ID, "document_version": doc.Version, "status": active.Status, "version": active.Version}, doc.ID, nil
		}
	}
	id, e := newID()
	if e != nil {
		return nil, "", e
	}
	payload, e := encode(map[string]any{"document_id": doc.ID, "file_id": doc.FileID, "owner_subject_id": doc.Owner})
	if e != nil {
		return nil, "", e
	}
	job := store.Job{ID: id, TenantID: doc.TenantID, Kind: "INVOICE_PARSE", ResourceID: doc.ID, PayloadJSON: payload, Status: "PENDING", Version: 1, NextAttemptAt: time.Now().UTC()}
	if e = tx.Create(&job).Error; e != nil {
		return nil, "", e
	}
	metadata["previous_parse_job_id"] = oldJobID
	metadata["parse_job_id"] = id
	metadata["parse_complete"] = false
	raw, e := encode(metadata)
	if e != nil {
		return nil, "", e
	}
	r := tx.Model(&store.Document{}).Where("tenant_id = ? AND id = ? AND version = ?", doc.TenantID, doc.ID, doc.Version).Updates(map[string]any{"version": doc.Version + 1, "status": "READY", "content_json": raw})
	if r.Error != nil {
		return nil, "", r.Error
	}
	if r.RowsAffected != 1 {
		return nil, "", domain.Fail("VERSION_CONFLICT", "文档状态已改变，请刷新", 409)
	}
	return map[string]any{"id": id, "parse_job_id": id, "parse_run_id": id, "document_id": doc.ID, "document_version": doc.Version + 1, "status": "PENDING", "version": job.Version}, doc.ID, nil
}

// authorizeVoucherUpload 仅授予有明确财务动作的主体上传证据，不附带付款登记能力。
// 参数：a 为当前身份。
// 返回值：没有任何允许证据用途的能力时拒绝。
func authorizeVoucherUpload(a domain.Actor) error {
	for _, permission := range []string{"expense.payment.record", "expense.payment.reversal.request", "expense.payment.reversal.approve", "expense.payment.exception.record", "expense.payment.exception.review"} {
		if a.Has(permission) {
			return require(a, permission)
		}
	}
	return domain.Fail("FORBIDDEN", "没有财务证据上传权限", 403)
}
