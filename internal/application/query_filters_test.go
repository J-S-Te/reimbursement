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
	"gorm.io/gorm"
)

// seedQueryClaim 保存独立查询夹具，允许覆盖旧版聚合缺少的类型字段。
// 参数：t 为测试上下文，db 为临时库，c 为单据，claimType 为可选类型。
// 返回值：无，失败时终止测试。
func seedQueryClaim(t *testing.T, db *gorm.DB, c domain.Claim, claimType string) {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if claimType != "" {
		fields["claim_type"] = claimType
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	row := store.Claim{ID: c.ID, TenantID: c.TenantID, Applicant: c.Applicant, OrgID: c.OrgID, Status: c.Status, Version: 1, ContentJSON: string(raw), CreatedAt: c.CreatedAt}
	if err = db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

// queryResultMap 通过真实查询入口获取业务响应。
// 参数：t 为测试上下文，s 为服务，a 为身份，kind 和 params 为查询条件。
// 返回值：反序列化响应，失败时终止测试。
func queryResultMap(t *testing.T, s *Service, a domain.Actor, kind string, params map[string]string) map[string]any {
	t.Helper()
	raw, err := s.Query(context.Background(), a, kind, "", params)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestQueryClaimFiltersRespectBeijingDay 验证组合筛选、北京日期边界与租户隔离。
// 参数：t 为测试上下文。
// 返回值：无。
func TestQueryClaimFiltersRespectBeijingDay(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	start := time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{start.Add(-time.Second), start, start.Add(24*time.Hour - time.Second), start.Add(24 * time.Hour)} {
		c := domain.Claim{ID: fmt.Sprint(i), TenantID: "t", Applicant: "app", OrgID: "org", ClaimNo: "BX-2026", Title: "出差100%", Status: "DRAFT", CreatedAt: at}
		seedQueryClaim(t, db, c, "TRAVEL")
	}
	seedQueryClaim(t, db, domain.Claim{ID: "foreign", TenantID: "other", Applicant: "app", Status: "DRAFT", Title: "出差100%", CreatedAt: start}, "TRAVEL")
	result := queryResultMap(t, s, a, "claims", map[string]string{"q": "100%", "date_from": "2026-11-11", "date_to": "2026-11-11", "claim_type": "TRAVEL", "applicant_subject_id": "app"})
	if result["total"] != float64(2) {
		t.Fatalf("北京业务日应仅匹配两单: %v", result)
	}
	result = queryResultMap(t, s, a, "claims", map[string]string{"q": "100_"})
	if result["total"] != float64(0) {
		t.Fatalf("通配符必须按字面量匹配: %v", result)
	}
}

// TestQueryFiltersRejectInvalidInput 验证非法日期、枚举和超长输入不会静默忽略。
// 参数：t 为测试上下文。
// 返回值：无。
func TestQueryFiltersRejectInvalidInput(t *testing.T) {
	s, _, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read", "expense.invoice.read", "expense.approval.act"}}
	cases := []struct {
		kind   string
		params map[string]string
	}{
		{"claims", map[string]string{"q": strings.Repeat("a", 129)}},
		{"claims", map[string]string{"date_from": "2026-02-30"}},
		{"claims", map[string]string{"date_from": "2026-11-12", "date_to": "2026-11-11"}},
		{"claims", map[string]string{"status": "PENDING"}},
		{"claims", map[string]string{"claim_type": "NON_PROJECT"}},
		{"invoices", map[string]string{"verification_status": "SUCCESS"}},
		{"invoices", map[string]string{"review_status": "OK"}},
		{"approval-tasks", map[string]string{"status": "PAID"}},
	}
	for _, tc := range cases {
		_, err := s.Query(context.Background(), a, tc.kind, "", tc.params)
		public, ok := err.(*domain.Problem)
		if !ok || public.Status != 422 {
			t.Errorf("%s %v 应返回422，实际 %v", tc.kind, tc.params, err)
		}
	}
}

// TestQueryInvoiceFiltersUseInvoiceDay 验证台账字段筛选与票面日期闭区间。
// 参数：t 为测试上下文。
// 返回值：无。
func TestQueryInvoiceFiltersUseInvoiceDay(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read"}}
	for i, date := range []string{"2026-11-10", "2026-11-11", "2026-11-12"} {
		inv := domain.Invoice{ID: fmt.Sprint(i), TenantID: "t", Owner: "app", InvoiceNumber: "INV-100%", InvoiceType: "VAT", InvoiceDate: date, ReviewStatus: "CONFIRMED", VerificationStatus: "MANUAL_REVIEWED"}
		raw, _ := json.Marshal(inv)
		if err := db.Create(&store.Invoice{ID: inv.ID, TenantID: "t", Owner: "app", ReservationState: "AVAILABLE", ContentJSON: string(raw)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	result := queryResultMap(t, s, a, "invoices", map[string]string{"q": "100%", "invoice_type": "VAT", "review_status": "CONFIRMED", "verification_status": "MANUAL_REVIEWED", "date_from": "2026-11-11", "date_to": "2026-11-11"})
	if result["total"] != float64(1) {
		t.Fatalf("应按票面日筛选一票: %v", result)
	}
}

// TestQueryDashboardPaidMonthUsesActivePayment 验证本月本人付款按有效付款业务日统计。
// 参数：t 为测试上下文。
// 返回值：无。
func TestQueryDashboardPaidMonthUsesActivePayment(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	month := time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01")
	for i, status := range []string{"PAID", "APPROVED_PENDING_PAYMENT"} {
		id := fmt.Sprint(i)
		seedQueryClaim(t, db, domain.Claim{ID: id, TenantID: "t", Applicant: "app", Status: status}, "")
		if err := db.Model(&store.Claim{}).Where("id = ?", id).Update("active_payment_id", "pay"+id).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&store.Payment{ID: "pay" + id, TenantID: "t", ClaimID: id, ContentJSON: `{"paid_date":"` + month + `-01"}`}).Error; err != nil {
			t.Fatal(err)
		}
	}
	result := queryResultMap(t, s, a, "dashboard", nil)
	if result["my_paid_this_month"] != float64(1) {
		t.Fatalf("仅统计本月有效已付单据: %v", result)
	}
	if _, ok := result["pending_tasks"].([]any); !ok {
		t.Fatalf("无审批能力应返回空待办数组: %v", result)
	}
}

// TestQueryProjectFilterCannotRevealOtherAllocation 验证项目筛选不会暴露本人任务未覆盖的项目。
// 参数：t 为测试上下文。
// 返回值：无。
func TestQueryProjectFilterCannotRevealOtherAllocation(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	c := domain.Claim{ID: "scoped", TenantID: "t", Applicant: "app", OrgID: "org", Status: "IN_APPROVAL", CurrentRound: 1, Items: []domain.Item{{ID: "visible", Requested: 100, Allocations: []domain.Allocation{{ProjectID: "p1", Amount: 100}}}, {ID: "hidden", Requested: 900, OverStandard: true, TravelException: true, Allocations: []domain.Allocation{{ProjectID: "p2", Amount: 900}}}}}
	seedQueryClaim(t, db, c, "")
	for _, ref := range []store.ClaimReference{{ID: "r1", TenantID: "t", ClaimID: c.ID, ItemID: "visible", ProjectID: "p1"}, {ID: "r2", TenantID: "t", ClaimID: c.ID, ItemID: "hidden", ProjectID: "p2"}} {
		if e := db.Create(&ref).Error; e != nil {
			t.Fatal(e)
		}
	}
	task := domain.Task{ID: "task", ClaimID: c.ID, Round: 1, Assignee: "lead", Status: "PENDING", AllocationKeys: []string{"visible/p1/"}, RequiredPermission: "expense.approval.act"}
	raw, _ := json.Marshal(task)
	if e := db.Create(&store.Task{ID: task.ID, TenantID: "t", ClaimID: c.ID, Round: 1, Assignee: "lead", Status: "PENDING", NodeCode: "PROJECT_EXPENSE_OWNER", ContentJSON: string(raw)}).Error; e != nil {
		t.Fatal(e)
	}
	result := queryResultMap(t, s, a, "claims", map[string]string{"project_id": "p2"})
	if result["total"] != float64(0) {
		t.Fatalf("隐藏项目不可被筛选探知: %v", result)
	}
	result = queryResultMap(t, s, a, "claims", map[string]string{"project_id": "p1"})
	if result["total"] != float64(1) {
		t.Fatalf("本人分摊应可检索: %v", result)
	}
	view := result["items"].([]any)[0].(map[string]any)
	if view["payable_minor"] != "100" || view["has_over_standard"] != false || view["has_exception"] != false {
		t.Fatalf("摘要不得泄露隐藏分摊: %v", view)
	}
}

// TestQueryAbnormalInvoiceFilterRejectsInvalidBoolean 验证异常票池真实筛选且拒绝模糊布尔值。
// 参数：t 为测试上下文。
// 返回值：无。
func TestQueryAbnormalInvoiceFilterRejectsInvalidBoolean(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read"}}
	for i, status := range []string{"PENDING", "CONFIRMED"} {
		raw, _ := json.Marshal(domain.Invoice{ID: fmt.Sprint(i), TenantID: "t", Owner: "app", ReviewStatus: status, VerificationStatus: "NOT_CHECKED"})
		if e := db.Create(&store.Invoice{ID: fmt.Sprint(i), TenantID: "t", Owner: "app", ContentJSON: string(raw)}).Error; e != nil {
			t.Fatal(e)
		}
	}
	raw, _ := json.Marshal(domain.Invoice{ID: "risk", TenantID: "t", Owner: "app", ReviewStatus: "CONFIRMED", VerificationStatus: "MANUAL_REVIEWED"})
	if e := db.Create(&store.Invoice{ID: "risk", TenantID: "t", Owner: "app", ContentJSON: string(raw)}).Error; e != nil {
		t.Fatal(e)
	}
	if e := db.Create(&store.InvoiceRisk{ID: "risk-case", TenantID: "t", InvoiceID: "risk", Status: "OPEN", ContentJSON: "{}"}).Error; e != nil {
		t.Fatal(e)
	}
	result := queryResultMap(t, s, a, "invoices", map[string]string{"exception_only": "true"})
	if result["total"] != float64(2) {
		t.Fatalf("异常池应含待复核票及未结风险票: %v", result)
	}
	_, e := s.Query(context.Background(), a, "invoices", "", map[string]string{"exception_only": "yes"})
	problem, ok := e.(*domain.Problem)
	if !ok || problem.Status != 422 {
		t.Fatalf("非法布尔条件应422: %v", e)
	}
}

// TestQueryDashboardPendingTasksAreCurrentAndBounded 验证待办只包含当前轮次且最多五条。
// 参数：t 为测试上下文。
// 返回值：无。
func TestQueryDashboardPendingTasksAreCurrentAndBounded(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	for i := 0; i < 8; i++ {
		id := fmt.Sprint(i)
		c := domain.Claim{ID: "c" + id, TenantID: "t", Applicant: "app", Status: "IN_APPROVAL", CurrentRound: 1}
		seedQueryClaim(t, db, c, "")
		raw, _ := json.Marshal(domain.Task{ID: "task" + id, ClaimID: c.ID, Round: 1, RequiredPermission: "expense.approval.act", Status: "PENDING", Assignee: "lead"})
		round := 1
		if i == 7 {
			round = 0
		}
		if e := db.Create(&store.Task{ID: "task" + id, TenantID: "t", ClaimID: c.ID, Round: round, Assignee: "lead", Status: "PENDING", ContentJSON: string(raw), CreatedAt: time.Date(2026, 11, 11, 0, i, 0, 0, time.UTC)}).Error; e != nil {
			t.Fatal(e)
		}
		snapshot, _ := json.Marshal(map[string]any{"claim": c})
		if e := db.Create(&store.Snapshot{ID: "s" + id, TenantID: "t", ClaimID: c.ID, Round: 1, ContentJSON: string(snapshot)}).Error; e != nil {
			t.Fatal(e)
		}
	}
	result := queryResultMap(t, s, a, "dashboard", nil)
	tasks := result["pending_tasks"].([]any)
	if result["pending_approvals"] != float64(7) || len(tasks) != 5 {
		t.Fatalf("真实当前轮次计数与Top5不符: %v", result)
	}
	if tasks[0].(map[string]any)["id"] != "task0" {
		t.Fatalf("应优先显示最早待办: %v", tasks)
	}
}

// TestQueryUnknownClaimTypeIsNotInvented 验证旧数据不会被猜测为任意报销类型。
// 参数：t 为测试上下文。
// 返回值：无。
func TestQueryUnknownClaimTypeIsNotInvented(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	seedQueryClaim(t, db, domain.Claim{ID: "legacy", TenantID: "t", Applicant: "app", Status: "DRAFT"}, "")
	result := queryResultMap(t, s, a, "claims", nil)
	if result["total"] != float64(1) {
		t.Fatalf("旧单据仍应可见: %v", result)
	}
	result = queryResultMap(t, s, a, "claims", map[string]string{"claim_type": "TRAVEL"})
	if result["total"] != float64(0) {
		t.Fatalf("旧类型不得推断为差旅: %v", result)
	}
}

// TestQueryClaimStatusSupportsSingleMultipleAndEmpty 验证单据列表状态单值、逗号多值、空值及组合条件的过滤与计数一致。
// 参数：t 为测试上下文。
// 返回值：无；仅使用临时 SQLite，不连接真实库。
func TestQueryClaimStatusSupportsSingleMultipleAndEmpty(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	applicant := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	finance := domain.Actor{TenantID: "t", SubjectID: "fin", Permissions: []string{"expense.claim.read", "expense.scope.tenant_all"}}
	created := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	rows := []struct{ id, tenant, owner, status, title string }{
		{"draft", "t", "app", "DRAFT", "报销草稿"},
		{"returned", "t", "app", "RETURNED", "报销退回"},
		{"approval", "t", "app", "IN_APPROVAL", "报销审批中"},
		{"approved", "t", "app", "APPROVED_PENDING_PAYMENT", "报销待付"},
		{"paid", "t", "app", "PAID", "报销已付"},
		{"void", "t", "app", "VOID", "报销作废"},
		{"mate-paid", "t", "mate", "PAID", "报销他人已付"},
		{"foreign-paid", "other", "app", "PAID", "报销跨租户已付"},
	}
	for _, row := range rows {
		seedQueryClaim(t, db, domain.Claim{ID: row.id, TenantID: row.tenant, Applicant: row.owner, OrgID: "org", ClaimNo: "BX-" + row.id, Title: row.title, Status: row.status, CreatedAt: created}, "TRAVEL")
	}
	cases := []struct {
		name   string
		actor  domain.Actor
		params map[string]string
		total  int
	}{
		{"空值不过滤", applicant, nil, 6},
		{"单状态", applicant, map[string]string{"status": "RETURNED"}, 1},
		{"多状态", applicant, map[string]string{"status": "RETURNED,PAID"}, 2},
		{"逗号后空格", applicant, map[string]string{"status": "RETURNED, PAID"}, 2},
		{"重复状态去重", applicant, map[string]string{"status": "PAID,PAID"}, 1},
		{"全部六态", applicant, map[string]string{"status": "DRAFT,IN_APPROVAL,RETURNED,APPROVED_PENDING_PAYMENT,PAID,VOID"}, 6},
		{"与关键词类型日期组合", applicant, map[string]string{"status": "PAID,VOID", "q": "报销", "claim_type": "TRAVEL", "date_from": "2026-11-10", "date_to": "2026-11-10"}, 2},
		{"与本人范围组合", applicant, map[string]string{"status": "PAID", "mine": "true"}, 1},
		{"财务范围单状态", finance, map[string]string{"status": "PAID"}, 2},
		{"财务范围与申请人组合", finance, map[string]string{"status": "PAID", "applicant_subject_id": "mate"}, 1},
		{"财务范围与本人组合无单据", finance, map[string]string{"status": "PAID", "mine": "true"}, 0},
	}
	for _, tc := range cases {
		result := queryResultMap(t, s, tc.actor, "claims", tc.params)
		items := result["items"].([]any)
		if result["total"] != float64(tc.total) || len(items) != tc.total {
			t.Fatalf("%s total=%v items=%d 期望 %d: %v", tc.name, result["total"], len(items), tc.total, result)
		}
	}
	first := queryResultMap(t, s, applicant, "claims", map[string]string{"status": "RETURNED,PAID", "page": "1", "page_size": "1"})
	second := queryResultMap(t, s, applicant, "claims", map[string]string{"status": "RETURNED,PAID", "page": "2", "page_size": "1"})
	firstItems, secondItems := first["items"].([]any), second["items"].([]any)
	if first["total"] != float64(2) || len(firstItems) != 1 || second["total"] != float64(2) || len(secondItems) != 1 {
		t.Fatalf("分页total须保持全部匹配数: %v %v", first, second)
	}
	if firstItems[0].(map[string]any)["id"] == secondItems[0].(map[string]any)["id"] {
		t.Fatalf("状态过滤分页不得重复同一单据: %v %v", firstItems, secondItems)
	}
}

// TestQueryClaimStatusRejectsInvalidValues 验证非法状态、空段与大小写差异返回422，任务和票据枚举语义不变。
// 参数：t 为测试上下文。
// 返回值：无；仅使用临时 SQLite，不连接真实库。
func TestQueryClaimStatusRejectsInvalidValues(t *testing.T) {
	s, _, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read", "expense.approval.act", "expense.invoice.read"}}
	cases := []struct {
		kind   string
		params map[string]string
	}{
		{"claims", map[string]string{"status": "PENDING"}},
		{"claims", map[string]string{"status": "DRAFT,BOGUS"}},
		{"claims", map[string]string{"status": "DRAFT,,PAID"}},
		{"claims", map[string]string{"status": "DRAFT,"}},
		{"claims", map[string]string{"status": "draft"}},
		{"approval-tasks", map[string]string{"status": "PENDING,APPROVED"}},
		{"invoices", map[string]string{"status": "AVAILABLE,RESERVED"}},
	}
	for _, tc := range cases {
		_, err := s.Query(context.Background(), a, tc.kind, "", tc.params)
		public, ok := err.(*domain.Problem)
		if !ok || public.Status != 422 || public.Code != "INVALID_QUERY_FILTER" {
			t.Errorf("%s %v 应返回422 INVALID_QUERY_FILTER，实际 %v", tc.kind, tc.params, err)
		}
	}
}
