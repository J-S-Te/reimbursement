package application

import (
	"context"
	"github.com/j-s-te/reimbursement/internal/domain"
	"gorm.io/gorm"
	"sort"
	"time"
)

type submissionProof struct {
	DraftVersion   int64
	Round          int
	PlanHash       string
	StartedAt      time.Time
	Qualifications []domain.ApproverQualification
}

// qualificationPlanHash 将当前目标、节点、分摊和规则版本转为可复核的资格集合摘要。
// 参数：plan 为服务端生成路线。
// 返回值：排除随机任务ID和目录证明的稳定摘要。
func qualificationPlanHash(plan approvalPlan) (string, error) {
	type target struct {
		Node                            int
		Code, Role, Subject, Permission string
		Keys                            []string
	}
	targets := []target{}
	for _, task := range plan.Tasks {
		keys := append([]string{}, task.AllocationKeys...)
		sort.Strings(keys)
		targets = append(targets, target{task.Node, task.NodeCode, task.NodeRole, task.Assignee, task.RequiredPermission, keys})
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Node != targets[j].Node {
			return targets[i].Node < targets[j].Node
		}
		return targets[i].Subject < targets[j].Subject
	})
	return digest(struct {
		Rule               string
		HistoricalPolicies []domain.HistoricalTailApplication
		Targets            []target
	}{plan.RuleVersion, plan.HistoricalPolicies, targets})
}

// preflightSubmissionApprovers 事务外核查本轮每个审批人的当前任职及精确节点能力。
// 参数：ctx 为网络取消边界，db 为数据库，cc 为可信提交命令，c 为版本固定草稿。
// 返回值：不可用、无资格或配置不完整错误，不写待办。
func (s *Service) preflightSubmissionApprovers(ctx context.Context, db *gorm.DB, cc *commandContext, c domain.Claim) error {
	if e := domain.ValidateClaim(&c); e != nil {
		return e
	}
	if e := applyStandards(db, &c); e != nil {
		return e
	}
	if e := applySubmissionProjectPolicies(db, &c, cc.refs); e != nil {
		return e
	}
	c.CurrentRound++
	plan, e := buildApprovalPlan(db, c, cc.refs)
	if e != nil {
		return e
	}
	if s.deps.Personnel == nil {
		return domain.Fail("PERSONNEL_DIRECTORY_UNAVAILABLE", "当前审批人员资格目录未配置", 503)
	}
	hash, e := qualificationPlanHash(plan)
	if e != nil {
		return e
	}
	proof := &submissionProof{DraftVersion: c.Version, Round: c.CurrentRound, PlanHash: hash, StartedAt: time.Now().UTC(), Qualifications: []domain.ApproverQualification{}}
	seen := map[string]bool{}
	for _, task := range plan.Tasks {
		if task.NodeCode == "FINANCE_DIRECTOR" || task.NodeCode == "ACCOUNTANT" {
			tenantAll, err := s.verifyTargetFinanceScope(ctx, db, cc.actor, task.Assignee, c.OrgID)
			if err != nil {
				return err
			}
			if tenantAll {
				proof.Qualifications = append(proof.Qualifications, domain.ApproverQualification{SubjectID: task.Assignee, RequiredPermission: "expense.scope.tenant_all", VerifiedAt: time.Now().UTC(), Source: "personnel.verify_approver"})
			}
		}
		key := task.Assignee + "\x00" + task.RequiredPermission
		if seen[key] {
			continue
		}
		if e = s.deps.Personnel.VerifyApprover(ctx, cc.actor, task.Assignee, task.RequiredPermission); e != nil {
			return e
		}
		proof.Qualifications = append(proof.Qualifications, domain.ApproverQualification{SubjectID: task.Assignee, RequiredPermission: task.RequiredPermission, VerifiedAt: time.Now().UTC(), Source: "personnel.verify_approver"})
		seen[key] = true
	}
	cc.submission = proof
	return nil
}

// bindSubmissionQualifications 在事务内重新生成路线后复核当前资格证明并冻结审核依据。
// 参数：tx 为事务，cc 为预检命令，c 为锁定单据，plan 为事务内重算路线。
// 返回值：证明缺失、过期或集合改变时拒绝本次提交。
func bindSubmissionQualifications(tx *gorm.DB, cc *commandContext, c domain.Claim, plan *approvalPlan) error {
	proof := cc.submission
	if proof == nil || proof.DraftVersion != c.Version || proof.Round != c.CurrentRound || time.Since(proof.StartedAt) < 0 || time.Since(proof.StartedAt) > time.Minute {
		return domain.Fail("APPROVER_QUALIFICATION_STALE", "审批人员资格证明缺失或过期，请重试", 409)
	}
	hash, e := qualificationPlanHash(*plan)
	if e != nil {
		return e
	}
	if hash != proof.PlanHash {
		return domain.Fail("APPROVER_QUALIFICATION_STALE", "审批路线或负责人范围已改变，请重新校验", 409)
	}
	for _, task := range plan.Tasks {
		if task.NodeCode != "ACCOUNTANT" && task.NodeCode != "FINANCE_DIRECTOR" {
			continue
		}
		target := domain.Actor{TenantID: c.TenantID, SubjectID: task.Assignee}
		for _, q := range proof.Qualifications {
			if q.SubjectID == task.Assignee && q.RequiredPermission == "expense.scope.tenant_all" {
				target.Permissions = []string{"expense.scope.tenant_all"}
			}
		}
		allowed, err := financeScope(tx, target, c.OrgID)
		if err != nil {
			return err
		}
		if !allowed {
			return domain.Fail("APPROVER_FINANCE_SCOPE_MISSING", "财务目标范围已改变", 422)
		}
	}
	plan.Qualifications = append([]domain.ApproverQualification{}, proof.Qualifications...)
	return nil
}
