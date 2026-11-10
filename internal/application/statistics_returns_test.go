package application

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// seedStatisticsSubmission 保存真实提交所产生的不可变轮次事实。
// 参数：t为测试上下文，db为独立临时库，c为单据，round为提交轮次。
// 返回值：无。
func seedStatisticsSubmission(t *testing.T, db *gorm.DB, c domain.Claim, round int) {
	t.Helper()
	row := store.Snapshot{ID: fmt.Sprintf("snapshot-%s-%d", c.ID, round), TenantID: c.TenantID, ClaimID: c.ID, Round: round, ContentJSON: "{}"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

// seedStatisticsAction 保存真实审批动作事实，不从单据当前状态构造退回结论。
// 参数：t为测试上下文，db为独立临时库，c为单据，id/action/task为动作标识、类型和任务。
// 返回值：无。
func seedStatisticsAction(t *testing.T, db *gorm.DB, c domain.Claim, id, action, task string) {
	t.Helper()
	row := store.Action{ID: id, TenantID: c.TenantID, ClaimID: c.ID, Round: 1, Action: action, TaskID: task}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

// assertStatisticsReturns 校验服务响应的去重分子分母和整数基点。
// 参数：t为测试上下文，result为实际响应，submitted/returned为期望计数，rate为期望基点或nil。
// 返回值：无。
func assertStatisticsReturns(t *testing.T, result map[string]any, submitted, returned int, rate any) {
	t.Helper()
	if result["submitted_count"] != float64(submitted) || result["returned_count"] != float64(returned) || result["return_rate_basis_points"] != rate || result["return_rate_basis"] != "DISTINCT_SUBMITTED_CLAIMS_WITH_RETURN_ACTION" {
		t.Fatalf("真实提交退回口径错误: submitted=%v returned=%v rate=%v basis=%v", result["submitted_count"], result["returned_count"], result["return_rate_basis_points"], result["return_rate_basis"])
	}
}

// TestStatisticsReturnsUseSubmissionAndActualActions 验证草稿、历史退回、多轮去重及撤回作废边界。
// 参数：t为测试上下文。
// 返回值：无。
func TestStatisticsReturnsUseSubmissionAndActualActions(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	for _, test := range []struct {
		id, status string
		submitted  bool
		actions    []string
	}{
		{"draft", "DRAFT", false, nil}, {"current-returned-only", "RETURNED", true, nil},
		{"submitted", "IN_APPROVAL", true, nil}, {"resubmitted", "IN_APPROVAL", true, []string{"RETURN", "RETURN"}},
		{"returned", "RETURNED", true, []string{"RETURN"}}, {"withdrawn", "DRAFT", true, []string{"WITHDRAW"}},
		{"voided", "VOIDED", true, []string{"VOID"}}, {"unsubmitted-return", "DRAFT", false, []string{"RETURN"}},
	} {
		c := domain.Claim{
			ID: test.id, TenantID: "t", Applicant: "app", Status: test.status,
			CreatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC),
		}
		seedQueryClaim(t, db, c, "")
		if test.submitted {
			seedStatisticsSubmission(t, db, c, 1)
		}
		if test.id == "resubmitted" {
			seedStatisticsSubmission(t, db, c, 2)
		}
		for i, action := range test.actions {
			seedStatisticsAction(t, db, c, fmt.Sprintf("%s-%d", c.ID, i), action, "")
		}
	}
	result := statisticsResult(t, s, a, map[string]string{"date_from": "2026-11-11", "date_to": "2026-11-11"})
	assertStatisticsReturns(t, result, 6, 2, float64(3333))
}

// TestStatisticsReturnsZeroAndBeijingTenantBounds 验证零分母显式空值及北京月份、租户和本人范围。
// 参数：t为测试上下文。
// 返回值：无。
func TestStatisticsReturnsZeroAndBeijingTenantBounds(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	params := map[string]string{"date_from": "2026-11-01", "date_to": "2026-11-01"}
	result := statisticsResult(t, s, a, params)
	assertStatisticsReturns(t, result, 0, 0, nil)
	if _, ok := result["return_rate_basis_points"]; !ok {
		t.Fatal("零分母必须显式返回null")
	}
	for _, test := range []struct {
		id, tenant, owner string
		at                time.Time
	}{
		{"beijing", "t", "app", time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)},
		{"before", "t", "app", time.Date(2026, 10, 31, 15, 59, 59, 0, time.UTC)},
		{"foreign", "other", "app", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{"other-owner", "t", "other", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	} {
		c := domain.Claim{ID: test.id, TenantID: test.tenant, Applicant: test.owner, Status: "IN_APPROVAL", CreatedAt: test.at}
		seedQueryClaim(t, db, c, "")
		seedStatisticsSubmission(t, db, c, 1)
		seedStatisticsAction(t, db, c, "return-"+c.ID, "RETURN", "")
	}
	assertStatisticsReturns(t, statisticsResult(t, s, a, params), 1, 1, float64(10000))
}

// TestStatisticsReturnsDoNotExposeHiddenTaskActions 验证局部审批者仅统计本人可读历史任务的真实退回。
// 参数：t为测试上下文。
// 返回值：无。
func TestStatisticsReturnsDoNotExposeHiddenTaskActions(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	for _, id := range []string{"hidden-return", "visible-return", "wrong-tenant", "wrong-round", "unbound"} {
		c := domain.Claim{
			ID: id, TenantID: "t", Applicant: "app", CurrentRound: 2, Status: "IN_APPROVAL",
			CreatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC),
			Items: []domain.Item{{ID: "i", Allocations: []domain.Allocation{
				{ProjectID: "visible", Amount: 10}, {ProjectID: "hidden", Amount: 90},
			}}},
		}
		seedQueryClaim(t, db, c, "")
		seedStatisticsSubmission(t, db, c, 1)
		for _, owner := range []string{"lead", "other"} {
			key := "i/visible/"
			if owner == "other" {
				key = "i/hidden/"
			}
			task := domain.Task{
				ID: id + owner, ClaimID: id, Round: 1, Assignee: owner,
				AllocationKeys: []string{key},
			}
			raw, _ := json.Marshal(task)
			if err := db.Create(&store.Task{ID: task.ID, TenantID: "t", ClaimID: id, Round: 1, Assignee: owner, Status: "RETURNED", ContentJSON: string(raw)}).Error; err != nil {
				t.Fatal(err)
			}
		}
		task := id + "lead"
		if id == "hidden-return" {
			task = id + "other"
		}
		if id == "unbound" {
			task = ""
		}
		seedStatisticsAction(t, db, c, "return-"+id, "RETURN", task)
		if id == "wrong-tenant" {
			if err := db.Model(&store.Action{}).Where("id = ?", "return-"+id).Update("tenant_id", "foreign").Error; err != nil {
				t.Fatal(err)
			}
		}
		if id == "wrong-round" {
			if err := db.Model(&store.Action{}).Where("id = ?", "return-"+id).Update("round", 2).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	assertStatisticsReturns(t, statisticsResult(t, s, a, map[string]string{"date_from": "2026-11-11", "date_to": "2026-11-11"}), 5, 1, float64(2000))
}

// TestStatisticsReturnsReadAllBatches 验证105单跨批查询仍去重并按整数向下取整基点。
// 参数：t为测试上下文。
// 返回值：无。
func TestStatisticsReturnsReadAllBatches(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	for i := 0; i < 105; i++ {
		c := domain.Claim{
			ID: fmt.Sprintf("c%03d", i), TenantID: "t", Applicant: "app", Status: "IN_APPROVAL",
			CreatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC),
		}
		seedQueryClaim(t, db, c, "")
		seedStatisticsSubmission(t, db, c, 1)
		if i == 104 {
			seedStatisticsAction(t, db, c, "return-last", "RETURN", "")
		}
	}
	assertStatisticsReturns(t, statisticsResult(t, s, a, map[string]string{"date_from": "2026-11-11", "date_to": "2026-11-11", "page_size": "1"}), 105, 1, float64(95))
}

// TestStatisticsReturnsSubmittedWithoutReturn 验证真实提交而未退回时返回零基点。
// 参数：t为测试上下文。
// 返回值：无。
func TestStatisticsReturnsSubmittedWithoutReturn(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	c := domain.Claim{
		ID: "submitted-only", TenantID: "t", Applicant: "app", Status: "IN_APPROVAL",
		CreatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC),
	}
	seedQueryClaim(t, db, c, "")
	seedStatisticsSubmission(t, db, c, 1)
	params := map[string]string{"date_from": "2026-11-11", "date_to": "2026-11-11"}
	assertStatisticsReturns(t, statisticsResult(t, s, a, params), 1, 0, float64(0))
}
