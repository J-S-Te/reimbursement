package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
)

// CurrentApplicant 精确读取当前申请人的平台任职，不接受浏览器指定他人身份。
// 参数：ctx 为调用边界；actor 为可信会话的租户与用户。
// 返回值：本人安全资料；目录缺记录时保持空任职，依赖异常返回503。
func (p *Personnel) CurrentApplicant(ctx context.Context, actor domain.Actor) (domain.PersonOption, error) {
	person, found, err := p.LookupPerson(ctx, actor, actor.SubjectID, "")
	if err != nil {
		return domain.PersonOption{}, err
	}
	if !found {
		return domain.PersonOption{ID: actor.SubjectID, Organizations: []domain.OrganizationOption{}}, nil
	}
	return person, nil
}

// LookupPerson 只按后端已限定的精确用户读取安全姓名和有效任职。
// 参数：ctx/actor 为可信调用；target 为服务端候选；permission 可限定当前节点能力。
// 返回值：单个用户及是否命中；目录故障或不一致响应失败关闭。
func (p *Personnel) LookupPerson(ctx context.Context, actor domain.Actor, target, permission string) (domain.PersonOption, bool, error) {
	unavailable := domain.Fail("PERSONNEL_UNAVAILABLE", "当前人员及组织服务暂不可用", 503)
	if p == nil || p.Client == nil || actor.TenantID == "" || actor.SubjectID == "" ||
		target == "" || len(target) > 128 || target != strings.TrimSpace(target) {
		return domain.PersonOption{}, false, unavailable
	}
	query := url.Values{"user_id": {target}, "page": {"1"}, "page_size": {"2"}}
	if permission != "" {
		roles, err := approverRoles(permission)
		if err != nil || len(roles) == 0 {
			return domain.PersonOption{}, false, domain.Fail("APPROVER_ROLE_MISSING", "当前节点能力未配置", 422)
		}
		for _, role := range roles {
			query.Add("role_code", role)
		}
	}
	data, err := p.Client.Call(ctx, http.MethodGet, "/api/v1/internal/owner-directory?"+query.Encode(), nil)
	if err != nil {
		return domain.PersonOption{}, false, unavailable
	}
	person, found, err := decodePersonnelSelection(data, target)
	if err == nil && permission != "" && len(person.Organizations) == 0 {
		return domain.PersonOption{}, false, nil
	}
	return person, found, err
}

// SearchPeople 受控分页搜索应用可见人员，业务层仍须裁剪明确组织范围。
// 参数：ctx/actor 为可信调用；keyword 为姓名；page/size 为有界页码和大小。
// 返回值：安全目录分页；响应损坏、超界或依赖故障返回错误。
func (p *Personnel) SearchPeople(ctx context.Context, actor domain.Actor, keyword string, page, size int) (domain.PersonOptionsPage, error) {
	result := domain.PersonOptionsPage{Items: []domain.PersonOption{}}
	unavailable := domain.Fail("PERSONNEL_UNAVAILABLE", "当前人员及组织服务暂不可用", 503)
	if p == nil || p.Client == nil || actor.TenantID == "" || actor.SubjectID == "" ||
		page < 1 || size < 1 || size > 50 || utf8.RuneCountInString(keyword) > 100 {
		return result, unavailable
	}
	query := url.Values{"keyword": {keyword}, "page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(size)}}
	data, err := p.Client.Call(ctx, http.MethodGet, "/api/v1/internal/owner-directory?"+query.Encode(), nil)
	if err != nil {
		return result, unavailable
	}
	var raw struct {
		Total *int64            `json:"total"`
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(data, &raw) != nil || raw.Total == nil || *raw.Total < 0 ||
		len(raw.Items) > size || int64(len(raw.Items)) > *raw.Total {
		return result, unavailable
	}
	result.Total = *raw.Total
	for _, item := range raw.Items {
		var identity struct {
			ID string `json:"user_id"`
		}
		if json.Unmarshal(item, &identity) != nil || identity.ID == "" {
			return result, unavailable
		}
		single, _ := json.Marshal(map[string]any{"total": 1, "items": []json.RawMessage{item}})
		person, _, err := decodePersonnelSelection(single, identity.ID)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, person)
	}
	return result, nil
}

// decodePersonnelSelection 校验平台负责人目录契约并裁剪为业务选择资料。
// 参数：data 为有界目录响应；target 为请求中精确限定的用户。
// 返回值：安全姓名与组织，空目录为未命中，错用户或损坏资料返回503。
func decodePersonnelSelection(data json.RawMessage, target string) (domain.PersonOption, bool, error) {
	var page struct {
		Total *int64 `json:"total"`
		Items []struct {
			ID            string `json:"user_id"`
			Name          string `json:"display_name"`
			Organizations []struct {
				ID        string `json:"organization_id"`
				Name      string `json:"organization_name"`
				IsPrimary bool   `json:"is_primary"`
			} `json:"organizations"`
		} `json:"items"`
	}
	unavailable := domain.Fail("PERSONNEL_UNAVAILABLE", "人员任职响应不完整，请稍后重试", 503)
	if json.Unmarshal(data, &page) != nil || page.Total == nil || *page.Total < 0 {
		return domain.PersonOption{}, false, unavailable
	}
	if *page.Total == 0 && len(page.Items) == 0 {
		return domain.PersonOption{}, false, nil
	}
	if *page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != target ||
		strings.TrimSpace(page.Items[0].Name) == "" || utf8.RuneCountInString(page.Items[0].Name) > 200 {
		return domain.PersonOption{}, false, unavailable
	}
	item := page.Items[0]
	person := domain.PersonOption{ID: target, Name: item.Name, Organizations: []domain.OrganizationOption{}}
	seen := map[string]bool{}
	for _, org := range item.Organizations {
		if org.ID == "" || org.Name == "" || seen[org.ID] || len(item.Organizations) > 100 {
			return domain.PersonOption{}, false, unavailable
		}
		seen[org.ID] = true
		person.Organizations = append(person.Organizations, domain.OrganizationOption{
			ID: org.ID, Name: org.Name, IsPrimary: org.IsPrimary,
		})
	}
	sort.Slice(person.Organizations, func(i, j int) bool {
		return person.Organizations[i].ID < person.Organizations[j].ID
	})
	return person, true, nil
}
