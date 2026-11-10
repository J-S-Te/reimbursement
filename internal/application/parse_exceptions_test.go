package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestQueryParseExceptionsAreOwnedLatestAndInvoiceFree 验证纯解析失败池不泄露其他主体或历史失败。
// 参数：t为测试上下文。
// 返回值：无。
func TestQueryParseExceptionsAreOwnedLatestAndInvoiceFree(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read", "expense.scope.tenant_all"}}
	at := time.Now().UTC().Add(-time.Hour)
	for i, status := range []string{"FAILED", "NEEDS_REVIEW", "PARTIAL", "RUNNING", "SUCCEEDED", "FAILED", "FAILED", "FAILED", "FAILED", "FAILED"} {
		id, tenant, owner := fmt.Sprint(i), "t", "app"
		if i == 5 {
			owner = "other"
		}
		if i == 6 {
			tenant = "other"
		}
		extra := ""
		if i == 9 {
			extra = `,"invoice_ids":["actual"]`
		}
		sourceSeedDocument(t, db, tenant, owner, id, "file-"+id, extra)
		run := store.InvoiceParseRun{ID: "run-" + id, TenantID: tenant, DocumentID: id, JobID: "job-" + id,
			Attempt: 1, Status: status, FailureCode: "PARSER_FAILED", CreatedAt: at,
			ResultJSON: `{"documents":[{"raw_fields":"SECRET_PARSER_DATA"}],"warnings":["PRIVATE"]}`}
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		if i == 7 {
			run.ID, run.JobID, run.Status, run.CreatedAt = "new-run", "new-job", "RUNNING", at.Add(time.Minute)
			if err := db.Create(&run).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Create(&store.Invoice{ID: "actual", TenantID: "t", Owner: "app", ContentJSON: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.InvoiceSource{ID: "manual", TenantID: "t", DocumentID: "8", InvoiceID: "actual", ParseRunID: "", CandidateIndex: -1, SourcesJSON: `[]`}).Error; err != nil {
		t.Fatal(err)
	}
	result := queryResultMap(t, s, a, "parse-exceptions", map[string]string{"page_size": "2"})
	if result["total"] != float64(3) || len(result["items"].([]any)) != 2 {
		t.Fatalf("仅本人当前三条无票异常且分页2条: %v", result)
	}
	for _, raw := range result["items"].([]any) {
		item := raw.(map[string]any)
		if item["candidate_count"] != float64(0) || item["parsed_candidate_count"] != float64(1) || item["parse_run_id"] == nil || item["document_version"] != float64(1) || item["parse_job_id"] == nil {
			t.Fatalf("候选数必须区分实际可用票和原始解析候选: %v", item)
		}
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "other") {
		t.Fatalf("不得暴露原始解析数据或隐藏主体: %s", raw)
	}
	result = queryResultMap(t, s, a, "parse-exceptions", map[string]string{"status": "FAILED"})
	if result["total"] != float64(1) {
		t.Fatalf("失败状态筛选应仅一条: %v", result)
	}
	result = queryResultMap(t, s, a, "parse-exceptions", map[string]string{"date_from": "2000-01-01", "date_to": "2000-01-01"})
	if result["total"] != float64(0) {
		t.Fatalf("合法日期条件须实际生效: %v", result)
	}
}

// TestQueryParseExceptionsRejectInvalidFiltersAndPermission 验证异常池权限和非法状态日期失败关闭。
// 参数：t为测试上下文。
// 返回值：无。
func TestQueryParseExceptionsRejectInvalidFiltersAndPermission(t *testing.T) {
	s, _, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read"}}
	for _, params := range []map[string]string{{"status": "RUNNING"}, {"date_from": "2026-02-30"}, {"date_from": "2026-11-12", "date_to": "2026-11-11"}} {
		_, err := s.Query(context.Background(), a, "parse-exceptions", "", params)
		if p, ok := err.(*domain.Problem); !ok || p.Status != 422 {
			t.Errorf("非法条件须422: %v %v", params, err)
		}
	}
	a.Permissions = nil
	_, err := s.Query(context.Background(), a, "parse-exceptions", "", nil)
	if p, ok := err.(*domain.Problem); !ok || p.Status != 403 {
		t.Fatalf("缺invoice.read须403: %v", err)
	}
}
