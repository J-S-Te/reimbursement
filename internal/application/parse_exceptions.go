package application

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
	"gorm.io/gorm"
)

type parseExceptionRow struct {
	DocumentID, FileID, OriginalName, DocumentStatus string
	ParseRunID, ParseJobID, ParseStatus, ErrorCode   string
	DocumentVersion                                  int64
	ParsedCandidateCount                             int
	CreatedAt                                        time.Time
	CompletedAt                                      *time.Time
}

// ParseExceptions 读取本人原件尚未生成业务票据的最新失败或待人工处理运行。
// 参数：ctx为取消边界，a为可信身份，params为状态、北京日期和有限分页条件。
// 返回值：不含原始解析内容的异常页；无票据读取权限或非法条件时拒绝。
func (s *Service) ParseExceptions(ctx context.Context, a domain.Actor, params map[string]string) (any, error) {
	return s.parseExceptions(s.db.WithContext(ctx), a, params)
}

// parseExceptions 在计数和分页前同时限制租户、原件所有权、最新运行和实际无票条件。
// 参数：db为当前查询上下文，a为可信身份，params为受限列表条件。
// 返回值：本人纯解析异常页或权限、校验及存储错误。
func (s *Service) parseExceptions(db *gorm.DB, a domain.Actor, params map[string]string) (any, error) {
	if err := require(a, "expense.invoice.read"); err != nil {
		return nil, err
	}
	f, err := parseExceptionFilters(params)
	if err != nil {
		return nil, err
	}
	q := parseExceptionScope(db, a)
	if f.Status != "" {
		q = q.Where("r.status = ?", f.Status)
	}
	if !f.From.IsZero() {
		q = q.Where("r.created_at >= ?", f.From.UTC())
	}
	if !f.Until.IsZero() {
		q = q.Where("r.created_at < ?", f.Until.UTC())
	}
	if params["document_id"] != "" {
		q = q.Where("d.id = ?", params["document_id"])
	}
	var total int64
	if err = q.Count(&total).Error; err != nil {
		return nil, err
	}
	page, size, offset := pageArgs(params)
	var rows []parseExceptionRow
	selection := "d.id AS document_id, d.version AS document_version, d.file_id, " +
		"d.status AS document_status, r.id AS parse_run_id, r.job_id AS parse_job_id, " +
		"r.status AS parse_status, " +
		"r.failure_code AS error_code, r.created_at, r.completed_at, " +
		queryJSONText(db, "d.content_json", "filename") + " AS original_name, " +
		queryJSONArrayLength(db, "r.result_json", "documents") + " AS parsed_candidate_count"
	if err = q.Select(selection).Order("r.created_at DESC, r.attempt DESC, r.id DESC").
		Limit(size).Offset(offset).Scan(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"document_id": row.DocumentID, "document_version": row.DocumentVersion,
			"file_id": row.FileID, "original_name": row.OriginalName,
			"document_status": row.DocumentStatus, "parse_run_id": row.ParseRunID,
			"parse_job_id": row.ParseJobID,
			"parse_status": row.ParseStatus, "error_code": row.ErrorCode,
			"candidate_count": 0, "parsed_candidate_count": row.ParsedCandidateCount,
			"created_at": row.CreatedAt, "completed_at": row.CompletedAt,
		})
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}, nil
}

// parseExceptionFilters 仅接受实际异常运行状态和合法北京日期，避免静默放宽。
// 参数：params为客户端查询条件。
// 返回值：合法条件；状态、日期及文档标识不合法时返回422。
func parseExceptionFilters(params map[string]string) (queryFilters, error) {
	f := queryFilters{Status: params["status"]}
	if f.Status != "" && f.Status != "FAILED" && f.Status != "NEEDS_REVIEW" && f.Status != "PARTIAL" {
		return f, domain.Fail("INVALID_QUERY_FILTER", "仅支持FAILED、NEEDS_REVIEW或PARTIAL异常状态", 422)
	}
	if utf8.RuneCountInString(params["document_id"]) > 64 {
		return f, domain.Fail("INVALID_QUERY_FILTER", "文档标识过长", 422)
	}
	err := parseQueryDates(params, &f)
	return f, err
}

// parseExceptionScope 构造不返回已有人工或解析业务发票的最新运行查询。
// 参数：db为基础库，a为可信身份，原件权限与现有解析任务读取同为本人所有。
// 返回值：已限制租户、所有权、票据用途及最新异常运行的查询。
func parseExceptionScope(db *gorm.DB, a domain.Actor) *gorm.DB {
	newer := db.Table("exp_invoice_parse_run AS newer").Select("1").
		Where("newer.tenant_id = r.tenant_id AND newer.document_id = r.document_id").
		Where("(newer.created_at > r.created_at OR (newer.created_at = r.created_at AND " +
			"(newer.attempt > r.attempt OR (newer.attempt = r.attempt AND newer.id > r.id))))")
	sources := db.Table("exp_invoice_source AS src").Select("1").
		Joins("JOIN exp_invoice AS inv ON inv.tenant_id = src.tenant_id AND inv.id = src.invoice_id").
		Where("src.tenant_id = d.tenant_id AND src.document_id = d.id")
	return db.Table("exp_invoice_parse_run AS r").
		Joins("JOIN exp_invoice_document AS d ON d.tenant_id = r.tenant_id AND d.id = r.document_id").
		Where("r.tenant_id = ? AND d.owner = ?", a.TenantID, a.SubjectID).
		Where(queryJSONText(db, "d.content_json", "purpose")+" = ?", "reimbursement.invoice.original").
		Where("r.status IN ?", []string{"FAILED", "NEEDS_REVIEW", "PARTIAL"}).
		Where("NOT EXISTS (?)", newer).Where("NOT EXISTS (?)", sources).
		Where(queryJSONArrayLength(db, "d.content_json", "invoice_ids") + " = 0")
}

// queryJSONArrayLength 生成受控JSON数组长度表达式，缺少字段按零处理。
// 参数：db为SQL方言，column和field均为服务端固定列及字段。
// 返回值：SQLite与MySQL语义一致的SQL表达式，不拼接客户端值。
func queryJSONArrayLength(db *gorm.DB, column, field string) string {
	function := "JSON_ARRAY_LENGTH"
	if db.Dialector.Name() == "mysql" {
		function = "JSON_LENGTH"
	}
	return "COALESCE(" + function + "(JSON_EXTRACT(" + column + ", '$." + field + "')), 0)"
}
