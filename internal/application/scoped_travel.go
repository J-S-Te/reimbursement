package application

import "github.com/j-s-te/reimbursement/internal/domain"

// scopedTailApplications 仅保留当前责任人选中分摊对应的尾款政策。
// 参数：values 为冻结政策，allocations 为当前可见分摊。
// 返回值：不含其他项目政策的列表。
func scopedTailApplications(values []domain.HistoricalTailApplication, allocations []domain.Allocation) []domain.HistoricalTailApplication {
	out := []domain.HistoricalTailApplication{}
	for _, value := range values {
		for _, allocation := range allocations {
			if value.ProjectID == allocation.ProjectID && value.ServiceItemID == allocation.ServiceItemID {
				out = append(out, value)
				break
			}
		}
	}
	return out
}

// keysCoverEvidence 校验责任集合覆盖同一个证据文件的所有费用引用。
// 参数：keys 为任务冻结责任，c 为单据，fileID 为证据。
// 返回值：是否完整覆盖。
func keysCoverEvidence(keys map[string]bool, c domain.Claim, fileID string) bool {
	used := false
	for _, item := range c.Items {
		if !itemHasEvidence(item, fileID) {
			continue
		}
		used = true
		for _, allocation := range item.Allocations {
			if !keys["*"] && !keys[item.ID+"/"+allocation.ProjectID+"/"+allocation.ServiceItemID] {
				return false
			}
		}
	}
	return used
}

// scopedEvidenceIDs 隐藏包含范围外费用的共享文件引用。
// 参数：ids 为当前项附件，c 为完整单据，keys 为当前责任范围。
// 返回值：允许签发票据的附件引用。
func scopedEvidenceIDs(ids []string, c domain.Claim, keys map[string]bool) []string {
	out := []string{}
	for _, id := range ids {
		if keysCoverEvidence(keys, c, id) {
			out = append(out, id)
		}
	}
	return out
}

// scopedTravelAssessment 重新组合当前可见分摊的事实，防止暴露其他项目原因。
// 参数：assessment 为冻结总体结论，allocations 为本主体可见分摊。
// 返回值：仅含责任范围事实的显示结论。
func scopedTravelAssessment(assessment domain.TravelAssessment, allocations []domain.Allocation) *domain.TravelAssessment {
	refs := []domain.TravelAssessmentReference{}
	for _, ref := range assessment.References {
		for _, allocation := range allocations {
			if ref.ProjectID == allocation.ProjectID && ref.ServiceItemID == allocation.ServiceItemID {
				refs = append(refs, ref)
				break
			}
		}
	}
	out := aggregateTravelAssessment(refs)
	out.ConfirmedUnknown = assessment.ConfirmedUnknown && out.Status == "UNKNOWN"
	return &out
}
