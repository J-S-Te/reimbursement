package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"sort"
	"strings"
	"time"
)

// referenceShape 验证精确项目、服务项、费用日集合和有效窗口，不把终止资格直接放行。
// 参数：c 为当前草稿，refs 为后端从项目权威源获取的有序结果。
// 返回值：按费用引用键索引的结果或不完整、过期错误。
func referenceShape(c domain.Claim, refs []domain.ProjectReference) (map[string]domain.ProjectReference, error) {
	needed := claimReferences(c)
	if len(refs) != len(needed) {
		return nil, domain.Fail("PROJECT_INELIGIBLE", "项目复核结果不完整", 422)
	}
	byKey := map[string]domain.ProjectReference{}
	for i, ref := range refs {
		request := needed[i]
		if ref.ProjectID != request.ProjectID || ref.ServiceItemID != request.ServiceItemID || !ref.ValidUntil.After(time.Now().UTC()) || ref.ValidatedAt.After(time.Now().Add(time.Minute)) {
			return nil, domain.Fail("PROJECT_INELIGIBLE", "项目引用不匹配或已经过期", 422)
		}
		byKey[projectPolicyKey(request.ProjectID, request.ServiceItemID, request.OccurredDate)] = ref
	}
	return byKey, nil
}

// projectPolicyKey 固定项目、服务项与费用日边界，不能用另一日期的旅行事实替代。
// 参数：project/service/date 为实际费用关联。
// 返回值：无歧义引用集合键。
func projectPolicyKey(project, service, date string) string {
	return project + "\x00" + service + "\x00" + date
}

// terminationOnly 仅识别权威终止原因，任意其他拒绝原因都不能进入尾款例外。
// 参数：ref 为当前可信项目引用。
// 返回值：是否唯一终止障碍与稳定原因集合。
func terminationOnly(ref domain.ProjectReference) (bool, []string) {
	if !ref.ParticipantEligible || ref.ProjectVersion == 0 || ref.ReferenceVersion == "" || (ref.ServiceItemID != "" && ref.ServiceItemVersion == 0) || len(ref.ReasonCodes) == 0 {
		return false, nil
	}
	reasons := []string{}
	seen := map[string]bool{}
	for _, reason := range ref.ReasonCodes {
		if reason != "PROJECT_TERMINATED" && reason != "SERVICE_ITEM_TERMINATED" {
			return false, nil
		}
		if !seen[reason] {
			reasons = append(reasons, reason)
			seen[reason] = true
		}
	}
	sort.Strings(reasons)
	return true, reasons
}

// tailPolicy 按当前提交业务日匹配唯一启用、已发布的精确组织和项目政策。
// 参数：tx 为数据库，c 为归属单据，project 为权威项目，date 为费用发生日。
// 返回值：不可变政策版本或默认拒绝、冲突及截止日错误。
func tailPolicy(tx *gorm.DB, c domain.Claim, project, date string) (domain.HistoricalTailPolicy, error) {
	rows, e := configs(tx, c.TenantID, "historical_tail_policies", true)
	if e != nil {
		return domain.HistoricalTailPolicy{}, e
	}
	day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	var found *domain.HistoricalTailPolicy
	for _, row := range rows {
		var policy domain.HistoricalTailPolicy
		if e = json.Unmarshal([]byte(row.ContentJSON), &policy); e != nil {
			return policy, e
		}
		if !policy.Enabled || policy.OrgID != c.OrgID || policy.ProjectID != project || day < policy.ValidFrom || (policy.ValidUntil != "" && day > policy.ValidUntil) {
			continue
		}
		if found != nil {
			return policy, domain.Fail("TAIL_POLICY_CONFLICT", "同项目存在重叠有效尾款政策，请先受控修复", 422)
		}
		copy := policy
		found = &copy
	}
	if found == nil {
		return domain.HistoricalTailPolicy{}, domain.Fail("TAIL_POLICY_MISSING", "终止项目默认禁止新报销，请先发布精确组织项目的历史尾款政策", 422)
	}
	if date > found.ExpenseCutoff {
		return *found, domain.Fail("TAIL_CUTOFF_EXCEEDED", "费用发生日超过已批准历史尾款截止日期", 422)
	}
	return *found, nil
}

// applySubmissionProjectPolicies 重算终止尾款和结构化差旅事实，不接受前端放行结论。
// 参数：tx 为本地事务或预检数据库，c 为已重算标准的草稿，refs 为当前项目权威结果。
// 返回值：资格、政策或人工确认缺失错误。
func applySubmissionProjectPolicies(tx *gorm.DB, c *domain.Claim, refs []domain.ProjectReference) error {
	byKey, e := referenceShape(*c, refs)
	if e != nil {
		return e
	}
	for i := range c.Items {
		item := &c.Items[i]
		item.HistoricalTailApplications = nil
		item.TravelAssessment = nil
		if e = validateTravelDeclaration(item.TravelDeclaration); e != nil {
			return e
		}
		invoiceChecks, crossEnabled, err := loadInvoiceTravelChecks(tx, *c, *item)
		if err != nil {
			return err
		}
		item.RequiresTravelFacts = item.RequiresTravelFacts || crossEnabled
		assessments := []domain.TravelAssessmentReference{}
		for _, allocation := range item.Allocations {
			ref := byKey[projectPolicyKey(allocation.ProjectID, allocation.ServiceItemID, item.OccurredDate)]
			if !ref.Eligible {
				allowed, reasons := terminationOnly(ref)
				if !allowed {
					return domain.Fail("PROJECT_INELIGIBLE", "当前参与资格、项目服务项存在性或其他障碍不允许尾款例外", 422)
				}
				policy, err := tailPolicy(tx, *c, allocation.ProjectID, item.OccurredDate)
				if err != nil {
					return err
				}
				if strings.TrimSpace(item.HistoricalTailReason) == "" || len(item.HistoricalTailReason) > 2000 || len(item.HistoricalTailEvidenceFileIDs) == 0 {
					return domain.Fail("TAIL_EVIDENCE_REQUIRED", "历史尾款必须说明原因并提交真实发生的证据", 422)
				}
				item.HistoricalTailApplications = append(item.HistoricalTailApplications, domain.HistoricalTailApplication{ProjectID: allocation.ProjectID, ServiceItemID: allocation.ServiceItemID, ReferenceVersion: ref.ReferenceVersion, Policy: policy, TerminationReasons: reasons})
			}
			if item.RequiresTravelFacts {
				assessment, err := mappedTravelAssessment(tx, *c, *item, ref)
				if err != nil {
					return err
				}
				if crossEnabled {
					assessment = crossCheckTravelReference(*item, ref, assessment, invoiceChecks)
				}
				assessments = append(assessments, assessment)
			}
		}
		if item.RequiresTravelFacts {
			assessment := aggregateTravelAssessment(assessments)
			if e = confirmTravelAssessment(item, &assessment); e != nil {
				return e
			}
			item.TravelAssessment = &assessment
		}
	}
	return nil
}

// frozenTailApplication 查找已经被本轮明确批准的同项目服务项尾款政策。
// 参数：item 为不可变批准费用，ref 为当前引用。
// 返回值：匹配的冻结政策及存在标记。
func frozenTailApplication(item domain.Item, ref domain.ProjectReference) (domain.HistoricalTailApplication, bool) {
	for _, application := range item.HistoricalTailApplications {
		if application.ProjectID == ref.ProjectID && application.ServiceItemID == ref.ServiceItemID && application.Policy.Enabled && application.Policy.ID != "" && item.OccurredDate <= application.Policy.ExpenseCutoff {
			return application, true
		}
	}
	return domain.HistoricalTailApplication{}, false
}

// validatePaymentProjectPolicies 付款时只复核冻结批准，不从新发布政策静默追加例外。
// 参数：tx 为数据库，c 为当前批准单据，refs 为付款前实时权威结果。
// 返回值：当前无资格或批准后新终止、新旅行异常时要求重开。
func validatePaymentProjectPolicies(tx *gorm.DB, c domain.Claim, refs []domain.ProjectReference) error {
	byKey, e := referenceShape(c, refs)
	if e != nil {
		return e
	}
	var snapshot store.Snapshot
	if e = tx.Where("tenant_id = ? AND claim_id = ? AND round = ?", c.TenantID, c.ID, c.CurrentRound).First(&snapshot).Error; e != nil {
		return e
	}
	if e = VerifyApprovalSnapshot(json.RawMessage(snapshot.ContentJSON), c.ContentHash); e != nil {
		return e
	}
	var frozen approvalSnapshot
	if e = json.Unmarshal([]byte(snapshot.ContentJSON), &frozen); e != nil {
		return e
	}
	oldRefs, e := referenceShapeForFrozen(frozen.Claim, frozen.Approval.References)
	if e != nil {
		return e
	}
	for _, item := range c.Items {
		invoiceChecks, crossEnabled, err := loadInvoiceTravelChecks(tx, c, item)
		if err != nil {
			return err
		}
		newAssessments := []domain.TravelAssessmentReference{}
		for _, allocation := range item.Allocations {
			key := projectPolicyKey(allocation.ProjectID, allocation.ServiceItemID, item.OccurredDate)
			ref := byKey[key]
			if !ref.Eligible {
				allowed, reasons := terminationOnly(ref)
				if !allowed {
					return domain.Fail("PROJECT_INELIGIBLE", "付款前当前参与资格或权威关联不满足", 422)
				}
				application, approved := frozenTailApplication(item, ref)
				old, exists := oldRefs[key]
				oldAllowed, _ := terminationOnly(old)
				if !approved || !exists || old.Eligible || !oldAllowed || !sameTerminationSubset(reasons, application.TerminationReasons) {
					return domain.Fail("REOPEN_REQUIRED", "项目或服务项在批准后出现新的终止障碍，请重开并走新轮次尾款审批", 409)
				}
			}
			if item.RequiresTravelFacts {
				assessment, err := mappedTravelAssessment(tx, c, item, ref)
				if err != nil {
					return err
				}
				if crossEnabled {
					assessment = crossCheckTravelReference(item, ref, assessment, invoiceChecks)
				}
				old, exists := oldRefs[key]
				if !exists {
					return domain.Fail("APPROVAL_SNAPSHOT_MISMATCH", "批准行程引用缺失", 409)
				}
				previous, err := mappedTravelAssessment(tx, c, item, old)
				if err != nil {
					return err
				}
				if crossEnabled {
					previous = frozenTravelCrossReference(item, old, previous)
				}
				if newTravelIssue(assessment, previous) {
					return domain.Fail("REOPEN_REQUIRED", "该项目服务项出现新的行程矛盾或缺失，请重开重新确认", 409)
				}
				newAssessments = append(newAssessments, assessment)
			}
		}
		if item.RequiresTravelFacts {
			latest := aggregateTravelAssessment(newAssessments)
			if item.TravelAssessment == nil || (latest.Status == "MISMATCH" && item.TravelAssessment.Status != "MISMATCH") || (latest.Status == "UNKNOWN" && item.TravelAssessment.Status == "MATCH") {
				return domain.Fail("REOPEN_REQUIRED", "批准后的行程事实出现新的矛盾或缺失，请重开重新确认", 409)
			}
		}
	}
	return nil
}

// referenceShapeForFrozen 索引历史引用而不把过期验证窗口误当成历史批准失效。
// 参数：c 为旧批准快照，refs 为当时冻结权威引用。
// 返回值：历史关联集合，不发起外部请求。
func referenceShapeForFrozen(c domain.Claim, refs []domain.ProjectReference) (map[string]domain.ProjectReference, error) {
	requests := claimReferences(c)
	if len(requests) != len(refs) {
		return nil, domain.Fail("APPROVAL_SNAPSHOT_MISMATCH", "批准项目引用集合不完整", 409)
	}
	out := map[string]domain.ProjectReference{}
	for i, request := range requests {
		ref := refs[i]
		if ref.ProjectID != request.ProjectID || ref.ServiceItemID != request.ServiceItemID {
			return nil, domain.Fail("APPROVAL_SNAPSHOT_MISMATCH", "批准引用与费用日期集合不一致", 409)
		}
		out[projectPolicyKey(request.ProjectID, request.ServiceItemID, request.OccurredDate)] = ref
	}
	return out, nil
}

// sameTerminationSubset 限定付款时的终止障碍已经逐项经过本轮审批。
// 参数：current 为新原因，approved 为当时批准的原因。
// 返回值：是否没有新增终止事实。
func sameTerminationSubset(current, approved []string) bool {
	allowed := map[string]bool{}
	for _, reason := range approved {
		allowed[reason] = true
	}
	for _, reason := range current {
		if !allowed[reason] {
			return false
		}
	}
	return true
}
