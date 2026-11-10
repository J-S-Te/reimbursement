package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	"sort"
	"strings"
)

// assessTravelReference 只从权威结构化事实派生一致性，不猜城市、不生成出差审批。
// 参数：ref 为按具体费用日期批验的权威引用。
// 返回值：MATCH/MISMATCH/UNKNOWN及原因，畸形摘要返回依赖错误。
func assessTravelReference(ref domain.ProjectReference) (domain.TravelAssessmentReference, error) {
	out := domain.TravelAssessmentReference{ProjectID: ref.ProjectID, ServiceItemID: ref.ServiceItemID, ReferenceVersion: ref.ReferenceVersion, Status: "UNKNOWN", ReasonCodes: []string{"TRAVEL_FACTS_MISSING"}}
	facts := ref.TravelSnapshot
	if facts == nil {
		return out, nil
	}
	if len(facts.SourceChain) > 16 || len(facts.ReasonCodes) > 100 {
		return out, domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "项目行程事实超过协议边界", 503)
	}
	copy := *facts
	copy.ContentDigest = ""
	raw, e := json.Marshal(copy)
	if e != nil {
		return out, e
	}
	hash := sha256.Sum256(raw)
	if facts.ContentDigest == "" || hex.EncodeToString(hash[:]) != facts.ContentDigest {
		return out, domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "项目行程事实摘要不完整或不一致", 503)
	}
	out.FactDigest = facts.ContentDigest
	out.ReasonCodes = append([]string{}, facts.ReasonCodes...)
	out.ReasonCodes = append(out.ReasonCodes, travelIssueDimensions(facts)...)
	mismatch := facts.Mode == "NO_TRAVEL" || facts.SiteMatchStatus == "MISMATCH" || facts.DateMatchStatus == "MISMATCH" || facts.CityCodeStatus == "MISMATCH" || (facts.ActorInTravelers != nil && !*facts.ActorInTravelers)
	unknown := facts.Status != "KNOWN" || (facts.Mode != "NEW" && facts.Mode != "EXISTING") || facts.ActorInTravelers == nil || facts.SiteMatchStatus != "MATCH" || facts.DateMatchStatus != "MATCH" || facts.CityCodeStatus != "MATCH" || facts.Transport == "" || facts.AccommodationNeed == ""
	switch {
	case mismatch:
		out.Status = "MISMATCH"
		out.ReasonCodes = append(out.ReasonCodes, "TRAVEL_FACTS_MISMATCH")
	case unknown:
		out.Status = "UNKNOWN"
		out.ReasonCodes = append(out.ReasonCodes, "TRAVEL_FACTS_REQUIRE_MANUAL_CONFIRMATION")
	default:
		out.Status = "MATCH"
	}
	out.ReasonCodes = uniqueReasons(out.ReasonCodes)
	return out, nil
}

// aggregateTravelAssessment 合并所有费用分摊关联，矛盾优先于未知，未知不能被其他MATCH覆盖。
// 参数：references 为本费用涉及的所有项目服务项事实。
// 返回值：保留逐引用来源的合并结论。
func aggregateTravelAssessment(references []domain.TravelAssessmentReference) domain.TravelAssessment {
	out := domain.TravelAssessment{Status: "MATCH", References: references, ReasonCodes: []string{}}
	out.CrossCheckStatus = "NOT_REQUESTED_LEGACY"
	if len(references) == 0 {
		out.Status = "UNKNOWN"
	}
	for _, ref := range references {
		if ref.CrossCheck != nil {
			if out.CrossCheckStatus == "NOT_REQUESTED_LEGACY" {
				out.CrossCheckStatus = "MATCH"
			}
			out.CrossCheckStatus = combineTravelStatus(out.CrossCheckStatus, ref.CrossCheck.Status)
		}
		if ref.Status == "MISMATCH" {
			out.Status = "MISMATCH"
		} else if ref.Status != "MATCH" && out.Status != "MISMATCH" {
			out.Status = "UNKNOWN"
		}
		out.ReasonCodes = append(out.ReasonCodes, ref.ReasonCodes...)
	}
	out.ReasonCodes = uniqueReasons(out.ReasonCodes)
	return out
}

// uniqueReasons 对来源原因去重排序，保持快照和资格集合稳定。
// 参数：reasons 为结构化原因集合。
// 返回值：非空原因的稳定列表。
func uniqueReasons(reasons []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, reason := range reasons {
		if reason != "" && !seen[reason] {
			seen[reason] = true
			out = append(out, reason)
		}
	}
	sort.Strings(out)
	return out
}

// confirmTravelAssessment 要求未知或矛盾由申请人明确说明并提供证据，但保持真实状态。
// 参数：item 为费用，assessment 为服务端派生结果。
// 返回值：缺少相符人工确认时拒绝提交。
func confirmTravelAssessment(item *domain.Item, assessment *domain.TravelAssessment) error {
	if assessment.Status == "MATCH" {
		return nil
	}
	confirmation := item.TravelConfirmation
	expected := "CONFIRMED_UNKNOWN"
	if assessment.Status == "MISMATCH" {
		expected = "ACKNOWLEDGED_MISMATCH"
	}
	if confirmation == nil || confirmation.Decision != expected || strings.TrimSpace(confirmation.Reason) == "" || len(confirmation.Reason) > 2000 || len(confirmation.EvidenceFileIDs) == 0 {
		return domain.Fail("TRAVEL_CONFIRMATION_REQUIRED", "行程未知或矛盾需对应人工确认、理由和本人证据，不能改成MATCH", 422)
	}
	if assessment.Status == "UNKNOWN" {
		assessment.ConfirmedUnknown = true
	} else {
		item.TravelException = true
		if item.ExceptionReason == "" {
			item.ExceptionReason = confirmation.Reason
		}
	}
	return nil
}

// travelIssueDimensions 逐维度标识矛盾与缺失，防止原有异常掩盖另一新问题。
// 参数：facts为当前或历史权威行程事实。
// 返回值：稳定维度原因码，不凭自由文本推断城市或审批结果。
func travelIssueDimensions(facts *domain.TravelSnapshot) []string {
	checks := []struct {
		code    string
		present bool
	}{
		{"TRAVEL_MODE_CONFLICT", facts.Mode == "NO_TRAVEL"},
		{"TRAVEL_ACTOR_MISMATCH", facts.ActorInTravelers != nil && !*facts.ActorInTravelers},
		{"TRAVEL_SITE_MISMATCH", facts.SiteMatchStatus == "MISMATCH"},
		{"TRAVEL_DATE_MISMATCH", facts.DateMatchStatus == "MISMATCH"},
		{"TRAVEL_CITY_MISMATCH", facts.CityCodeStatus == "MISMATCH"},
		{"TRAVEL_STATUS_UNKNOWN", facts.Status != "KNOWN"},
		{"TRAVEL_MODE_UNKNOWN", facts.Mode != "NEW" && facts.Mode != "EXISTING" && facts.Mode != "NO_TRAVEL"},
		{"TRAVEL_ACTOR_UNKNOWN", facts.ActorInTravelers == nil},
		{"TRAVEL_SITE_UNKNOWN", facts.SiteMatchStatus != "MATCH" && facts.SiteMatchStatus != "MISMATCH"},
		{"TRAVEL_DATE_UNKNOWN", facts.DateMatchStatus != "MATCH" && facts.DateMatchStatus != "MISMATCH"},
		{"TRAVEL_CITY_UNKNOWN", facts.CityCodeStatus != "MATCH" && facts.CityCodeStatus != "MISMATCH"},
		{"TRAVEL_TRANSPORT_UNKNOWN", facts.Transport == ""},
		{"TRAVEL_ACCOMMODATION_UNKNOWN", facts.AccommodationNeed == ""},
	}
	reasons := []string{}
	for _, check := range checks {
		if check.present {
			reasons = append(reasons, check.code)
		}
	}
	return reasons
}

// newTravelIssue 比较同一项目服务项费用日的原批准原因，新增矛盾或缺失须重新确认。
// 参数：current为付款前事实；previous为本轮冻结权威事实的重算结果。
// 返回值：当前出现未批准异常维度时为真；事实改善为MATCH不触发重开。
func newTravelIssue(current, previous domain.TravelAssessmentReference) bool {
	if changedTravelInvoiceFacts(current.CrossCheck, previous.CrossCheck) {
		return true
	}
	if current.Status == "MATCH" {
		return false
	}
	if current.Status != previous.Status {
		return true
	}
	approved := map[string]bool{}
	for _, reason := range previous.ReasonCodes {
		approved[reason] = true
	}
	for _, reason := range current.ReasonCodes {
		if !approved[reason] {
			return true
		}
	}
	return false
}
