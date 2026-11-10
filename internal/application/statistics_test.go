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

// statisticsResult 读取真实统计服务响应并转换为断言字段。
// 参数：t 为测试上下文，s 为服务，a 为身份，params 为受控日期筛选。
// 返回值：响应字段，错误时终止测试。
func statisticsResult(t *testing.T, s *Service, a domain.Actor, params map[string]string) map[string]any {
	t.Helper()
	value, e := s.Statistics(context.Background(), a, params)
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

// seedStatisticsPayment 保存真实效果夹具，覆盖有效付款与历史全额冲正。
// 参数：t 为测试上下文，db 为临时库，c 为单据，reversed 为是否已全额冲正。
// 返回值：无。
func seedStatisticsPayment(t *testing.T, db *gorm.DB, c domain.Claim, reversed bool) {
	t.Helper()
	id := "pay-" + c.ID
	seedQueryClaim(t, db, c, "TRAVEL")
	payment, _ := json.Marshal(PaymentResult{ID: id, ClaimID: c.ID, Amount: c.Payable, Status: "RECORDED", PaidDate: "2026-11-11"})
	if e := db.Create(&store.Payment{ID: id, TenantID: c.TenantID, ClaimID: c.ID, ContentJSON: string(payment)}).Error; e != nil {
		t.Fatal(e)
	}
	effect := paymentEffect(c, id, "2026-11-11")
	if e := storeEffect(db, c, effect); e != nil {
		t.Fatal(e)
	}
	if reversed {
		effect.EffectID = "reverse-" + c.ID
		effect.Kind = "PAYMENT_FULLY_REVERSED"
		effect.AmountMinor = "-" + effect.AmountMinor
		effect.ReversesEffectID = id
		for i := range effect.Allocations {
			effect.Allocations[i].AmountMinor = "-" + effect.Allocations[i].AmountMinor
		}
		if e := storeEffect(db, c, effect); e != nil {
			t.Fatal(e)
		}
	} else if e := db.Model(&store.Claim{}).Where("id = ?", c.ID).Update("active_payment_id", id).Error; e != nil {
		t.Fatal(e)
	}
}

// TestStatisticsCountsCurrentPaidNetWithoutCrossScope 验证真实净额、冲正排除与对象范围。
// 参数：t 为测试上下文。
// 返回值：无。
func TestStatisticsCountsCurrentPaidNetWithoutCrossScope(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	at := time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id, tenant, app, status string
		reversed                bool
		amount                  domain.Money
	}{{"paid", "t", "app", "PAID", false, 100}, {"reversed", "t", "app", "APPROVED_PENDING_PAYMENT", true, 200}, {"foreign", "foreign", "app", "PAID", false, 900}, {"other-owner", "t", "other", "PAID", false, 800}} {
		c := domain.Claim{ID: row.id, TenantID: row.tenant, Applicant: row.app, Status: row.status, Payable: row.amount, CreatedAt: at, Items: []domain.Item{{ID: "i", Requested: row.amount, OverStandard: true, Allocations: []domain.Allocation{{ProjectID: "p1", Amount: row.amount}}}}}
		seedStatisticsPayment(t, db, c, row.reversed)
	}
	result := statisticsResult(t, s, a, map[string]string{"date_from": "2026-11-11", "date_to": "2026-11-11"})
	if result["total_count"] != float64(2) || result["over_standard_count"] != float64(2) {
		t.Fatalf("统计不得越出租户及本人范围: %v", result)
	}
	monthly := result["monthly"].([]any)
	if len(monthly) != 1 || monthly[0].(map[string]any)["amount_minor"] != "100" {
		t.Fatalf("冲正后不得累计原毛额: %v", result)
	}
	projects := result["by_project"].([]any)
	if len(projects) != 1 || projects[0].(map[string]any)["amount_minor"] != "100" {
		t.Fatalf("项目应只计当前有效净额: %v", result)
	}
}

// TestStatisticsEmptyAndDateBounds 验证空统计、月份边界与最大日期窗口。
// 参数：t 为测试上下文。
// 返回值：无。
func TestStatisticsEmptyAndDateBounds(t *testing.T) {
	s, _, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	result := statisticsResult(t, s, a, map[string]string{"date_from": "2026-10-31", "date_to": "2026-11-01"})
	months := result["monthly"].([]any)
	if result["total_count"] != float64(0) || len(months) != 2 || months[0].(map[string]any)["month"] != "2026-10" || months[1].(map[string]any)["month"] != "2026-11" {
		t.Fatalf("空窗口仍应返回完整月份: %v", result)
	}
	_, e := s.Statistics(context.Background(), a, map[string]string{"date_from": "2025-01-01", "date_to": "2026-11-11"})
	p, ok := e.(*domain.Problem)
	if !ok || p.Status != 422 {
		t.Fatalf("超过366日应422: %v", e)
	}
	_, e = s.Statistics(context.Background(), domain.Actor{TenantID: "t", SubjectID: "none"}, nil)
	p, ok = e.(*domain.Problem)
	if !ok || p.Status != 403 {
		t.Fatalf("缺能力不得统计: %v", e)
	}
}

// TestStatisticsScopesItemsSharingFinancialKey 验证同一财务分组中的隐藏费用条目不会泄露。
// 参数：t 为测试上下文。
// 返回值：无。
func TestStatisticsScopesItemsSharingFinancialKey(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	c := domain.Claim{ID: "scope", TenantID: "t", Applicant: "app", Status: "PAID", Payable: 1000, CurrentRound: 1, CreatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), Items: []domain.Item{{ID: "visible", FeeType: "TRAVEL", Requested: 100, Allocations: []domain.Allocation{{ProjectID: "p", Amount: 100}}}, {ID: "hidden", FeeType: "TRAVEL", Requested: 900, OverStandard: true, TravelException: true, Allocations: []domain.Allocation{{ProjectID: "p", Amount: 900}}}}}
	seedStatisticsPayment(t, db, c, false)
	raw, _ := json.Marshal(domain.Task{ID: "task", ClaimID: c.ID, Round: 1, Assignee: "lead", AllocationKeys: []string{"visible/p/"}})
	if e := db.Create(&store.Task{ID: "task", TenantID: "t", ClaimID: c.ID, Round: 1, Assignee: "lead", Status: "APPROVED", ContentJSON: string(raw)}).Error; e != nil {
		t.Fatal(e)
	}
	result := statisticsResult(t, s, a, map[string]string{"date_from": "2026-11-11", "date_to": "2026-11-11"})
	monthly := result["monthly"].([]any)
	if monthly[0].(map[string]any)["amount_minor"] != "100" || result["over_standard_count"] != float64(0) || result["exception_count"] != float64(0) {
		t.Fatalf("仅本人覆盖条目可以参与净额与风险统计: %v", result)
	}
}

// TestStatisticsReadsAllPages 验证统计不会仅取默认列表页或首个批次。
// 参数：t 为测试上下文。
// 返回值：无。
func TestStatisticsReadsAllPages(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	for i := 0; i < 105; i++ {
		seedQueryClaim(t, db, domain.Claim{ID: fmt.Sprintf("c%03d", i), TenantID: "t", Applicant: "app", Status: "DRAFT", CreatedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}, "")
	}
	result := statisticsResult(t, s, a, map[string]string{"date_from": "2026-11-11", "date_to": "2026-11-11", "page_size": "1"})
	if result["total_count"] != float64(105) {
		t.Fatalf("必须统计全部105单而非当前页: %v", result)
	}
}
