package application

import (
	"context"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	"gorm.io/gorm"
)

// verifyTargetFinanceScope 事务外确认目标明确组织范围或当前独立全租户能力。
// 参数：ctx 为调用边界，db 为数据库，actor 为操作者，target/org 为财务目标范围。
// 返回值：是否使用当前全租户证明；普通无范围拒绝，目录故障失败关闭。
func (s *Service) verifyTargetFinanceScope(ctx context.Context, db *gorm.DB, actor domain.Actor, target, org string) (bool, error) {
	ok, e := financeScope(db, domain.Actor{TenantID: actor.TenantID, SubjectID: target}, org)
	if e != nil {
		return false, e
	}
	if ok {
		return false, nil
	}
	if s.deps.Personnel == nil {
		return false, domain.Fail("PERSONNEL_DIRECTORY_UNAVAILABLE", "当前人员目录未配置", 503)
	}
	e = s.deps.Personnel.VerifyApprover(ctx, actor, target, "expense.scope.tenant_all")
	if e == nil {
		return true, nil
	}
	var failure *domain.Problem
	if errors.As(e, &failure) && failure.Status == 422 {
		return false, domain.Fail("APPROVER_FINANCE_SCOPE_MISSING", "财务目标缺少该组织明确范围或当前全租户能力", 422)
	}
	return false, e
}
