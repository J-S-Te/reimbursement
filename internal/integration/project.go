package integration

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	"net/http"
)

type Projects struct{ Client *Client }

// Search 仅以当前报销会话主体代理最小项目引用查询。
// 参数：ctx为取消边界；actor为服务端主体；keyword/cursor/pageSize为有界查询。
// 返回值：项目服务的最小引用信封；依赖失败返回503业务错误。
func (p *Projects) Search(ctx context.Context, actor domain.Actor, keyword, cursor string, pageSize int) (json.RawMessage, error) {
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 50 {
		pageSize = 50
	}
	data, err := p.Client.Call(ctx, http.MethodPost, "/internal/v1/reimbursement/project-references/search", map[string]any{"actor_subject_id": actor.SubjectID, "keyword": keyword, "cursor": cursor, "page_size": pageSize})
	if err != nil {
		return nil, domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "项目资格服务暂不可用", 503)
	}
	return data, nil
}

// Validate 为指定草稿版本申请权威项目资格快照。
// 参数：ctx和actor限定调用；claimID/version绑定草稿；refs为后端从草稿派生的引用集合。
// 返回值：有效期内权威结果；服务失败或畸形响应均不放行。
func (p *Projects) Validate(ctx context.Context, actor domain.Actor, claimID string, version int64, refs []domain.ReferenceRequest) ([]domain.ProjectReference, error) {
	if len(refs) > 1000 {
		return nil, domain.Fail("PROJECT_REFERENCE_LIMIT", "项目引用超过受控上限", 422)
	}
	all := make([]domain.ProjectReference, 0, len(refs))
	for start := 0; start < len(refs); start += 100 {
		batch, err := p.validateBatch(ctx, actor, claimID, version, refs[start:min(start+100, len(refs))])
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}
	return all, nil
}

// validateBatch 以项目服务单批上限复核同一草稿的精确引用集合。
// 参数：ctx/actor限定调用；claimID/version绑定同一草稿；refs至多100个引用。
// 返回值：完整单批结果；错误不向调用方返回部分资格。
func (p *Projects) validateBatch(ctx context.Context, actor domain.Actor, claimID string, version int64, refs []domain.ReferenceRequest) ([]domain.ProjectReference, error) {
	data, err := p.Client.Call(ctx, http.MethodPost, "/internal/v1/reimbursement/project-references/batch-validate", map[string]any{"actor_subject_id": actor.SubjectID, "claim_id": claimID, "draft_version": version, "references": refs})
	if err != nil {
		return nil, domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "项目资格复核失败，请保留草稿稍后重试", 503)
	}
	var result struct {
		Items        []domain.ProjectReference `json:"items"`
		ClaimID      string                    `json:"claim_id"`
		DraftVersion int64                     `json:"draft_version"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Items) != len(refs) || result.ClaimID != claimID || result.DraftVersion != version {
		return nil, domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "项目返回的资格结果不完整", 503)
	}
	return result.Items, nil
}
