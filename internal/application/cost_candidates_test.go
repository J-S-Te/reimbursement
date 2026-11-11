package application

import (
	"github.com/j-s-te/reimbursement/internal/domain"
	"testing"
	"time"
)

// TestFrozenCostItemsPreserveIdentity 验证费用项来源及预付不会被项目合并丢失。
// 参数：t为测试上下文。
// 返回值：无，冻结条目、发生日或分摊金额丢失时失败。
func TestFrozenCostItemsPreserveIdentity(t *testing.T) {
	c := domain.Claim{ID: "claim", TenantID: "tenant", Applicant: "person", OrgID: "org", Currency: "CNY", ClaimType: "TRAVEL", Items: []domain.Item{
		{ID: "item-1", ExpenseNature: "ACTUAL", FeeType: "TRAVEL", OccurredDate: "2026-11-01", Requested: 40, Allocations: []domain.Allocation{{ProjectID: "p", ServiceItemID: "s", Amount: 40}}},
		{ID: "item-2", ExpenseNature: "PREPAYMENT", FeeType: "TRAVEL", OccurredDate: "2026-11-02", Requested: 60, Allocations: []domain.Allocation{{ProjectID: "p", ServiceItemID: "s", Amount: 60}}},
	}}
	rows, err := frozenCostItems(c, "hash")
	if err != nil || len(rows) != 2 || rows[0].ItemID == rows[1].ItemID || rows[1].OccurredDate != "2026-11-02" || rows[0].AmountMinor != 40 {
		t.Fatalf("items=%+v err=%v", rows, err)
	}
	if rows[0].AllocationKey != rows[1].AllocationKey {
		t.Fatal("same project allocation must be stable; item identity is separate")
	}
	c.Items[0].Allocations[0].Amount = 41
	if _, err = frozenCostItems(c, "hash"); err == nil {
		t.Fatal("accepted nonconserving frozen allocation")
	}
	_ = time.Now()
}
