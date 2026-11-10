package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

// ratioClaim 通过公开 JSON 协议构造有票据覆盖的比例费用草稿。
// 参数：t 为测试上下文，item 为人工指定的费用 JSON。
// 返回值：可直接提交领域校验的草稿。
func ratioClaim(t *testing.T, item string) Claim {
	t.Helper()
	var c Claim
	raw := `{"title":"比例报销","route_type":"TECHNICAL","currency":"CNY","items":[` + item + `]}`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRatioAllocationsDistributeRemainders 防止按比例分摊丢失尾分或依赖请求行顺序。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRatioAllocationsDistributeRemainders(t *testing.T) {
	c := ratioClaim(t, `{"id":"i","fee_type":"TRAVEL","occurred_date":"2026-11-10",
		"requested_minor":"101","no_invoice_reason":"待政策核验","allocation_mode":"RATIO",
		"allocation_rule_version":"client-value","allocations":[
		{"project_id":"b","ratio_basis_points":5000},
		{"project_id":"a","ratio_basis_points":5000}]}`)
	if err := ValidateClaim(&c); err != nil {
		t.Fatalf("比例单驱动应派生金额: %v", err)
	}
	if c.Items[0].Allocations[0].Amount != 50 || c.Items[0].Allocations[1].Amount != 51 {
		t.Fatalf("尾分应归字典序较小目标: %+v", c.Items[0].Allocations)
	}
	raw, _ := json.Marshal(c.Items[0])
	var item map[string]any
	_ = json.Unmarshal(raw, &item)
	if item["allocation_rule_version"] != "largest-remainder-bp-v1" || c.Payable != 101 {
		t.Fatalf("规则版本与总额应由服务端固定: %s", raw)
	}
}

// TestRatioAllocationsUseTupleOrderAndAvoidOverflow 防止拼接排序碰撞及大金额乘法溢出。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRatioAllocationsUseTupleOrderAndAvoidOverflow(t *testing.T) {
	c := ratioClaim(t, `{"id":"i","fee_type":"TRAVEL","occurred_date":"2026-11-10",
		"requested_minor":"9223372036854775807","no_invoice_reason":"待政策核验",
		"allocation_mode":"RATIO","allocations":[
		{"project_id":"ab","service_item_id":"a","ratio_basis_points":5000},
		{"project_id":"a","service_item_id":"z","ratio_basis_points":5000}]}`)
	if err := ValidateClaim(&c); err != nil {
		t.Fatal(err)
	}
	if c.Items[0].Allocations[0].Amount != 4611686018427387903 ||
		c.Items[0].Allocations[1].Amount != 4611686018427387904 {
		t.Fatalf("整数分计算错误: %+v", c.Items[0].Allocations)
	}
}

// TestRatioAllocationsRejectInvalidDrivers 防止比例与金额双权威或零分目标进入账本。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRatioAllocationsRejectInvalidDrivers(t *testing.T) {
	cases := []struct{ name, mode, amount, rows string }{
		{"sum", "RATIO", "100", `{"project_id":"a","ratio_basis_points":9999}`},
		{"negative", "RATIO", "100", `{"project_id":"a","ratio_basis_points":-1},{"project_id":"b","ratio_basis_points":10001}`},
		{"duplicate", "RATIO", "100", `{"project_id":"a","ratio_basis_points":5000},{"project_id":"a","ratio_basis_points":5000}`},
		{"missing-project", "RATIO", "100", `{"ratio_basis_points":10000}`},
		{"different-amount", "RATIO", "100", `{"project_id":"a","amount_minor":"99","ratio_basis_points":10000}`},
		{"zero-rounded", "RATIO", "1", `{"project_id":"a","ratio_basis_points":5000},{"project_id":"b","ratio_basis_points":5000}`},
		{"amount-with-ratio", "AMOUNT", "100", `{"project_id":"a","amount_minor":"100","ratio_basis_points":10000}`},
		{"legacy-with-ratio", "", "100", `{"project_id":"a","amount_minor":"100","ratio_basis_points":10000}`},
		{"unknown-mode", "UNKNOWN", "100", `{"project_id":"a","amount_minor":"100"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ratioClaim(t, `{"id":"i","fee_type":"TRAVEL","occurred_date":"2026-11-10",
			"requested_minor":"`+tc.amount+`","no_invoice_reason":"待政策核验",
			"allocation_mode":"`+tc.mode+`","allocations":[`+tc.rows+`]}`)
			err := ValidateClaim(&c)
			var p *Problem
			if !errors.As(err, &p) || p.Status != 422 {
				t.Fatalf("非法分摊必须明确422: %v", err)
			}
		})
	}
}

// TestRatioAllocationsAcceptEqualDerivedAmount 防止回传服务器金额的合法草稿无法再次提交。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRatioAllocationsAcceptEqualDerivedAmount(t *testing.T) {
	c := ratioClaim(t, `{"id":"i","fee_type":"TRAVEL","occurred_date":"2026-11-10",
		"requested_minor":"100","no_invoice_reason":"待政策核验","allocation_mode":"RATIO",
		"allocations":[{"project_id":"a","amount_minor":"100","ratio_basis_points":10000}]}`)
	if err := ValidateClaim(&c); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c.Items[0])
	var item map[string]any
	_ = json.Unmarshal(raw, &item)
	if item["allocation_rule_version"] != "largest-remainder-bp-v1" {
		t.Fatalf("比例重算规则未冻结: %s", raw)
	}
}

// TestRatioAllocationsPreferRemainderBeforeTarget 防止按目标排序分尾分而忽略比例余数。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRatioAllocationsPreferRemainderBeforeTarget(t *testing.T) {
	c := ratioClaim(t, `{"id":"i","fee_type":"TRAVEL","occurred_date":"2026-11-10",
		"requested_minor":"101","no_invoice_reason":"待政策核验","allocation_mode":"RATIO",
		"allocations":[{"project_id":"b","ratio_basis_points":3333},
		{"project_id":"a","ratio_basis_points":3333},
		{"project_id":"c","ratio_basis_points":3334}]}`)
	if err := ValidateClaim(&c); err != nil {
		t.Fatal(err)
	}
	for i, want := range []Money{33, 34, 34} {
		if c.Items[0].Allocations[i].Amount != want {
			t.Fatalf("余数大的目标应优先获尾分: %+v", c.Items[0].Allocations)
		}
	}
}

// TestRatioAllocationsBreakTiesByServiceItem 防止同项目下服务项尾分依赖输入顺序。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRatioAllocationsBreakTiesByServiceItem(t *testing.T) {
	c := ratioClaim(t, `{"id":"i","fee_type":"TRAVEL","occurred_date":"2026-11-10",
		"requested_minor":"3","no_invoice_reason":"待政策核验","allocation_mode":"RATIO",
		"allocations":[{"project_id":"p","service_item_id":"b","ratio_basis_points":5000},
		{"project_id":"p","service_item_id":"a","ratio_basis_points":5000}]}`)
	if err := ValidateClaim(&c); err != nil {
		t.Fatal(err)
	}
	if c.Items[0].Allocations[0].Amount != 1 || c.Items[0].Allocations[1].Amount != 2 {
		t.Fatalf("同项目应按服务项字典序分尾分: %+v", c.Items[0].Allocations)
	}
}

// TestAllocationNormalizationLeavesFailureUntouched 防止冲突回传金额造成半更新。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAllocationNormalizationLeavesFailureUntouched(t *testing.T) {
	item := Item{Requested: 100, AllocationMode: "RATIO", AllocationRuleVersion: "original",
		Allocations: []Allocation{{ProjectID: "a", RatioBasisPoints: 5000},
			{ProjectID: "b", Amount: 51, RatioBasisPoints: 5000}}}
	if err := NormalizeItemAllocations(&item); err == nil {
		t.Fatal("冲突回传金额应拒绝")
	}
	if item.Allocations[0].Amount != 0 || item.Allocations[1].Amount != 51 ||
		item.AllocationRuleVersion != "original" {
		t.Fatalf("校验失败不应半更新费用分摊: %+v", item)
	}
}

// TestRatioAllocationsTreatTargetAsTuple 防止项目与服务项标识中的分隔符造成错误去重。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRatioAllocationsTreatTargetAsTuple(t *testing.T) {
	c := ratioClaim(t, `{"id":"i","fee_type":"TRAVEL","occurred_date":"2026-11-10",
		"requested_minor":"2","no_invoice_reason":"待政策核验","allocation_mode":"RATIO",
		"allocations":[{"project_id":"a/b","service_item_id":"c","ratio_basis_points":5000},
		{"project_id":"a","service_item_id":"b/c","ratio_basis_points":5000}]}`)
	if err := ValidateClaim(&c); err != nil {
		t.Fatalf("不同的项目服务项目标不应被拼接键合并: %v", err)
	}
}
