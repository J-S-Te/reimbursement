package domain

import "sort"

const ratioBasisTotal int64 = 10000
const ratioAllocationRule = "largest-remainder-bp-v1"

type allocationRemainder struct {
	index     int
	amount    Money
	remainder int64
}

// NormalizeItemAllocations 按唯一金额或比例驱动规范化费用分摊。
// 参数：item 为申请人费用，空模式兼容历史金额分摊。
// 返回值：模式、比例、金额冲突或零分目标的422错误。
func NormalizeItemAllocations(item *Item) error {
	switch item.AllocationMode {
	case "", "AMOUNT":
		for _, a := range item.Allocations {
			if a.RatioBasisPoints != 0 {
				return Fail("ALLOCATION_DRIVER_CONFLICT", "金额分摊不能同时填写比例", 422)
			}
		}
		item.AllocationRuleVersion = ""
		return nil
	case "RATIO":
		if err := validateAllocationRatios(*item); err != nil {
			return err
		}
		return applyRatioAmounts(item)
	default:
		return Fail("INVALID_ALLOCATION_MODE", "分摊方式必须为金额或比例", 422)
	}
}

// validateAllocationRatios 校验比例分摊的目标和万分比总和。
// 参数：item 为具有正整数分费用金额的比例费用。
// 返回值：缺失、重复、越界或总和不等于100%的422错误。
func validateAllocationRatios(item Item) error {
	if item.Requested <= 0 || len(item.Allocations) == 0 || len(item.Allocations) > 1000 {
		return Fail("INVALID_ALLOCATION_RATIO", "比例分摊需要正费用金额及1至1000个目标", 422)
	}
	seen := map[[2]string]bool{}
	var total int64
	for _, a := range item.Allocations {
		key := [2]string{a.ProjectID, a.ServiceItemID}
		if a.ProjectID == "" || seen[key] || a.RatioBasisPoints <= 0 ||
			a.RatioBasisPoints > ratioBasisTotal-total {
			return Fail("INVALID_ALLOCATION_RATIO", "分摊目标不得重复，比例须为正且合计100%", 422)
		}
		seen[key] = true
		total += a.RatioBasisPoints
	}
	if total != ratioBasisTotal {
		return Fail("INVALID_ALLOCATION_RATIO", "分摊比例合计必须恰好为100%", 422)
	}
	return nil
}

// ratioRemainders 用安全商余计算最大余数法的整分基数和尾分顺序。
// 参数：item 为已通过比例总和和正金额校验的费用。
// 返回值：按余数及目标字典序排序的分摊结果。
func ratioRemainders(item Item) []allocationRemainder {
	parts := make([]allocationRemainder, len(item.Allocations))
	var allocated Money
	for i, a := range item.Allocations {
		// 先除后乘保证最大int64金额乘以比例也不溢出，余数乘积小于一亿。
		quotient := int64(item.Requested) / ratioBasisTotal
		product := int64(item.Requested) % ratioBasisTotal * a.RatioBasisPoints
		amount := Money(quotient*a.RatioBasisPoints + product/ratioBasisTotal)
		parts[i] = allocationRemainder{i, amount, product % ratioBasisTotal}
		allocated += amount
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].remainder != parts[j].remainder {
			return parts[i].remainder > parts[j].remainder
		}
		a, b := item.Allocations[parts[i].index], item.Allocations[parts[j].index]
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		return a.ServiceItemID < b.ServiceItemID
	})
	for i := Money(0); i < item.Requested-allocated; i++ {
		parts[int(i)].amount++
	}
	return parts
}

// applyRatioAmounts 检查客户端回传金额并一次性写入服务端派生结果。
// 参数：item 为通过比例校验的费用；非零回传金额必须与计算值一致。
// 返回值：双驱动冲突或小额比例产生零分目标的422错误。
func applyRatioAmounts(item *Item) error {
	parts := ratioRemainders(*item)
	for _, part := range parts {
		if part.amount <= 0 {
			return Fail("ALLOCATION_ZERO_AMOUNT", "费用金额过小导致分摊目标为零分，请调整目标或比例", 422)
		}
		clientAmount := item.Allocations[part.index].Amount
		if clientAmount != 0 && clientAmount != part.amount {
			return Fail("ALLOCATION_DRIVER_CONFLICT", "比例分摊金额由服务器计算，请清空金额或回传一致金额", 422)
		}
	}
	for _, part := range parts {
		item.Allocations[part.index].Amount = part.amount
	}
	item.AllocationRuleVersion = ratioAllocationRule
	return nil
}
