package jobs

import (
	"context"
	"errors"
	"time"

	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// archiveRequestedParseReferences 按冻结来源派生新清单，仅旧请求使用时间边界兼容读取。
// 参数：ctx 为取消边界，tenant/documents 为已验证租户与原件，cutoff/request 为持久请求。
// 返回值：精确解析运行和来源；旧请求明确记录兼容标记，缺件或错配时拒绝封存。
func (p *Processor) archiveRequestedParseReferences(
	ctx context.Context, tenant string, documents []string, cutoff time.Time, request *archiveFrozenRequest,
) ([]archiveParseRunRef, []archiveSourceRef, error) {
	if request.InvoiceEvidence == nil {
		request.LegacyFallbacks = append(request.LegacyFallbacks,
			"LEGACY_INVOICE_REFERENCES_WITHOUT_FROZEN_SOURCE_IDS")
		return p.archiveParseReferences(ctx, tenant, documents, cutoff)
	}
	sources, err := p.archiveFrozenSourceReferences(ctx, tenant, request.InvoiceEvidence.SourceIDs)
	if err != nil {
		return nil, nil, err
	}
	runs, err := p.archiveFrozenParseRuns(ctx, tenant, sources, cutoff)
	return runs, sources, err
}

// archiveFrozenSourceReferences 读取请求明确选中的来源，不按原件推断其他票据或后续来源。
// 参数：ctx 为取消边界，tenant 为可信租户，ids 为已冻结来源标识。
// 返回值：完整的来源元数据；重复、超限、跨租户或缺失时拒绝。
func (p *Processor) archiveFrozenSourceReferences(
	ctx context.Context, tenant string, ids []string,
) ([]archiveSourceRef, error) {
	sources := []archiveSourceRef{}
	if err := validateArchiveIDs(ids); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return sources, nil
	}
	err := p.DB.WithContext(ctx).Model(&store.InvoiceSource{}).
		Select("id,invoice_id,document_id,parse_run_id,candidate_index,sources_hash").
		Where("tenant_id = ? AND id IN ?", tenant, ids).Order("id").Scan(&sources).Error
	if err != nil {
		return nil, err
	}
	if len(sources) != len(ids) {
		return nil, errors.New("ARCHIVE_INVOICE_SOURCE_MISSING")
	}
	return sources, nil
}

// archiveFrozenParseRuns 仅归档冻结来源实际引用的不可变解析运行，并逐项复核原件关系。
// 参数：ctx 为取消边界，tenant 为可信租户，sources 为精确来源，cutoff 为请求时刻。
// 返回值：去重的运行元数据；缺失、原件错配或请求时尚未完成时拒绝。
func (p *Processor) archiveFrozenParseRuns(
	ctx context.Context, tenant string, sources []archiveSourceRef, cutoff time.Time,
) ([]archiveParseRunRef, error) {
	runs := []archiveParseRunRef{}
	documents := map[string]string{}
	ids := []string{}
	for _, source := range sources {
		if source.ParseRunID == "" {
			continue
		}
		if doc, exists := documents[source.ParseRunID]; exists {
			if doc != source.DocumentID {
				return nil, errors.New("ARCHIVE_PARSE_SOURCE_DOCUMENT_MISMATCH")
			}
			continue
		}
		documents[source.ParseRunID] = source.DocumentID
		ids = append(ids, source.ParseRunID)
	}
	if len(ids) == 0 {
		return runs, nil
	}
	err := p.DB.WithContext(ctx).Model(&store.InvoiceParseRun{}).
		Select("id,document_id,parser_version,rules_version,input_sha256,status,result_hash,failure_code,created_at,completed_at").
		Where("tenant_id = ? AND id IN ?", tenant, ids).Order("created_at,id").Scan(&runs).Error
	if err != nil {
		return nil, err
	}
	if len(runs) != len(ids) {
		return nil, errors.New("ARCHIVE_INVOICE_PARSE_RUN_MISSING")
	}
	for _, run := range runs {
		if run.DocumentID != documents[run.ID] {
			return nil, errors.New("ARCHIVE_PARSE_SOURCE_DOCUMENT_MISMATCH")
		}
		if run.CreatedAt.After(cutoff) || run.CompletedAt != nil && run.CompletedAt.After(cutoff) {
			return nil, errors.New("ARCHIVE_PARSE_COMPLETED_AFTER_REQUEST")
		}
		if run.Status == "RUNNING" {
			return nil, errors.New("ARCHIVE_PARSE_IN_PROGRESS")
		}
	}
	return runs, nil
}
