package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

const (
	// invoiceSourceSelectionLimit 与票据控制预检同源：单次命令最多20项来源选择。
	invoiceSourceSelectionLimit = 20
	// invoiceSourceRowLimit 与读取侧一致：单票据来源行总数不得超过1000。
	invoiceSourceRowLimit = 1000
)

// InvoiceSourceSelection 描述一份原件要追加的来源关系。
// 客户端只能引用服务器已归档的原件、不可变解析运行及其候选序号，不能提交页区坐标或摘要；
// run_id/candidate_index 与读取侧来源视图同名字段一致，便于前端按 items 回填选择。
type InvoiceSourceSelection struct {
	FileID         string `json:"file_id"`
	DocumentID     string `json:"document_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	CandidateIndex *int   `json:"candidate_index,omitempty"`
}

// frozenInvoiceSources 仅提取本发票关联的不可变run摘要及候选来源，不返回其他候选原文。
// 参数：ctx 为取消边界，invoice 为已经过当前对象ACL校验的发票。
// 返回值：独立来源列表、是否存在新式来源及错误。
func (s *Service) frozenInvoiceSources(ctx context.Context, invoice domain.Invoice) ([]any, bool, error) {
	db := s.db.WithContext(ctx)
	var sources []store.InvoiceSource
	if e := db.Where("tenant_id = ? AND invoice_id = ?", invoice.TenantID, invoice.ID).Order("created_at,id").Limit(1001).Find(&sources).Error; e != nil {
		return nil, false, e
	}
	if len(sources) > 1000 {
		return nil, false, domain.Fail("INVOICE_SOURCE_LIMIT", "票据来源超过单次读取上限", 422)
	}
	items := []any{}
	for _, source := range sources {
		view, e := sourceView(db, invoice, source)
		if e != nil {
			return nil, false, e
		}
		items = append(items, view)
	}
	return items, len(sources) > 0, nil
}

// sourceView 按固定来源行生成白名单摘要，文档只提供文件名等静态显示元数据。
// 参数：db 为数据库，invoice 为已授权发票，source 为不可变来源关系。
// 返回值：原件与运行摘要或完整性错误。
func sourceView(db *gorm.DB, invoice domain.Invoice, source store.InvoiceSource) (any, error) {
	var doc store.Document
	if e := db.Where("tenant_id = ? AND id = ?", invoice.TenantID, source.DocumentID).First(&doc).Error; e != nil {
		return nil, domain.Fail("INVOICE_SOURCE_MISSING", "来源文档记录缺失", 409)
	}
	var metadata struct {
		Filename  string `json:"filename"`
		MediaType string `json:"media_type"`
		Request   struct {
			Filename     string `json:"filename"`
			OriginalName string `json:"original_name"`
		} `json:"request"`
	}
	if e := json.Unmarshal([]byte(doc.ContentJSON), &metadata); e != nil {
		return nil, e
	}
	name := metadata.Filename
	if name == "" {
		name = metadata.Request.Filename
	}
	if name == "" {
		name = metadata.Request.OriginalName
	}
	hash := sha256.Sum256([]byte(source.SourcesJSON))
	if source.SourcesHash != "" && source.SourcesHash != hex.EncodeToString(hash[:]) {
		return nil, domain.Fail("INVOICE_SOURCE_HASH_MISMATCH", "来源关系摘要不一致", 409)
	}
	view := map[string]any{"id": source.ID, "document_id": doc.ID, "file_id": doc.FileID, "original_name": name, "media_type": metadata.MediaType, "run_id": source.ParseRunID, "candidate_index": source.CandidateIndex, "sources_hash": hex.EncodeToString(hash[:]), "sources": []any{}, "issues": []string{}}
	if source.ParseRunID == "" {
		var manual struct {
			Method string `json:"method"`
			Reason string `json:"reason"`
		}
		if e := json.Unmarshal([]byte(source.SourcesJSON), &manual); e != nil {
			return nil, e
		}
		view["source_kind"] = "MANUAL_LINK"
		view["parse_status"] = "MANUAL_LINK"
		view["manual_reason"] = manual.Reason
		return view, nil
	}
	var run store.InvoiceParseRun
	if e := db.Where("tenant_id = ? AND id = ? AND document_id = ?", invoice.TenantID, source.ParseRunID, doc.ID).First(&run).Error; e != nil {
		return nil, domain.Fail("INVOICE_PARSE_RUN_MISSING", "票据不可变解析运行缺失", 409)
	}
	var result struct {
		Warnings  []string `json:"warnings"`
		Documents []struct {
			Issues []string `json:"issues"`
		} `json:"documents"`
	}
	if e := json.Unmarshal([]byte(run.ResultJSON), &result); e != nil {
		return nil, e
	}
	issues := append([]string{}, result.Warnings...)
	if source.CandidateIndex >= 0 && source.CandidateIndex < len(result.Documents) {
		issues = append(issues, result.Documents[source.CandidateIndex].Issues...)
	}
	var coordinates []struct {
		Page    int     `json:"page,omitempty"`
		X       float64 `json:"x,omitempty"`
		Y       float64 `json:"y,omitempty"`
		Width   float64 `json:"width,omitempty"`
		Text    string  `json:"text,omitempty"`
		XMLPath string  `json:"xml_path,omitempty"`
	}
	if e := json.Unmarshal([]byte(source.SourcesJSON), &coordinates); e != nil {
		return nil, e
	}
	view["source_kind"] = "PARSED"
	view["parse_status"] = run.Status
	view["parser_version"] = run.ParserVersion
	view["rules_version"] = run.RulesVersion
	view["sources"] = coordinates
	view["issues"] = issues
	view["result_hash"] = run.ResultHash
	return view, nil
}

// linkSelectedInvoiceSources 为已授权票据按不可变解析运行或显式人工关联写入来源关系。
// 参数：tx 为票据命令事务，actor 为可信身份，invoice 为已确认归属的票据，selections 为受控来源选择，reason 为人工依据。
// 返回值：越权原件、无效运行、候选越界、越限或持久化错误；重复选择命中唯一键时保持幂等。
func linkSelectedInvoiceSources(tx *gorm.DB, actor domain.Actor, invoice domain.Invoice, selections []InvoiceSourceSelection, reason string) error {
	if len(selections) == 0 {
		return nil
	}
	if len(selections) > invoiceSourceSelectionLimit {
		return domain.Fail("FILE_REFERENCE_LIMIT", "单次来源选择最多20项", 422)
	}
	tenant := strings.TrimSpace(invoice.TenantID)
	if tenant == "" {
		tenant = actor.TenantID
	}
	if strings.TrimSpace(invoice.ID) == "" || tenant == "" {
		return domain.Fail("INVOICE_SOURCE_INVALID", "票据尚未建立可信业务身份", 422)
	}
	// 先写解析来源再补人工关联，避免同一原件同时留下两类来源行。
	for _, selection := range selections {
		if strings.TrimSpace(selection.RunID) == "" {
			continue
		}
		if e := linkParsedInvoiceSource(tx, tenant, actor, invoice, selection); e != nil {
			return e
		}
	}
	for _, selection := range selections {
		if strings.TrimSpace(selection.RunID) != "" {
			continue
		}
		if e := linkManualInvoiceSource(tx, tenant, actor, invoice, selection, reason); e != nil {
			return e
		}
	}
	return nil
}

// linkParsedInvoiceSource 仅用同租户同原件的不可变运行候选坐标建立解析来源。
// 参数：tx 为事务，tenant 为可信租户，actor 为操作身份，invoice 为票据，selection 为带运行的选择。
// 返回值：原件、运行、候选或持久化错误。
func linkParsedInvoiceSource(tx *gorm.DB, tenant string, actor domain.Actor, invoice domain.Invoice, selection InvoiceSourceSelection) error {
	doc, e := selectionDocument(tx, actor, invoice, selection)
	if e != nil {
		return e
	}
	raw, e := selectionCoordinates(tx, tenant, doc, selection)
	if e != nil {
		return e
	}
	return appendInvoiceSource(tx, store.InvoiceSource{TenantID: tenant, InvoiceID: invoice.ID, DocumentID: doc.ID, ParseRunID: strings.TrimSpace(selection.RunID), CandidateIndex: *selection.CandidateIndex, SourcesHash: parseSourceHash(raw), SourcesJSON: raw})
}

// linkManualInvoiceSource 为本人已授权原件建立不伪造解析运行的人工关联行。
// 参数：tx 为事务，tenant 为可信租户，actor 为操作身份，invoice 为票据，selection 为无运行的选择，reason 为人工依据。
// 返回值：原件或持久化错误；原件已有任一来源关系时保持既有语义不变。
func linkManualInvoiceSource(tx *gorm.DB, tenant string, actor domain.Actor, invoice domain.Invoice, selection InvoiceSourceSelection, reason string) error {
	if selection.CandidateIndex != nil && *selection.CandidateIndex != -1 {
		return domain.Fail("INVOICE_SOURCE_INVALID", "人工关联不接受候选序号", 422)
	}
	doc, e := selectionDocument(tx, actor, invoice, selection)
	if e != nil {
		return e
	}
	var count int64
	if e = tx.Model(&store.InvoiceSource{}).Where("tenant_id = ? AND invoice_id = ? AND document_id = ?", tenant, invoice.ID, doc.ID).Count(&count).Error; e != nil {
		return e
	}
	if count > 0 {
		return nil
	}
	raw, e := encode(map[string]any{"method": "MANUAL_LINK", "reason": reason, "invoice_version": invoice.Version})
	if e != nil {
		return e
	}
	return appendInvoiceSource(tx, store.InvoiceSource{TenantID: tenant, InvoiceID: invoice.ID, DocumentID: doc.ID, ParseRunID: "", CandidateIndex: -1, SourcesHash: parseSourceHash(raw), SourcesJSON: raw})
}

// appendInvoiceSource 幂等写入一行不可变来源关系，并保持单票据来源不超过读取上限。
// 参数：tx 为事务，source 为服务端已计算的来源关系。
// 返回值：相同唯一键已存在时成功且不重复写入；超过上限或持久化失败时拒绝。
func appendInvoiceSource(tx *gorm.DB, source store.InvoiceSource) error {
	var existing store.InvoiceSource
	e := tx.Where("tenant_id = ? AND invoice_id = ? AND document_id = ? AND parse_run_id = ? AND candidate_index = ?", source.TenantID, source.InvoiceID, source.DocumentID, source.ParseRunID, source.CandidateIndex).First(&existing).Error
	if e == nil {
		return nil
	}
	if !errors.Is(e, gorm.ErrRecordNotFound) {
		return e
	}
	var count int64
	if e = tx.Model(&store.InvoiceSource{}).Where("tenant_id = ? AND invoice_id = ?", source.TenantID, source.InvoiceID).Count(&count).Error; e != nil {
		return e
	}
	if count >= invoiceSourceRowLimit {
		return domain.Fail("INVOICE_SOURCE_LIMIT", "票据来源超过单票受控上限", 422)
	}
	if source.ID == "" {
		id, err := newID()
		if err != nil {
			return err
		}
		source.ID = id
	}
	if source.CreatedAt.IsZero() {
		source.CreatedAt = time.Now().UTC()
	}
	if e = tx.Create(&source).Error; e != nil {
		// 并发重复选择命中唯一键时仍按幂等成功处理，不产生重复来源行。
		var raced store.InvoiceSource
		if q := tx.Where("tenant_id = ? AND invoice_id = ? AND document_id = ? AND parse_run_id = ? AND candidate_index = ?", source.TenantID, source.InvoiceID, source.DocumentID, source.ParseRunID, source.CandidateIndex).First(&raced).Error; q == nil {
			return nil
		}
		return e
	}
	return nil
}

// selectionDocument 解析来源选择引用的原件，跨租户或他人文件不泄漏存在性。
// 参数：tx 为事务，actor 为操作身份，invoice 为票据，selection 为来源选择。
// 返回值：属于本人且已完成上传的授权原件；越权访问按404、用途或状态不符按422拒绝。
func selectionDocument(tx *gorm.DB, actor domain.Actor, invoice domain.Invoice, selection InvoiceSourceSelection) (store.Document, error) {
	var doc store.Document
	fileID := strings.TrimSpace(selection.FileID)
	if fileID == "" {
		return doc, domain.Fail("INVOICE_SOURCE_INVALID", "来源选择缺少原件标识", 422)
	}
	if e := tx.Where("tenant_id = ? AND file_id = ? AND owner = ?", actor.TenantID, fileID, actor.SubjectID).First(&doc).Error; e != nil {
		return doc, domain.Fail("RESOURCE_NOT_FOUND", "原件不存在或无权访问", 404)
	}
	if selection.DocumentID != "" && selection.DocumentID != doc.ID {
		return doc, domain.Fail("RESOURCE_NOT_FOUND", "原件不存在或无权访问", 404)
	}
	var metadata struct {
		Purpose string `json:"purpose"`
	}
	if e := json.Unmarshal([]byte(doc.ContentJSON), &metadata); e != nil {
		return doc, e
	}
	if (doc.Status != "READY" && doc.Status != "PARSED") || metadata.Purpose != "reimbursement.invoice.original" {
		return doc, domain.Fail("INVOICE_SOURCE_INVALID", "仅可引用已完成上传的票据原件", 422)
	}
	if !invoiceDocumentBound(doc, invoice) {
		return doc, domain.Fail("RESOURCE_NOT_FOUND", "原件不属于该票据", 404)
	}
	return doc, nil
}

// invoiceDocumentBound 判定原件是否属于该票据的原件集合或其解析候选关系。
// 参数：doc 为已授权原件，invoice 为票据。
// 返回值：属于票据原件集合或解析出的候选为该票据时为真。
func invoiceDocumentBound(doc store.Document, invoice domain.Invoice) bool {
	for _, id := range invoice.FileIDs {
		if id == doc.FileID {
			return true
		}
	}
	var metadata struct {
		InvoiceIDs []string `json:"invoice_ids"`
	}
	if e := json.Unmarshal([]byte(doc.ContentJSON), &metadata); e != nil {
		return false
	}
	for _, id := range metadata.InvoiceIDs {
		if id != "" && id == invoice.ID {
			return true
		}
	}
	return false
}

// selectionCoordinates 只从不可变运行结果取候选坐标，不接受客户端提交的坐标或摘要。
// 参数：tx 为事务，tenant 为可信租户，doc 为已授权原件，selection 为带运行及候选序号的选择。
// 返回值：候选页区坐标JSON；运行越权、候选越界或结果损坏时拒绝。
func selectionCoordinates(tx *gorm.DB, tenant string, doc store.Document, selection InvoiceSourceSelection) (string, error) {
	if selection.CandidateIndex == nil || *selection.CandidateIndex < 0 {
		return "", domain.Fail("INVOICE_SOURCE_INVALID", "解析来源须指明有效候选序号", 422)
	}
	index := *selection.CandidateIndex
	var run store.InvoiceParseRun
	if e := tx.Where("tenant_id = ? AND id = ? AND document_id = ?", tenant, strings.TrimSpace(selection.RunID), doc.ID).First(&run).Error; e != nil {
		return "", domain.Fail("RESOURCE_NOT_FOUND", "解析运行不存在或不属于该原件", 404)
	}
	var result struct {
		Documents []struct {
			Sources json.RawMessage `json:"sources"`
		} `json:"documents"`
	}
	if e := json.Unmarshal([]byte(run.ResultJSON), &result); e != nil {
		return "", e
	}
	if index >= len(result.Documents) {
		return "", domain.Fail("INVOICE_SOURCE_INVALID", "候选序号超出不可变解析结果范围", 422)
	}
	raw := result.Documents[index].Sources
	if len(raw) == 0 || string(raw) == "null" {
		return "[]", nil
	}
	var coordinates []json.RawMessage
	if e := json.Unmarshal(raw, &coordinates); e != nil {
		return "", domain.Fail("INVOICE_SOURCE_INVALID", "解析结果来源不是页区坐标数组", 422)
	}
	return string(raw), nil
}

// appendControlledInvoiceSources 为已确认票据受控追加来源关系，不改票面、归属或原件集合。
// 参数：tx 为票据命令事务，cc 为本人票据命令，input 为原件、来源选择及原因。
// 返回值：票据标识、当前版本及读侧来源视图；越权、越界或版本冲突时拒绝。
func appendControlledInvoiceSources(tx *gorm.DB, cc *commandContext, input InvoiceControlInput) (any, string, error) {
	if e := require(cc.actor, "expense.invoice.review"); e != nil {
		return nil, "", e
	}
	if strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 2000 {
		return nil, "", domain.Fail("REASON_REQUIRED", "请填写明确原因", 422)
	}
	row, claim, e := lockControlledInvoice(tx, cc.actor.TenantID, cc.resourceID)
	if e != nil {
		return nil, "", e
	}
	if row.Owner != cc.actor.SubjectID {
		return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "票据不存在或非本人票据", 404)
	}
	if e = verifyVersion(row.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	var invoice domain.Invoice
	if e = json.Unmarshal([]byte(row.ContentJSON), &invoice); e != nil {
		return nil, "", e
	}
	invoice.ID, invoice.TenantID, invoice.Owner = row.ID, row.TenantID, row.Owner
	if invoice.Version == 0 {
		invoice.Version = row.Version
	}
	files, e := appendableSourceFiles(tx, cc.actor, input.FileIDs)
	if e != nil {
		return nil, "", e
	}
	selections, e := controlledSourceSelections(files, input.SourceSelections)
	if e != nil {
		return nil, "", e
	}
	// 只在明确补证命令中纳入已核验的新原件，不改已批准票面的FileIDs。
	linkInvoice := invoice
	linkInvoice.FileIDs = append(append([]string{}, invoice.FileIDs...), files...)
	if e = linkSelectedInvoiceSources(tx, cc.actor, linkInvoice, selections, input.Reason); e != nil {
		return nil, "", e
	}
	if e = appendInvoiceEvidenceArchives(tx, claim); e != nil {
		return nil, "", e
	}
	items, e := invoiceSourceViews(tx, invoice)
	if e != nil {
		return nil, "", e
	}
	return map[string]any{"invoice_id": row.ID, "version": row.Version, "file_ids": invoice.FileIDs, "items": items}, row.ID, nil
}

// appendableSourceFiles 校验本次追加的每一份原件均为本租户本人已完成上传的票据原件。
// 参数：tx 为事务，actor 为操作身份，fileIDs 为请求原件引用。
// 返回值：去重后的受控原件引用；缺失、越权或超限时拒绝。
func appendableSourceFiles(tx *gorm.DB, actor domain.Actor, fileIDs []string) ([]string, error) {
	if len(fileIDs) == 0 || len(fileIDs) > invoiceSourceSelectionLimit {
		return nil, domain.Fail("FILE_REFERENCE_LIMIT", "来源追加须关联1至20份授权原件", 422)
	}
	files := make([]string, 0, len(fileIDs))
	seen := map[string]bool{}
	for _, id := range fileIDs {
		if strings.TrimSpace(id) == "" {
			return nil, domain.Fail("INVOICE_SOURCE_INVALID", "来源追加缺少原件标识", 422)
		}
		doc, e := invoiceControlDocument(tx, actor, id)
		if e != nil {
			return nil, e
		}
		if !seen[doc.FileID] {
			seen[doc.FileID] = true
			files = append(files, doc.FileID)
		}
	}
	return files, nil
}

// controlledSourceSelections 把本次原件集合与来源选择合并为必须写入的完整选择列表。
// 参数：files 为本次受控原件，selections 为客户端来源选择。
// 返回值：覆盖全部原件的选择列表；选择引用非本次原件或总数超限时拒绝。
func controlledSourceSelections(files []string, selections []InvoiceSourceSelection) ([]InvoiceSourceSelection, error) {
	allowed := map[string]bool{}
	for _, id := range files {
		allowed[id] = true
	}
	covered := map[string]bool{}
	out := make([]InvoiceSourceSelection, 0, len(files)+len(selections))
	for _, selection := range selections {
		fileID := strings.TrimSpace(selection.FileID)
		if !allowed[fileID] {
			return nil, domain.Fail("INVOICE_SOURCE_INVALID", "来源选择必须引用本次追加的原件", 422)
		}
		selection.FileID = fileID
		covered[fileID] = true
		out = append(out, selection)
	}
	// 未指明解析运行的原件按显式人工关联追加，避免只报文件而不留下可追溯来源。
	for _, id := range files {
		if !covered[id] {
			out = append(out, InvoiceSourceSelection{FileID: id})
		}
	}
	if len(out) > invoiceSourceSelectionLimit {
		return nil, domain.Fail("FILE_REFERENCE_LIMIT", "单次来源选择最多20项", 422)
	}
	return out, nil
}

// invoiceSourceViews 按读侧白名单返回票据当前全部来源视图。
// 参数：tx 为事务，invoice 为已授权票据。
// 返回值：来源视图列表或规模/完整性错误。
func invoiceSourceViews(tx *gorm.DB, invoice domain.Invoice) ([]any, error) {
	var rows []store.InvoiceSource
	if e := tx.Where("tenant_id = ? AND invoice_id = ?", invoice.TenantID, invoice.ID).Order("created_at,id").Limit(invoiceSourceRowLimit + 1).Find(&rows).Error; e != nil {
		return nil, e
	}
	if len(rows) > invoiceSourceRowLimit {
		return nil, domain.Fail("INVOICE_SOURCE_LIMIT", "票据来源超过单次读取上限", 422)
	}
	items := []any{}
	for _, row := range rows {
		view, e := sourceView(tx, invoice, row)
		if e != nil {
			return nil, e
		}
		items = append(items, view)
	}
	return items, nil
}
