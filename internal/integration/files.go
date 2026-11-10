package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/oklog/ulid/v2"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Files struct {
	DB     *gorm.DB
	Client *Client
}
type Upload struct {
	Purpose        string `json:"purpose"`
	OriginalName   string `json:"original_name"`
	MediaType      string `json:"media_type"`
	SizeBytes      int64  `json:"size_bytes"`
	SHA256         string `json:"sha256"`
	IdempotencyKey string `json:"idempotency_key"`
}
type fileMetadata struct {
	UploadID  string `json:"upload_id"`
	FileID    string `json:"file_id"`
	Status    string `json:"status"`
	Purpose   string `json:"purpose"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
}

// NewFiles 组装文件网关v2与本地资源归属检查。
// 参数：db为报销库；client为报销独立文件用途机器凭据。
// 返回值：文件适配器。
func NewFiles(db *gorm.DB, client *Client) *Files { return &Files{DB: db, Client: client} }

// filePurpose 校验首发允许的文件格式与用途，防止任意绑定用途穿透。
// 参数：u为上传声明。
// 返回值：是否合法。
func filePurpose(u Upload) bool {
	switch u.Purpose {
	case "reimbursement.invoice.original":
		return u.MediaType == "application/pdf" || u.MediaType == "application/ofd" ||
			u.MediaType == "application/vnd.ofd" || u.MediaType == "image/png" || u.MediaType == "image/jpeg"
	case "reimbursement.payment.voucher", "reimbursement.claim.evidence":
		return u.MediaType == "application/pdf" || u.MediaType == "image/png" || u.MediaType == "image/jpeg"
	}
	return false
}

// CreateUpload 为已预分配的本地文档创建v2上传会话与一次性票据。
// 参数：ctx与actor限定调用；resourceID是本地文档ID；payload是上传元数据。
// 返回值：网关会话、票据及本地ID；失败保留UPLOAD_PENDING供诊断。
func (f *Files) CreateUpload(ctx context.Context, actor domain.Actor, resourceID string, payload json.RawMessage) (json.RawMessage, error) {
	return f.createUpload(ctx, actor, resourceID, payload, false)
}

// createUpload 对外上传和后台受控导出共享绑定协议，生成用途不会对浏览器开放。
// 参数：ctx和actor限定调用；resourceID为本地文档；payload为元数据；generated仅限内部生成路径。
// 返回值：会话票据及绑定错误。
func (f *Files) createUpload(ctx context.Context, actor domain.Actor, resourceID string, payload json.RawMessage, generated bool) (json.RawMessage, error) {
	var u Upload
	if json.Unmarshal(payload, &u) != nil || !(filePurpose(u) || generated && u.Purpose == "reimbursement.export.result" && u.MediaType == "text/csv") || u.SizeBytes < 1 || u.SizeBytes > 20<<20 || len(u.OriginalName) > 255 || strings.ContainsAny(u.OriginalName, "/\\\x00") {
		return nil, domain.Fail("UPLOAD_INVALID", "请选择20MiB以内允许格式的原始文件", 422)
	}
	h, e := hex.DecodeString(u.SHA256)
	if e != nil || len(h) != 32 || u.IdempotencyKey == "" {
		return nil, domain.Fail("UPLOAD_INVALID", "文件摘要和幂等标识不完整", 422)
	}
	var doc store.Document
	if e = f.DB.WithContext(ctx).Where("tenant_id = ? AND id = ? AND owner = ?", actor.TenantID, resourceID, actor.SubjectID).First(&doc).Error; e != nil {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "文件记录不存在", 404)
	}
	body := map[string]any{"purpose": u.Purpose, "original_name": u.OriginalName, "media_type": u.MediaType, "size_bytes": u.SizeBytes, "sha256": u.SHA256, "resource_type": "invoice_document", "resource_id": resourceID, "binding_type": "original", "classification": "CONFIDENTIAL", "actor_user_id": actor.SubjectID, "idempotency_key": resourceID + ":" + u.IdempotencyKey}
	data, e := f.Client.Call(ctx, "POST", "/api/v2/upload-sessions", body)
	if e != nil {
		return nil, domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件上传服务暂不可用", 503)
	}
	var metadata fileMetadata
	if json.Unmarshal(data, &metadata) != nil || metadata.UploadID == "" || metadata.FileID == "" {
		return nil, ErrUnavailable
	}
	var session map[string]any
	if json.Unmarshal(data, &session) != nil {
		return nil, ErrUnavailable
	}
	var persisted map[string]any
	finished := false
	e = f.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var latest store.Document
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ? AND owner = ?", actor.TenantID, resourceID, actor.SubjectID).Take(&latest).Error; err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(latest.ContentJSON), &persisted); err != nil {
			return err
		}
		if latest.Status != "UPLOAD_PENDING" {
			finished = true
			persisted["document_id"] = latest.ID
			persisted["file_id"] = latest.FileID
			persisted["status"] = latest.Status
			return nil
		}
		for _, key := range []string{"upload_id", "file_id", "mode", "purpose", "status", "original_name", "media_type", "size_bytes", "sha256", "failure_code", "created_at", "completed_at"} {
			if value, exists := session[key]; exists {
				persisted[key] = value
			}
		}
		stored, err := json.Marshal(persisted)
		if err != nil {
			return err
		}
		result := tx.Model(&store.Document{}).Where("tenant_id = ? AND id = ? AND version = ?", actor.TenantID, resourceID, latest.Version).Updates(map[string]any{"content_json": string(stored), "file_id": metadata.FileID, "version": latest.Version + 1})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return domain.Fail("UPLOAD_VERSION_CONFLICT", "上传状态已改变，请刷新后继续", 409)
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	if finished {
		return json.Marshal(persisted)
	}
	ticket, e := f.Client.Call(ctx, "POST", "/api/v2/upload-sessions/"+url.PathEscape(metadata.UploadID)+"/tickets", nil)
	if e != nil {
		return nil, domain.Fail("FILE_GATEWAY_UNAVAILABLE", "无法签发上传票据", 503)
	}
	var result map[string]any
	if json.Unmarshal(ticket, &result) != nil {
		return nil, ErrUnavailable
	}
	for key, value := range session {
		if _, exists := result[key]; !exists {
			result[key] = value
		}
	}
	result["document_id"] = resourceID
	result["purpose"] = u.Purpose
	return json.Marshal(result)
}

// CompleteUpload 查询v2完成状态，不再次提交原件内容。
// 参数：ctx与actor限定主体；documentID是本地预分配文档。
// 返回值：真实READY状态元数据；未完成或不可用不返回成功。
func (f *Files) CompleteUpload(ctx context.Context, actor domain.Actor, documentID string) (json.RawMessage, error) {
	var doc store.Document
	if f.DB.WithContext(ctx).Where("tenant_id = ? AND id = ? AND owner = ?", actor.TenantID, documentID, actor.SubjectID).First(&doc).Error != nil {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "文件记录不存在", 404)
	}
	var meta fileMetadata
	if json.Unmarshal([]byte(doc.ContentJSON), &meta) != nil || meta.UploadID == "" {
		return nil, ErrUnavailable
	}
	data, err := f.Client.Call(ctx, "POST", "/api/v2/upload-sessions/"+url.PathEscape(meta.UploadID)+"/complete", nil)
	if err != nil {
		return nil, domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件状态暂不可读取", 503)
	}
	var current fileMetadata
	if json.Unmarshal(data, &current) != nil || current.Status != "READY" || current.FileID != doc.FileID || current.SHA256 != meta.SHA256 {
		return nil, domain.Fail("FILE_NOT_READY", "文件尚未就绪或摘要不一致", 409)
	}
	return data, nil
}

// Validate 检查原件归属、用途与远端READY，付款不得借用他人凭证。
// 参数：ctx和actor为可信调用；ids是文件ID；purpose为后端指定用途。
// 返回值：全部合法时为空；文件不可用或越权返回错误。
func (f *Files) Validate(ctx context.Context, actor domain.Actor, ids []string, purpose string) error {
	if len(ids) == 0 || len(ids) > 20 {
		return domain.Fail("FILE_REQUIRED", "缺少文件凭证", 422)
	}
	for _, id := range ids {
		var doc store.Document
		if f.DB.WithContext(ctx).Where("tenant_id = ? AND file_id = ? AND owner = ? AND status IN ?", actor.TenantID, id, actor.SubjectID, []string{"READY", "PARSED", "REVIEW_REQUIRED"}).First(&doc).Error != nil {
			return domain.Fail("FILE_NOT_ACCESSIBLE", "文件未完成上传或无使用权限", 404)
		}
		data, err := f.Client.Call(ctx, http.MethodGet, "/api/v2/files/"+url.PathEscape(id), nil)
		if err != nil {
			return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件服务暂不可用", 503)
		}
		var meta fileMetadata
		if json.Unmarshal(data, &meta) != nil || meta.Status != "READY" || meta.Purpose != purpose {
			return domain.Fail("FILE_NOT_READY", "凭证文件未就绪或用途不符", 422)
		}
	}
	return nil
}

// downloadDocument 在服务端完成对象ACL后申请绑定资源的一次性下载票据。
// 参数：ctx为取消边界；doc为已验证本地归属的文档。
// 返回值：短效票据JSON；外部服务失败返回错误。
func (f *Files) downloadDocument(ctx context.Context, doc store.Document) (json.RawMessage, error) {
	return f.Client.Call(ctx, "POST", "/api/v2/files/"+url.PathEscape(doc.FileID)+"/download-tickets", map[string]string{"resource_type": "invoice_document", "resource_id": doc.ID})
}

// Download 在应用已完成当前对象ACL后核验同租户文档绑定并签发票据。
// 参数：ctx和actor限定调用；fileID不是访问授权。
// 返回值：一次性下载票据；无对象范围时统一404。
func (f *Files) Download(ctx context.Context, actor domain.Actor, fileID string) (json.RawMessage, error) {
	var doc store.Document
	if f.DB.WithContext(ctx).Where("tenant_id = ? AND file_id = ? AND status IN ?", actor.TenantID, fileID, []string{"READY", "PARSED", "REVIEW_REQUIRED"}).First(&doc).Error != nil {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "文件不存在或无权访问", 404)
	}
	// 此适配器仅由application.DownloadInvoice完成当前整单ACL及file_id归属校验后调用。
	// 组织财务范围不在这里重复映射为owner/TENANT_ALL，避免与应用权威策略分叉。
	if actor.SubjectID == "" {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "文件不存在或无权访问", 404)
	}
	return f.downloadDocument(ctx, doc)
}

// ReadDocument 在后台任务中读取已绑定且哈希匹配的原件，不接受用户提供URL。
// 参数：ctx为任务超时；tenant/documentID为持久化任务引用。
// 返回值：最多20MiB原件；票据、内容或摘要异常返回错误。
func (f *Files) ReadDocument(ctx context.Context, tenant, documentID string) ([]byte, error) {
	var doc store.Document
	if f.DB.WithContext(ctx).Where("tenant_id = ? AND id = ? AND status IN ?", tenant, documentID, []string{"READY", "PARSED", "REVIEW_REQUIRED"}).First(&doc).Error != nil {
		return nil, ErrUnavailable
	}
	data, err := f.downloadDocument(ctx, doc)
	if err != nil {
		return nil, err
	}
	var ticket struct {
		URL    string `json:"download_url"`
		Ticket string `json:"ticket"`
	}
	if json.Unmarshal(data, &ticket) != nil || ticket.Ticket == "" {
		return nil, ErrUnavailable
	}
	// 仅使用固定服务地址和绑定文件路径，忽略网关返回URL以封住SSRF与重定向。
	req, err := http.NewRequestWithContext(ctx, "GET", f.Client.BaseURL+"/api/v2/files/"+url.PathEscape(doc.FileID)+"/content", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "DownloadTicket "+ticket.Ticket)
	resp, err := f.Client.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (20<<20)+1))
	if err != nil || len(raw) > 20<<20 {
		return nil, ErrUnavailable
	}
	var meta fileMetadata
	if json.Unmarshal([]byte(doc.ContentJSON), &meta) != nil || meta.SHA256 == "" {
		return nil, ErrUnavailable
	}
	h := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(h[:]), meta.SHA256) {
		return nil, ErrUnavailable
	}
	return raw, nil
}

// StoreGenerated 将后台生成的授权导出写入网关，结果仍经本地文档归属授权。
// 参数：ctx为任务边界；actor为受控任务申请人；content为已过滤CSV；name为固定安全文件名。
// 返回值：READY文件ID；失败不会假报导出成功。
func (f *Files) StoreGenerated(ctx context.Context, actor domain.Actor, content []byte, name string) (string, error) {
	if len(content) == 0 || len(content) > 1<<20 {
		return "", domain.Fail("EXPORT_SIZE_LIMIT", "导出结果超出首发限制", 422)
	}
	id := ulid.Make().String()
	hash := sha256.Sum256(content)
	payload, _ := json.Marshal(Upload{Purpose: "reimbursement.export.result", OriginalName: name, MediaType: "text/csv", SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(hash[:]), IdempotencyKey: id})
	document := store.Document{ID: id, TenantID: actor.TenantID, Owner: actor.SubjectID, Status: "UPLOAD_PENDING", Version: 1, ContentJSON: string(payload)}
	if err := f.DB.WithContext(ctx).Create(&document).Error; err != nil {
		return "", err
	}
	data, err := f.createUpload(ctx, actor, id, payload, true)
	if err != nil {
		return "", err
	}
	var ticket struct {
		UploadID string `json:"upload_id"`
		Ticket   string `json:"ticket"`
		FileID   string `json:"file_id"`
	}
	if json.Unmarshal(data, &ticket) != nil || ticket.Ticket == "" {
		return "", ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", f.Client.BaseURL+"/api/v2/upload-sessions/"+url.PathEscape(ticket.UploadID)+"/content", bytes.NewReader(content))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "UploadTicket "+ticket.Ticket)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := f.Client.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		return "", ErrUnavailable
	}
	completed, err := f.CompleteUpload(ctx, actor, id)
	if err != nil {
		return "", err
	}
	if err = f.DB.WithContext(ctx).Model(&store.Document{}).Where("tenant_id = ? AND id = ?", actor.TenantID, id).Updates(map[string]any{"status": "READY", "content_json": string(completed)}).Error; err != nil {
		return "", err
	}
	return ticket.FileID, nil
}
