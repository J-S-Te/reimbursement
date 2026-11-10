package application

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// seedArchiveProject 保存有封存版本的单据，付款净额仍由真实效果夹具生成。
// 参数：t 为测试上下文，db 为临时库，id/project 为单据与项目，reversed 为冲正标记。
// 返回值：无。
func seedArchiveProject(t *testing.T, db *gorm.DB, id, tenant, applicant, project string, reversed bool) {
	t.Helper()
	status := "PAID"
	if reversed {
		status = "APPROVED_PENDING_PAYMENT"
	}
	c := domain.Claim{ID: id, TenantID: tenant, Applicant: applicant, OrgID: "org", ClaimNo: "BX-" + id, Title: "归档检索", Status: status, Payable: 100, Items: []domain.Item{{ID: "i", Requested: 100, Allocations: []domain.Allocation{{ProjectID: project, Amount: 100}}}}}
	seedStatisticsPayment(t, db, c, reversed)
	for version := 1; version <= 2; version++ {
		sealed := time.Date(2026, 11, 10, 16, version, 0, 0, time.UTC)
		manifest, _ := json.Marshal(map[string]any{"sealed_at": sealed.Format(time.RFC3339Nano)})
		row := store.Archive{ID: id + string(rune('a'+version)), TenantID: tenant, ClaimID: id,
			PackageVersion: int64(version), Status: "SEALED", ManifestJSON: string(manifest), CreatedAt: sealed}
		if e := db.Create(&row).Error; e != nil {
			t.Fatal(e)
		}
	}
}

// archiveProjectsResult 读取真实归档项目聚合服务响应。
// 参数：t 为测试上下文，s 为服务，a 为可信身份，params 为组合筛选。
// 返回值：解码业务响应，失败时终止测试。
func archiveProjectsResult(t *testing.T, s *Service, a domain.Actor, params map[string]string) map[string]any {
	t.Helper()
	value, e := s.ArchiveProjects(context.Background(), a, params)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	var result map[string]any
	if e = json.Unmarshal(raw, &result); e != nil {
		t.Fatal(e)
	}
	return result
}

// TestArchiveProjectsCurrentNetAndTenantScope 验证项目档案不重复计版本或历史已冲正毛额。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchiveProjectsCurrentNetAndTenantScope(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read", "expense.archive.read"}}
	seedArchiveProject(t, db, "paid", "t", "app", "p1", false)
	seedArchiveProject(t, db, "reversed", "t", "app", "p1", true)
	seedArchiveProject(t, db, "foreign", "foreign", "app", "p1", false)
	seedArchiveProject(t, db, "other-owner", "t", "other", "p1", false)
	result := archiveProjectsResult(t, s, a, nil)
	rows := result["items"].([]any)
	if result["total"] != float64(1) || len(rows) != 1 {
		t.Fatalf("应只聚合本人当前租户项目: %v", result)
	}
	row := rows[0].(map[string]any)
	if row["claim_count"] != float64(2) || row["amount_minor"] != "100" {
		t.Fatalf("档案多版本及冲正不得重复金额: %v", result)
	}
}

// TestArchiveProjectsDoNotPromoteTaskScope 验证审批任务可见性不能扩大为档案读取权。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchiveProjectsDoNotPromoteTaskScope(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.archive.read", "expense.approval.act"}}
	seedArchiveProject(t, db, "scope", "t", "app", "p1", false)
	raw, _ := json.Marshal(domain.Task{ID: "task", ClaimID: "scope", Assignee: "lead", AllocationKeys: []string{"i/p1/"}})
	if e := db.Create(&store.Task{ID: "task", TenantID: "t", ClaimID: "scope", Assignee: "lead", ContentJSON: string(raw)}).Error; e != nil {
		t.Fatal(e)
	}
	result := archiveProjectsResult(t, s, a, map[string]string{"project_id": "p1"})
	if result["total"] != float64(0) {
		t.Fatalf("任务不得自动获得整单档案范围: %v", result)
	}
}

// TestArchiveProjectsPaginationAndBeijingArchiveDay 验证按封存日筛选后对完整项目聚合分页。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchiveProjectsPaginationAndBeijingArchiveDay(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read", "expense.archive.read"}}
	seedArchiveProject(t, db, "one", "t", "app", "p1", false)
	seedArchiveProject(t, db, "two", "t", "app", "p2", false)
	result := archiveProjectsResult(t, s, a, map[string]string{"date_from": "2026-11-11", "date_to": "2026-11-11", "q": "归档", "page": "2", "page_size": "1"})
	rows := result["items"].([]any)
	if result["total"] != float64(2) || len(rows) != 1 || rows[0].(map[string]any)["project_id"] != "p2" {
		t.Fatalf("应在完整聚合后分页: %v", result)
	}
	filtered := archiveProjectsResult(t, s, a, map[string]string{"project_id": "p2"})
	if filtered["total"] != float64(1) || filtered["items"].([]any)[0].(map[string]any)["project_id"] != "p2" {
		t.Fatalf("项目过滤不能返回其他项目: %v", filtered)
	}
	result = archiveProjectsResult(t, s, a, map[string]string{"date_to": "2026-11-10"})
	if result["total"] != float64(0) {
		t.Fatalf("UTC11月10日16点属于北京11日，不应提前落入10日: %v", result)
	}
	_, e := s.ArchiveProjects(context.Background(), a, map[string]string{"date_from": "2026-02-30"})
	p, ok := e.(*domain.Problem)
	if !ok || p.Status != 422 {
		t.Fatalf("非法日期须422: %v", e)
	}
}

// TestArchiveProjectsUseActualSealingTime 验证请求日与真实封存日不同不会错误归属。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchiveProjectsUseActualSealingTime(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read", "expense.archive.read"}}
	seedArchiveProject(t, db, "late", "t", "app", "p1", false)
	manifest := `{"sealed_at":"2026-11-12T16:00:00.001Z"}`
	if e := db.Model(&store.Archive{}).Where("claim_id = ?", "late").Updates(map[string]any{"manifest_json": manifest}).Error; e != nil {
		t.Fatal(e)
	}
	result := archiveProjectsResult(t, s, a, map[string]string{"date_from": "2026-11-13", "date_to": "2026-11-13"})
	if result["total"] != float64(1) {
		t.Fatalf("必须按北京真实封存日而非请求日归属: %v", result)
	}
	rows := result["items"].([]any)
	if rows[0].(map[string]any)["last_archived_at"] != "2026-11-12T16:00:00.001Z" {
		t.Fatalf("最近封存时间应来自真实manifest: %v", result)
	}
}

// TestArchiveProjectsReadAllClaimsAndRequirePermission 验证跨批次全量聚合及独立档案权限。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchiveProjectsReadAllClaimsAndRequirePermission(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read", "expense.archive.read"}}
	for i := 0; i < 101; i++ {
		id := fmt.Sprintf("c%03d", i)
		c := domain.Claim{ID: id, TenantID: "t", Applicant: "app", Status: "APPROVED_PENDING_PAYMENT", Items: []domain.Item{{ID: "i", Requested: 100, Allocations: []domain.Allocation{{ProjectID: "p1", Amount: 100}}}}}
		seedQueryClaim(t, db, c, "")
		if e := db.Create(&store.Archive{ID: "a" + id, TenantID: "t", ClaimID: id, Status: "SEALED", PackageVersion: 1, ManifestJSON: `{"sealed_at":"2026-11-11T00:00:00Z"}`}).Error; e != nil {
			t.Fatal(e)
		}
	}
	result := archiveProjectsResult(t, s, a, map[string]string{"page_size": "1"})
	if result["items"].([]any)[0].(map[string]any)["claim_count"] != float64(101) {
		t.Fatalf("不可只汇总列表当前页或首批次: %v", result)
	}
	_, e := s.ArchiveProjects(context.Background(), domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}, nil)
	p, ok := e.(*domain.Problem)
	if !ok || p.Status != 403 {
		t.Fatalf("档案读取必须具备独立权限: %v", e)
	}
}
