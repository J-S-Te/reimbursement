package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// seedFinishTask 建立含真实任务时间及冻结单据的独立查询夹具。
// 参数：t为测试上下文，db为临时库，id/route/status为任务属性，created/updated为持久时间。
// 返回值：无，持久化失败时终止测试。
func seedFinishTask(t *testing.T, db *gorm.DB, id, route, status string, created, updated time.Time) {
	t.Helper()
	c := domain.Claim{ID: "claim-" + id, TenantID: "t", Applicant: "app", RouteType: route,
		Status: "IN_APPROVAL", CurrentRound: 1}
	seedQueryClaim(t, db, c, "TRAVEL")
	task := domain.Task{ID: id, ClaimID: c.ID, Round: 1, Node: 1, NodeCode: "ACCOUNTANT",
		Assignee: "lead", Status: status, RequiredPermission: "expense.approval.act"}
	raw, _ := json.Marshal(task)
	row := store.Task{ID: id, TenantID: "t", ClaimID: c.ID, Round: 1, Node: 1,
		Assignee: "lead", Status: status, ContentJSON: string(raw), CreatedAt: created, UpdatedAt: updated}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(map[string]any{"claim": c})
	if err := db.Create(&store.Snapshot{ID: "snapshot-" + id, TenantID: "t", ClaimID: c.ID,
		Round: 1, ContentJSON: string(snapshot)}).Error; err != nil {
		t.Fatal(err)
	}
}

// TestQueryApprovalRouteAndUrgency 验证线路筛选与真实最近激活时间的48小时展示口径。
// 参数：t为测试上下文。
// 返回值：无。
func TestQueryApprovalRouteAndUrgency(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	now := time.Now().UTC()
	seedFinishTask(t, db, "old", "TECHNICAL", "PENDING", now.Add(-96*time.Hour), now.Add(-49*time.Hour))
	seedFinishTask(t, db, "recent", "SALES", "PENDING", now.Add(-96*time.Hour), now.Add(-time.Hour))
	seedFinishTask(t, db, "waiting", "TECHNICAL", "WAITING", now.Add(-96*time.Hour), now.Add(-96*time.Hour))
	seedFinishTask(t, db, "done", "TECHNICAL", "APPROVED", now.Add(-96*time.Hour), now.Add(-96*time.Hour))
	seedFinishTask(t, db, "future", "SALES", "PENDING", now.Add(time.Hour), now.Add(time.Hour))
	result := queryResultMap(t, s, a, "approval-tasks", map[string]string{"route_type": "TECHNICAL"})
	if result["total"] != float64(3) {
		t.Fatalf("技术线路只应含三条: %v", result)
	}
	for _, raw := range result["items"].([]any) {
		item := raw.(map[string]any)
		if item["route_type"] != "TECHNICAL" {
			t.Fatalf("缺少可信线路: %v", item)
		}
		if item["id"] == "old" {
			if item["urgency"] != "OVERDUE" || item["pending_since"] == nil || item["urgency_basis"] != "TASK_LAST_UPDATED" {
				t.Fatalf("旧待办应按真实更新超期: %v", item)
			}
		} else if item["urgency"] != "NORMAL" || item["pending_since"] != nil {
			t.Fatalf("非待办不得显示假超期: %v", item)
		}
	}
	result = queryResultMap(t, s, a, "approval-tasks", map[string]string{"route_type": "SALES"})
	if result["total"] != float64(2) {
		t.Fatalf("销售线路应两条: %v", result)
	}
	for _, raw := range result["items"].([]any) {
		if raw.(map[string]any)["urgency"] != "NORMAL" {
			t.Fatalf("新变更及未来时间不能超期: %v", raw)
		}
	}
	_, err := s.Query(context.Background(), a, "approval-tasks", "", map[string]string{"route_type": "UNKNOWN"})
	if p, ok := err.(*domain.Problem); !ok || p.Status != 422 {
		t.Fatalf("非法线路须422: %v", err)
	}
}

// TestQueryWaitingActivationResetsUrgency 验证真实节点推进使用GORM更新时间，防止把等待期算入超期。
// 参数：t为测试上下文。
// 返回值：无。
func TestQueryWaitingActivationResetsUrgency(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	old := time.Now().UTC().Add(-96 * time.Hour)
	seedFinishTask(t, db, "activate", "TECHNICAL", "WAITING", old, old)
	c := domain.Claim{ID: "claim-activate", TenantID: "t", CurrentRound: 1}
	if err := advanceNode(db, &c, 0); err != nil {
		t.Fatal(err)
	}
	var row store.Task
	if err := db.First(&row, "id = ?", "activate").Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "PENDING" || !row.UpdatedAt.After(old) {
		t.Fatalf("激活须持久化新时间: %+v", row)
	}
	result := queryResultMap(t, s, a, "approval-tasks", nil)
	item := result["items"].([]any)[0].(map[string]any)
	if item["urgency"] != "NORMAL" {
		t.Fatalf("刚激活不能按旧创建时间超期: %v", item)
	}
}

// TestQueryUrgencyBoundaryAndHistoricalTask 验证48小时临界、历史轮次和未知时间不会产生假超期。
// 参数：t为测试上下文。
// 返回值：无。
func TestQueryUrgencyBoundaryAndHistoricalTask(t *testing.T) {
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	c := domain.Claim{Status: "IN_APPROVAL", CurrentRound: 2}
	for _, tc := range []struct {
		name    string
		row     store.Task
		want    string
		pending bool
	}{
		{"boundary", store.Task{Status: "PENDING", Round: 2, CreatedAt: now.Add(-48 * time.Hour)}, "OVERDUE", true},
		{"before", store.Task{Status: "PENDING", Round: 2, CreatedAt: now.Add(-48*time.Hour + time.Nanosecond)}, "NORMAL", true},
		{"historical", store.Task{Status: "PENDING", Round: 1, CreatedAt: now.Add(-96 * time.Hour)}, "NORMAL", false},
		{"unknown", store.Task{Status: "PENDING", Round: 2}, "NORMAL", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := map[string]any{}
			enrichTaskUrgency(item, tc.row, c, now)
			if item["urgency"] != tc.want || (item["pending_since"] != nil) != tc.pending {
				t.Fatalf("超期边界不符: %v", item)
			}
		})
	}
}
