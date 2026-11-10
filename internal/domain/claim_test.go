package domain

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestAllocationAndInvoiceCoverage 防止缺票覆盖或分摊尾差被静默放行。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAllocationAndInvoiceCoverage(t *testing.T) {
	c := Claim{Title: "差旅", RouteType: "TECHNICAL", Currency: "CNY", Items: []Item{{ID: "i", FeeType: "TRAVEL", OccurredDate: "2026-11-10", Requested: 101, Allocations: []Allocation{{ProjectID: "p", Amount: 100}}, InvoiceUsages: []InvoiceUsage{{InvoiceID: "v", Used: 101}}}}}
	if ValidateClaim(&c) == nil {
		t.Fatal("分摊少1分钱必须阻断")
	}
	c.Items[0].Allocations[0].Amount = 101
	c.Items[0].InvoiceUsages = nil
	if ValidateClaim(&c) == nil {
		t.Fatal("没有显式政策时不能隐式免票")
	}
	c.Items[0].InvoiceUsages = []InvoiceUsage{{InvoiceID: "v", Used: 101}}
	if err := ValidateClaim(&c); err != nil {
		t.Fatal(err)
	}
	if c.Payable != 101 {
		t.Fatalf("应付金额未服务端重算: %v", c.Payable)
	}
}

// TestMoneyRejectsJSONNumbers 防止 JavaScript 数字丢失精度。
// 参数：t 为测试上下文。
// 返回值：无。
func TestMoneyRejectsJSONNumbers(t *testing.T) {
	var m Money
	if json.Unmarshal([]byte(`100`), &m) == nil {
		t.Fatal("金额必须为字符串")
	}
	if err := json.Unmarshal([]byte(`"9223372036854775808"`), &m); err == nil {
		t.Fatal("不得溢出")
	}
}

// TestClaimResourceLimits 防止大草稿在提交时触发无界票据或项目批验。
// 参数：t 为测试上下文。
// 返回值：无。
func TestClaimResourceLimits(t *testing.T) {
	c := Claim{Title: "资源限制", RouteType: "TECHNICAL", Currency: "CNY"}
	for itemIndex := 0; itemIndex < 3; itemIndex++ {
		item := Item{ID: fmt.Sprintf("item-%d", itemIndex), FeeType: "TRAVEL", OccurredDate: "2026-11-10", Requested: 100}
		for i := 0; i < 100; i++ {
			item.InvoiceUsages = append(item.InvoiceUsages, InvoiceUsage{InvoiceID: fmt.Sprintf("invoice-%d-%d", itemIndex, i), Used: 1})
		}
		item.Allocations = []Allocation{{ProjectID: "p", Amount: 100}}
		c.Items = append(c.Items, item)
	}
	if ValidateClaim(&c) == nil {
		t.Fatal("超过200张不同票据未阻断")
	}
	c.Items = nil
	for itemIndex := 0; itemIndex < 2; itemIndex++ {
		item := Item{ID: fmt.Sprintf("item-%d", itemIndex), FeeType: "TRAVEL", OccurredDate: "2026-11-10", Requested: 501, NoInvoiceReason: "允许补贴"}
		for i := 0; i < 501; i++ {
			item.Allocations = append(item.Allocations, Allocation{ProjectID: fmt.Sprintf("project-%d-%d", itemIndex, i), Amount: 1})
		}
		c.Items = append(c.Items, item)
	}
	if ValidateClaim(&c) == nil {
		t.Fatal("超过1000分摊行未阻断")
	}
}
