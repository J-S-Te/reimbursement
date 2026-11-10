package application

import (
	"encoding/json"
	"math/bits"
	"strings"

	"github.com/j-s-te/reimbursement/internal/domain"
)

const maxRouteThresholdBasisPoints int64 = 1000000

// decodeRouteRule 保留旧版本缺省字段但拒绝显式空值误关审批阈值。
// 参数：raw 为保存或发布的完整审批规则JSON。
// 返回值：结构化规则；类型、空值或阈值范围非法时返回422。
func decodeRouteRule(raw string) (domain.RouteRule, error) {
	var rule domain.RouteRule
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &rule); err != nil {
		return rule, domain.Fail("INVALID_CONFIGURATION", "审批阈值必须为合法整数，金额使用整数分字符串", 422)
	}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return rule, domain.Fail("INVALID_CONFIGURATION", "审批规则格式非法", 422)
	}
	for _, field := range []string{"extra_threshold_basis_points", "extra_threshold_minor"} {
		if strings.TrimSpace(string(fields[field])) == "null" {
			return rule, domain.Fail("INVALID_CONFIGURATION", "审批阈值不能显式设为空值", 422)
		}
	}
	return rule, validateRouteThresholds(rule)
}

// routeThresholdDecision 冻结逐项超额、严格OR规则及本轮实际采用的阈值版本。
type routeThresholdDecision struct {
	RuleID                    string               `json:"rule_id"`
	RuleVersion               int64                `json:"rule_version"`
	CalculationPolicy         string               `json:"calculation_policy"`
	ExtraThresholdBasisPoints int64                `json:"extra_threshold_basis_points"`
	ExtraThresholdMinor       domain.Money         `json:"extra_threshold_minor"`
	Triggered                 bool                 `json:"triggered"`
	Items                     []routeThresholdItem `json:"items"`
}

// routeThresholdItem 保留超标原始事实与逐项升级原因，避免汇总金额改变计算粒度。
type routeThresholdItem struct {
	ItemID          string       `json:"item_id"`
	Requested       domain.Money `json:"requested_minor"`
	StandardLimit   domain.Money `json:"standard_limit_minor"`
	Excess          domain.Money `json:"excess_minor"`
	OverStandard    bool         `json:"over_standard"`
	TravelException bool         `json:"travel_exception"`
	Triggered       bool         `json:"triggered"`
	Reasons         []string     `json:"reasons"`
}

// validateRouteThresholds 校验审批升级阈值且保留全零的旧规则语义。
// 参数：rule 为保存或读取的结构化审批规则。
// 返回值：负数、超过百分比上限时返回422配置错误。
func validateRouteThresholds(rule domain.RouteRule) error {
	if rule.ExtraThresholdBasisPoints < 0 ||
		rule.ExtraThresholdBasisPoints > maxRouteThresholdBasisPoints || rule.ExtraThresholdMinor < 0 {
		return domain.Fail("INVALID_CONFIGURATION", "审批阈值必须非负，比例不能超过10000%", 422)
	}
	return nil
}

// evaluateRouteThresholds 按单项超额严格OR判定，差旅异常始终升级。
// 参数：claim 为已重算标准的单据，rule 为当前已发布审批规则。
// 返回值：不修改费用事实的本轮冻结升级结论。
func evaluateRouteThresholds(claim domain.Claim, rule domain.RouteRule) routeThresholdDecision {
	decision := routeThresholdDecision{
		RuleID: rule.ID, RuleVersion: rule.Version, CalculationPolicy: "PER_ITEM_EXCESS_OR_STRICT_V1",
		ExtraThresholdBasisPoints: rule.ExtraThresholdBasisPoints,
		ExtraThresholdMinor:       rule.ExtraThresholdMinor, Items: make([]routeThresholdItem, 0, len(claim.Items)),
	}
	for _, item := range claim.Items {
		result := evaluateRouteThresholdItem(item, rule)
		decision.Items = append(decision.Items, result)
		decision.Triggered = decision.Triggered || result.Triggered
	}
	return decision
}

// evaluateRouteThresholdItem 保留每个正数阈值的独立命中原因与严格边界。
// 参数：item 为服务端已计算超标事实的费用项，rule 为当前升级阈值。
// 返回值：单项判定，全零仅对已有超标事实启用兼容升级。
func evaluateRouteThresholdItem(item domain.Item, rule domain.RouteRule) routeThresholdItem {
	result := routeThresholdItem{
		ItemID: item.ID, Requested: item.Requested, StandardLimit: item.StandardLimit,
		OverStandard: item.OverStandard, TravelException: item.TravelException, Reasons: []string{},
	}
	if item.StandardLimit >= 0 && item.Requested > item.StandardLimit {
		result.Excess = item.Requested - item.StandardLimit
	}
	if item.TravelException {
		result.Reasons = append(result.Reasons, "TRAVEL_EXCEPTION")
	}
	if item.OverStandard {
		if rule.ExtraThresholdBasisPoints == 0 && rule.ExtraThresholdMinor == 0 {
			result.Reasons = append(result.Reasons, "LEGACY_OVER_STANDARD")
		}
		if rule.ExtraThresholdMinor > 0 && result.Excess > rule.ExtraThresholdMinor {
			result.Reasons = append(result.Reasons, "EXCESS_AMOUNT")
		}
		if rule.ExtraThresholdBasisPoints > 0 && item.StandardLimit > 0 &&
			exceedsRoutePercentage(result.Excess, item.StandardLimit, rule.ExtraThresholdBasisPoints) {
			result.Reasons = append(result.Reasons, "EXCESS_PERCENT")
		}
	}
	result.Triggered = len(result.Reasons) > 0
	if !result.Triggered {
		reason := "WITHIN_STANDARD"
		if item.OverStandard {
			reason = "THRESHOLDS_NOT_EXCEEDED"
		}
		result.Reasons = append(result.Reasons, reason)
	}
	return result
}

// exceedsRoutePercentage 通过128位乘积严格比较超额比例，避免整数乘法溢出。
// 参数：excess 为单项超额分，limit 为该项正标准金额，basis 为启用的正基点。
// 返回值：超额乘10000严格大于标准乘基点时为真。
func exceedsRoutePercentage(excess, limit domain.Money, basis int64) bool {
	leftHigh, leftLow := bits.Mul64(uint64(excess), 10000)
	rightHigh, rightLow := bits.Mul64(uint64(limit), uint64(basis))
	return leftHigh > rightHigh || (leftHigh == rightHigh && leftLow > rightLow)
}
