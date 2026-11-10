package application

import (
	"encoding/hex"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	"gorm.io/gorm"
	"strings"
	"time"
)

// projectRoleKeys 只从项目权威节点来源生成实际线路所需的分摊责任。
// 参数：c 为费用组织及分摊，refs 为权威复核结果，role 为所需岗位。
// 返回值：每名当前岗位受理人的分摊集合；缺失或来源不完整时拒绝。
func projectRoleKeys(c domain.Claim, refs []domain.ProjectReference, role string) (map[string][]string, error) {
	byKey, e := referenceShapeForFrozen(c, refs)
	if e != nil {
		return nil, e
	}
	out := map[string][]string{}
	for _, item := range c.Items {
		for _, alloc := range item.Allocations {
			found := false
			ref := byKey[projectPolicyKey(alloc.ProjectID, alloc.ServiceItemID, item.OccurredDate)]
			for _, target := range ref.RequiredApproverRefs {
				if target.NodeRole != role || !validProjectApprover(c, alloc, target) {
					continue
				}
				out[target.SubjectID] = append(out[target.SubjectID], item.ID+"/"+alloc.ProjectID+"/"+alloc.ServiceItemID)
				found = true
			}
			if !found {
				return nil, domain.Fail("APPROVER_MISSING", "项目权威来源缺少当前线路所需岗位："+role, 422)
			}
		}
	}
	return out, nil
}

// validProjectApprover 区分真实经理版本和目录总监摘要，不伪造数字角色版本。
// 参数：c 为审核组织，alloc 为项目上下文，target 为权威岗位引用。
// 返回值：来源是否满足该岗位协议。
func validProjectApprover(c domain.Claim, alloc domain.Allocation, target domain.ApproverRef) bool {
	if target.SubjectID == "" {
		return false
	}
	if target.NodeRole == "PROJECT_EXPENSE_OWNER" {
		field := "manager_identity_id"
		if alloc.ServiceItemID != "" {
			field = "project_manager_id"
		}
		return target.SourceField == field && target.SourceVersion > 0
	}
	raw, e := hex.DecodeString(target.SourceDigest)
	if e != nil || len(raw) != 32 || target.SourceOrgID == "" {
		return false
	}
	if target.NodeRole == "SALES_DIRECTOR" {
		return target.SourceField == "role:sales_director" && target.SourceOrgID == c.OrgID
	}
	return target.NodeRole == "TECHNICAL_DIRECTOR" && target.SourceField == "role:technical_director"
}

// taskTransferAssignments 保留旧通用替代配置并按业务岗位和精确项目验证全部分摊覆盖。
// 参数：tx 为数据库，c 为单据，task 为冻结岗位范围，target 为明确替代人。
// 返回值：当前有效配置证据；缺任何项目覆盖时拒绝。
func taskTransferAssignments(tx *gorm.DB, c domain.Claim, task domain.Task, target string) ([]domain.Assignment, error) {
	if !isProjectRole(task.NodeRole) {
		return transferAssignments(tx, c, task.NodeCode, target)
	}
	rows, e := configs(tx, c.TenantID, "assignments", true)
	if e != nil {
		return nil, e
	}
	day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	evidence := []domain.Assignment{}
	covered := map[string]bool{}
	for _, row := range rows {
		var a domain.Assignment
		if e = json.Unmarshal([]byte(row.ContentJSON), &a); e != nil {
			return nil, e
		}
		if a.SubjectID != target || a.OrgID != c.OrgID || (a.Role != task.NodeRole && a.Role != task.NodeCode) || day < a.ValidFrom || (a.ValidUntil != "" && day > a.ValidUntil) {
			continue
		}
		for _, key := range task.AllocationKeys {
			parts := strings.Split(key, "/")
			if len(parts) != 3 {
				continue
			}
			if a.ProjectID == parts[1] || (a.ProjectID == "" && task.NodeRole != "PROJECT_EXPENSE_OWNER") {
				covered[key] = true
			}
		}
		evidence = append(evidence, a)
	}
	for _, key := range task.AllocationKeys {
		if !covered[key] {
			return nil, domain.Fail("TRANSFER_TARGET_INVALID", "替代映射未覆盖全部当前项目分摊", 422)
		}
	}
	if len(evidence) == 0 {
		return nil, domain.Fail("TRANSFER_TARGET_INVALID", "缺少当前有效替代映射", 422)
	}
	return evidence, nil
}

// currentTaskRoleKeys 仅复核本任务覆盖的项目和费用日，不让其他岗位缺失扩大阻断范围。
// 参数：c 为当前单据，refs 为完整有序权威响应，task 为冻结任务分摊。
// 返回值：该任务范围内的当前目标，或关联不完整错误。
func currentTaskRoleKeys(c domain.Claim, refs []domain.ProjectReference, task domain.Task) (map[string][]string, error) {
	indexed, e := referenceShapeForFrozen(c, refs)
	if e != nil {
		return nil, e
	}
	allowed := map[string]bool{}
	for _, key := range task.AllocationKeys {
		allowed[key] = true
	}
	selected := c
	selected.Items = nil
	for _, item := range c.Items {
		copyItem := item
		copyItem.Allocations = nil
		for _, alloc := range item.Allocations {
			if allowed[item.ID+"/"+alloc.ProjectID+"/"+alloc.ServiceItemID] {
				copyItem.Allocations = append(copyItem.Allocations, alloc)
			}
		}
		if len(copyItem.Allocations) > 0 {
			selected.Items = append(selected.Items, copyItem)
		}
	}
	selectedRefs := []domain.ProjectReference{}
	for _, request := range claimReferences(selected) {
		selectedRefs = append(selectedRefs, indexed[projectPolicyKey(request.ProjectID, request.ServiceItemID, request.OccurredDate)])
	}
	return projectRoleKeys(selected, selectedRefs, task.NodeRole)
}
