package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/j-s-te/reimbursement/internal/domain"
	"gorm.io/gorm"
)

// ClaimPreviewInput 用已保存草稿坐标和可编辑字段描述一次只读预览。
type ClaimPreviewInput struct {
	domain.Claim
	ClaimID         string `json:"claim_id"`
	ExpectedVersion int64  `json:"expected_version"`
}

type claimPreviewNode struct {
	Node      int      `json:"node"`
	Code      string   `json:"node_code"`
	Label     string   `json:"node_label"`
	Assignees []string `json:"assignee_subject_ids"`
	Mode      string   `json:"mode"`
}

type claimPreviewResult struct {
	ClaimID         string                  `json:"claim_id"`
	ExpectedVersion int64                   `json:"expected_version"`
	Items           []domain.Item           `json:"items"`
	Route           *routeThresholdDecision `json:"route_decision"`
	Nodes           []claimPreviewNode      `json:"approval_nodes"`
	RuleVersion     string                  `json:"rule_version"`
	PreviewOnly     bool                    `json:"preview_only"`
}

// PreviewClaim 只读重算本人草稿标准和当前审批路线，不保存命令或任何业务结果。
// 参数：ctx 为取消边界，actor 为可信身份，payload 为草稿坐标与可编辑字段。
// 返回值：标准、超标阈值及可读审批节点；版本、权限或依赖异常时返回错误。
func (s *Service) PreviewClaim(ctx context.Context, actor domain.Actor, payload json.RawMessage) (any, error) {
	if err := require(actor, "expense.claim.update"); err != nil {
		return nil, err
	}
	input, err := decodeClaimPreview(payload)
	if err != nil {
		return nil, err
	}
	db := s.db.WithContext(ctx)
	c, _, err := loadClaim(db, actor.TenantID, input.ClaimID, false)
	if err != nil {
		return nil, err
	}
	if err = validatePreviewDraft(c, actor, input.ExpectedVersion); err != nil {
		return nil, err
	}
	if err = validateClaimEditingFields(input.Claim); err != nil {
		return nil, err
	}
	if err = domain.ValidateResourceLimits(input.Claim); err != nil {
		return nil, err
	}
	if err = populateDraftClaim(&c, input.Claim); err != nil {
		return nil, err
	}
	if err = domain.ValidateClaim(&c); err != nil {
		return nil, err
	}
	if s.deps.Projects == nil {
		return nil, domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "项目引用服务尚未配置", 503)
	}
	refs, err := s.deps.Projects.Validate(ctx, domain.Actor{TenantID: c.TenantID, SubjectID: c.Applicant}, c.ID, c.Version, claimReferences(c))
	if err != nil {
		return nil, err
	}
	if _, err = referenceShape(c, refs); err != nil {
		return nil, err
	}
	cc := &commandContext{actor: actor, refs: refs}
	if err = s.preflightSubmissionApprovers(ctx, db, cc, c); err != nil {
		return nil, err
	}
	if err = s.validatePreviewFiles(ctx, db, actor, c); err != nil {
		return nil, err
	}
	return s.finishClaimPreview(db, actor, c, input.ExpectedVersion, cc)
}

// decodeClaimPreview 拒绝身份声明并读取有界草稿坐标，不接受伪造持久化身份。
// 参数：payload 为来自有界HTTP请求的原始JSON。
// 返回值：已验证坐标的预览输入或公开请求错误。
func decodeClaimPreview(payload json.RawMessage) (ClaimPreviewInput, error) {
	var input ClaimPreviewInput
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &input) != nil || json.Unmarshal(payload, &fields) != nil || fields == nil {
		return input, domain.Fail("INVALID_REQUEST", "预览请求字段不正确", 422)
	}
	for field := range fields {
		switch strings.ToLower(field) {
		case "tenant_id", "applicant_subject_id", "permissions", "org_ids", "id", "version", "status":
			return input, domain.Fail("INVALID_REQUEST", "预览不能包含身份或持久化状态字段", 422)
		}
	}
	if strings.TrimSpace(input.ClaimID) == "" || input.ExpectedVersion <= 0 {
		return input, domain.Fail("INVALID_REQUEST", "请先保存草稿并提供当前版本", 422)
	}
	return input, nil
}

// validatePreviewDraft 复核预览对象是本人当前可编辑版本。
// 参数：c 为真实数据库草稿，actor 为可信身份，version 为用户读取的版本。
// 返回值：归属、状态或版本冲突错误。
func validatePreviewDraft(c domain.Claim, actor domain.Actor, version int64) error {
	if c.Applicant != actor.SubjectID {
		return domain.Fail("FORBIDDEN", "只能预览本人草稿", 403)
	}
	if c.Status != "DRAFT" && c.Status != "RETURNED" {
		return domain.Fail("STATE_CONFLICT", "当前单据不能编辑预览", 409)
	}
	return verifyVersion(c.Version, version)
}

// finishClaimPreview 在依赖验证后重新读取草稿并以正式算法计算展示结果。
// 参数：db 为只读数据库，actor 为身份，c 为编辑副本，version 为真实草稿版本，cc 为资格证明。
// 返回值：当前预览结果或政策、票据、并发变化错误。
func (s *Service) finishClaimPreview(db *gorm.DB, actor domain.Actor, c domain.Claim, version int64, cc *commandContext) (any, error) {
	current, _, err := loadClaim(db, actor.TenantID, c.ID, false)
	if err != nil {
		return nil, err
	}
	if err = validatePreviewDraft(current, actor, version); err != nil {
		return nil, err
	}
	if err = VerifyInvoiceControls(db, c, "SUBMIT"); err != nil {
		return nil, err
	}
	if err = applyStandards(db, &c); err != nil {
		return nil, err
	}
	if err = applySubmissionProjectPolicies(db, &c, cc.refs); err != nil {
		return nil, err
	}
	if err = validateProfiles(db, &c); err != nil {
		return nil, err
	}
	if err = validateClaimInvoices(db, &c, false); err != nil {
		return nil, err
	}
	c.CurrentRound++
	plan, err := buildApprovalPlan(db, c, cc.refs)
	if err != nil {
		return nil, err
	}
	if err = bindSubmissionQualifications(db, cc, c, &plan); err != nil {
		return nil, err
	}
	return claimPreviewResult{ClaimID: c.ID, ExpectedVersion: version, Items: c.Items,
		Route: plan.RouteDecision, Nodes: previewApprovalNodes(plan), RuleVersion: plan.RuleVersion, PreviewOnly: true}, nil
}

// validatePreviewFiles 沿用正式提交的独立费用证明与本人发票原件用途校验。
// 参数：ctx 为请求边界，db 为数据库，actor 为身份，c 为待预览费用副本。
// 返回值：文件不可用、归属或用途错误，不修改任何文件状态。
func (s *Service) validatePreviewFiles(ctx context.Context, db *gorm.DB, actor domain.Actor, c domain.Claim) error {
	if err := s.validateClaimEvidence(ctx, c); err != nil {
		return err
	}
	if s.deps.Files == nil {
		return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件服务尚未配置", 503)
	}
	invoices, err := snapshotInvoices(db, c)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.Fail("INVOICE_NOT_AVAILABLE", "发票不存在或无权使用", 422)
	}
	if err != nil {
		return err
	}
	for _, invoice := range invoices {
		if invoice.Owner != c.Applicant || len(invoice.FileIDs) == 0 {
			return domain.Fail("INVOICE_NOT_AVAILABLE", "发票缺少本人可使用的原件", 422)
		}
		if err = s.deps.Files.Validate(ctx, actor, invoice.FileIDs, "reimbursement.invoice.original"); err != nil {
			return err
		}
	}
	return nil
}

// previewApprovalNodes 将真实审批计划折叠为可读的ALL节点，不向调用者返回临时任务ID。
// 参数：plan 为正式算法生成并已复核人员资格的审批计划。
// 返回值：稳定顺序的节点名称与真实受理人集合。
func previewApprovalNodes(plan approvalPlan) []claimPreviewNode {
	labels := map[string]string{"BUSINESS_REVIEW": "业务负责人审批", "TECHNICAL_DIRECTOR": "技术总监审批", "FINANCE_DIRECTOR": "财务总监审批", "ACCOUNTANT": "财务会计复核"}
	nodes := []claimPreviewNode{}
	for _, task := range plan.Tasks {
		if len(nodes) == 0 || nodes[len(nodes)-1].Node != task.Node {
			label := labels[task.NodeCode]
			if label == "" {
				label = task.NodeCode
			}
			nodes = append(nodes, claimPreviewNode{Node: task.Node, Code: task.NodeCode, Label: label, Mode: "ALL", Assignees: []string{}})
		}
		nodes[len(nodes)-1].Assignees = append(nodes[len(nodes)-1].Assignees, task.Assignee)
	}
	return nodes
}
