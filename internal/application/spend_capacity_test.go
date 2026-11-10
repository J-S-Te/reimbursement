package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// spendCapacityFixture 使用真实命令建立两种费用分摊的待付单。
// 参数：t 为隔离测试上下文。
// 返回值：服务、临时库、批准单据与独立出纳。
func spendCapacityFixture(t *testing.T) (*Service, *gorm.DB, domain.Claim, domain.Actor) {
	t.Helper()
	s, db, _ := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	c.Items[0].Requested = 50
	c.Items[0].Allocations[0].Amount = 50
	c.Items[0].InvoiceUsages[0].Used = 50
	second := c.Items[0]
	second.ID, second.FeeType = "item2", "OFFICE"
	c.Items = append(c.Items, second)
	raw, _ := json.Marshal(c)
	if err = db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	seedPolicy(t, db, "standards", domain.Standard{ID: "office", FeeType: "OFFICE", OrgID: "org", Grade: "G", Unit: "PER_OCCURRENCE", Limit: 200, ValidFrom: "2026-01-01"})
	seedPolicy(t, db, "scopes", domain.Scope{ID: "cashier", SubjectID: "cashier", OrgIDs: []string{"org"}})
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "capacity-submit", "expected_version": 1})
	var tasks []store.Task
	if err = db.Where("claim_id = ?", id).Order("node").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	version := int64(2)
	for _, task := range tasks {
		if err = db.First(&task, "id = ?", task.ID).Error; err != nil {
			t.Fatal(err)
		}
		actor := domain.Actor{TenantID: "t", SubjectID: task.Assignee, Permissions: []string{"expense.claim.read", "expense.approval.act", "expense.finance.review"}}
		runCommand(t, s, actor, "approval.action", task.ID, map[string]any{"command_id": "capacity-approve-" + task.ID, "expected_version": version, "task_version": task.Version, "round": 1, "action": "APPROVE"})
		version++
	}
	c, _, err = loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record"}}
	return s, db, c, cashier
}

// seedCapacityHistory 写入不可变旧付款及完整反向的有效闭合历史。
// 参数：t 为上下文，db 为隔离库，c 为单据，pairs 为历史付款冲正对数。
// 返回值：无。
func seedCapacityHistory(t *testing.T, db *gorm.DB, c domain.Claim, pairs int) {
	t.Helper()
	for i := 0; i < pairs; i++ {
		p := paymentEffect(c, fmt.Sprintf("history-%d", i), "2026-11-10")
		p.Allocations = p.Allocations[:1]
		p.Allocations[0].AmountMinor, p.AmountMinor = "100", "100"
		r := p
		r.EffectID, r.Kind, r.ReversesEffectID, r.AmountMinor = fmt.Sprintf("reverse-%d", i), "PAYMENT_FULLY_REVERSED", p.EffectID, "-100"
		r.Allocations = append([]domain.SpendAllocation{}, p.Allocations...)
		r.Allocations[0].AmountMinor = "-100"
		if err := storeEffect(db, c, p); err != nil {
			t.Fatal(err)
		}
		if err := storeEffect(db, c, r); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPaymentReservesExactReverseAllocationSpace 验证新付虽能容纳但无完整反向空间时提前拒绝。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestPaymentReservesExactReverseAllocationSpace(t *testing.T) {
	s, db, c, cashier := spendCapacityFixture(t)
	seedCapacityHistory(t, db, c, 999)
	raw, _ := json.Marshal(bindingPaymentBody("capacity-new-payment", c.Version, "capacity-transaction"))
	_, err := s.Execute(context.Background(), cashier, "payment.record", c.ID, raw)
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "SPEND_HISTORY_CAPACITY" {
		t.Fatalf("新付没有预留精确反向空间: %v", err)
	}
	var payments, bindings int64
	db.Model(&store.Payment{}).Count(&payments)
	db.Model(&store.PaymentBinding{}).Count(&bindings)
	if payments != 0 || bindings != 0 {
		t.Fatal("拒绝的新付仍占用流水或保留付款")
	}
}

// TestSpendCapacityWarnsBeforeLimit 验证接近容量时返回告警并仍保留完整反向空间。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSpendCapacityWarnsBeforeLimit(t *testing.T) {
	s, db, c, cashier := spendCapacityFixture(t)
	seedCapacityHistory(t, db, c, 800)
	result, err := s.SpendCapacity(context.Background(), cashier, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CanRecord || result.Warning != "SPEND_HISTORY_NEAR_CAPACITY" || result.AllocationsUsed != 1600 || result.AllocationReservation != 4 {
		t.Fatalf("容量预警不准确: %+v", result)
	}
}

// TestSpendCapacityRejectsMissingFinancialScope 验证容量读取不泄漏其他组织的单据历史。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSpendCapacityRejectsMissingFinancialScope(t *testing.T) {
	s, _, c, cashier := spendCapacityFixture(t)
	cashier.SubjectID = "outsider"
	_, err := s.SpendCapacity(context.Background(), cashier, c.ID)
	if err == nil {
		t.Fatal("无明确财务范围仍获取容量")
	}
	cashier.TenantID = "other"
	if _, err = s.SpendCapacity(context.Background(), cashier, c.ID); err == nil {
		t.Fatal("容量跨租户可读")
	}
}

// TestSpendCapacityDoesNotMislabelDraftAsExhausted 验证尚未批准的草稿不会收到容量耗尽误报。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestSpendCapacityDoesNotMislabelDraftAsExhausted(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	_, id := submissionFixture(t, db, true)
	seedPolicy(t, db, "scopes", domain.Scope{ID: "cashier", SubjectID: "cashier", OrgIDs: []string{"org"}})
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.payment.read"}}
	result, err := s.SpendCapacity(context.Background(), cashier, id)
	if err != nil || result.CanRecord || result.Warning != "" {
		t.Fatalf("草稿被误判容量耗尽: %+v %v", result, err)
	}
}
