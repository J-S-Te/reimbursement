package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// readInvoiceArchiveEvidence 读取请求冻结的案件内容和来源精确集合，并将补充文件纳入完整性检查。
// 参数：ctx 为数据库边界，row 为归档任务，request 为已验证的持久请求，files 为本包文件集合。
// 返回值：补充清单；旧请求显式标记未冻结，不读取后来状态填充旧包。
func (p *Processor) readInvoiceArchiveEvidence(
	ctx context.Context,
	row Record,
	request archiveFrozenRequest,
	files map[string]bool,
) (map[string]any, error) {
	if request.InvoiceEvidence == nil {
		return map[string]any{"status": "LEGACY_NOT_FROZEN", "sources": []any{}, "controls": []any{}}, nil
	}
	frozen := request.InvoiceEvidence
	if len(frozen.Controls) > 3000 || validateArchiveIDs(frozen.SourceIDs) != nil {
		return nil, errors.New("ARCHIVE_INVOICE_EVIDENCE_LIMIT")
	}
	for _, control := range frozen.Controls {
		var metadata struct {
			TenantID string `json:"tenant_id"`
			ClaimID  string `json:"claim_id"`
		}
		if json.Unmarshal(control.Record, &metadata) != nil ||
			metadata.TenantID != row.TenantID ||
			metadata.ClaimID != "" && metadata.ClaimID != request.ClaimID && control.Kind == "IMPACT" {
			return nil, errors.New("ARCHIVE_INVOICE_CONTROL_SCOPE_INVALID")
		}
		if err := collectEvidenceIDs(string(control.Content), files); err != nil {
			return nil, err
		}
	}
	sources, err := p.readArchiveInvoiceSources(ctx, row.TenantID, frozen.SourceIDs, files)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "FROZEN", "sources": sources, "controls": frozen.Controls}, nil
}

// readArchiveInvoiceSources 仅读取冻结来源集合，校验摘要并收集同租户补充原件。
// 参数：ctx 为数据库边界，tenant 为任务租户，ids 为精确来源ID，files 为待校验集合。
// 返回值：原始来源页区与摘要；缺失或超限时拒绝封存。
func (p *Processor) readArchiveInvoiceSources(ctx context.Context, tenant string, ids []string, files map[string]bool) ([]map[string]any, error) {
	out := []map[string]any{}
	if len(ids) == 0 {
		return out, nil
	}
	q := p.DB.WithContext(ctx).Model(&store.InvoiceSource{}).Where("tenant_id = ? AND id IN ?", tenant, ids)
	var size int64
	if err := q.Select("COALESCE(SUM(LENGTH(sources_json)),0)").Row().Scan(&size); err != nil {
		return nil, err
	}
	if size > 2<<20 {
		return nil, errors.New("ARCHIVE_INVOICE_SOURCE_LIMIT")
	}
	var sources []store.InvoiceSource
	if err := q.Select("*").Order("id").Find(&sources).Error; err != nil {
		return nil, err
	}
	if len(sources) != len(ids) {
		return nil, errors.New("ARCHIVE_INVOICE_SOURCE_MISSING")
	}
	for _, source := range sources {
		hash := sha256.Sum256([]byte(source.SourcesJSON))
		if !json.Valid([]byte(source.SourcesJSON)) || hex.EncodeToString(hash[:]) != source.SourcesHash {
			return nil, errors.New("ARCHIVE_INVOICE_SOURCE_HASH_MISMATCH")
		}
		var doc store.Document
		if err := p.DB.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenant, source.DocumentID).First(&doc).Error; err != nil {
			return nil, errors.New("ARCHIVE_INVOICE_SOURCE_DOCUMENT_MISSING")
		}
		if doc.FileID == "" {
			return nil, errors.New("ARCHIVE_INVOICE_SOURCE_DOCUMENT_MISSING")
		}
		files[doc.FileID] = true
		out = append(out, map[string]any{"id": source.ID, "invoice_id": source.InvoiceID, "document_id": doc.ID, "file_id": doc.FileID, "run_id": source.ParseRunID, "candidate_index": source.CandidateIndex, "sources_hash": source.SourcesHash, "sources": json.RawMessage(source.SourcesJSON)})
	}
	return out, nil
}
