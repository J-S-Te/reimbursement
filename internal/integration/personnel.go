package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"

	"github.com/j-s-te/reimbursement/authz"
	"github.com/j-s-te/reimbursement/internal/domain"
)

type Personnel struct{ Client *Client }

// approverRoles 从本应用真实目录推导持有动作能力的角色，不接受浏览器扩大查询范围。
// 参数：permission为后端固定节点所需权限。
// 返回值：稳定排序的最小角色集合；空集合意味着未配置目标能力。
func approverRoles(permission string) ([]string, error) {
	var manifest struct {
		Roles []struct {
			Code        string   `json:"code"`
			Permissions []string `json:"permissions"`
		} `json:"roles"`
	}
	if err := json.Unmarshal(authz.PermissionManifest, &manifest); err != nil {
		return nil, err
	}
	roles := []string{}
	for _, role := range manifest.Roles {
		for _, code := range role.Permissions {
			if code == permission {
				roles = append(roles, role.Code)
				break
			}
		}
	}
	sort.Strings(roles)
	return roles, nil
}

// VerifyApprover 精确复核目标主体当前在职与应用角色，组织责任仍由本地映射校验。
// 参数：ctx为超时边界；actor为当前可信操作者；target为后端批准的转交目标；permission为节点要求。
// 返回值：目录不可用503；主体无当前资格422；不会替代本地任务版本或责任范围检查。
func (p *Personnel) VerifyApprover(ctx context.Context, actor domain.Actor, target, permission string) error {
	if p == nil || p.Client == nil {
		return domain.Fail("PERSONNEL_UNAVAILABLE", "当前人员资格服务未配置", 503)
	}
	if actor.TenantID == "" || actor.SubjectID == "" || target == "" || len(target) > 128 || target != strings.TrimSpace(target) {
		return domain.Fail("APPROVER_INELIGIBLE", "审批目标身份不完整", 422)
	}
	roles, err := approverRoles(permission)
	if err != nil || len(roles) == 0 {
		return domain.Fail("APPROVER_ROLE_MISSING", "节点审批能力尚未正确配置", 422)
	}
	query := url.Values{"user_id": {target}, "page": {"1"}, "page_size": {"2"}}
	for _, role := range roles {
		query.Add("role_code", role)
	}
	data, err := p.Client.Call(ctx, "GET", "/api/v1/internal/owner-directory?"+query.Encode(), nil)
	if err != nil {
		return domain.Fail("PERSONNEL_UNAVAILABLE", "当前人员资格服务暂不可用，不能转交", 503)
	}
	var page struct {
		Total int `json:"total"`
		Items []struct {
			UserID        string `json:"user_id"`
			Organizations []struct {
				OrganizationID string `json:"organization_id"`
			} `json:"organizations"`
		} `json:"items"`
	}
	if json.Unmarshal(data, &page) != nil {
		return domain.Fail("PERSONNEL_UNAVAILABLE", "人员资格响应不完整", 503)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].UserID != target || len(page.Items[0].Organizations) == 0 {
		return domain.Fail("APPROVER_INELIGIBLE", "目标人员不在职或缺少当前节点审批能力", 422)
	}
	return nil
}

// CurrentExportActor 重新验证生成导出所需的当前能力，旧管理员标记不能永久授权任务。
// 参数：ctx 为调用边界，requested 为发起时冻结身份，仅允许收窄其能力。
// 返回值：当前仍有效的身份能力；可选能力明确撤销则剔除，目录故障失败关闭。
func (p *Personnel) CurrentExportActor(ctx context.Context, requested domain.Actor) (domain.Actor, error) {
	for _, permission := range []string{"expense.archive.export", "expense.claim.read"} {
		if !requested.Has(permission) {
			return domain.Actor{}, domain.Fail("EXPORT_AUTHORIZATION_INVALID", "导出任务缺少读取能力", 403)
		}
		if e := p.VerifyApprover(ctx, requested, requested.SubjectID, permission); e != nil {
			return domain.Actor{}, e
		}
	}
	current := requested
	current.Permissions = []string{"expense.archive.export", "expense.claim.read"}
	current.OrgIDs = nil
	for _, permission := range []string{"expense.scope.tenant_all", "expense.audit.read", "expense.finance.review", "expense.payment.read", "expense.payment.record", "expense.archive.read", "expense.claim.reopen", "expense.claim.approved_void", "expense.payment.exception.record", "expense.payment.exception.review"} {
		if !requested.Has(permission) {
			continue
		}
		e := p.VerifyApprover(ctx, requested, requested.SubjectID, permission)
		if e == nil {
			current.Permissions = append(current.Permissions, permission)
			continue
		}
		var failure *domain.Problem
		if errors.As(e, &failure) && failure.Status == 422 {
			continue
		}
		return domain.Actor{}, e
	}
	return current, nil
}
