package application

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// preflight 为新命令执行动作权限与事务外远端复核。
// 参数：ctx 为上下文，db 为数据库，cc 为命令，payload 为请求内容。
// 返回值：依赖不可用或前置条件错误。
func (s *Service) preflight(ctx context.Context, db *gorm.DB, cc *commandContext, payload json.RawMessage) error {
	permission := map[string]string{"claim.create": "expense.claim.create", "claim.update": "expense.claim.update", "claim.submit": "expense.claim.submit", "claim.withdraw": "expense.claim.withdraw", "claim.void": "expense.claim.void", "claim.reopen": "expense.claim.reopen", "claim.approved_void": "expense.claim.approved_void", "payment.exception.record": "expense.payment.exception.record", "payment.exception.review": "expense.payment.exception.review", "invoice.create": "expense.invoice.upload", "invoice.confirm": "expense.invoice.review", "invoice.verify": "expense.invoice.verify", "invoice.reparse": "expense.invoice.upload", "approval.action": "expense.approval.act", "approval.recover": "expense.approval.recover", "payment.record": "expense.payment.record", "reversal.request": "expense.payment.reversal.request", "reversal.decide": "expense.payment.reversal.approve", "config.save": "expense.config.manage", "config.publish": "expense.policy.publish", "config.retire": "expense.policy.publish", "config.replace": "expense.policy.publish", "archive.package.create": "expense.archive.export", "archive.retry": "expense.archive.retry", "invoice.correction.request": "expense.invoice.review", "invoice.correction.decide": "expense.invoice.correct", "claim.invoice.reconfirm": "expense.claim.submit", "invoice.source.append": "expense.invoice.review", "invoice.risk.record": "expense.invoice.review", "invoice.risk.close": "expense.invoice.correct"}[cc.operation]
	if permission == "" {
		return domain.Fail("OPERATION_NOT_SUPPORTED", "不支持此命令", 422)
	}
	if e := require(cc.actor, permission); e != nil {
		return e
	}
	if e := commandResourceBounds(cc.operation, payload); e != nil {
		return e
	}
	if cc.operation == "claim.create" || cc.operation == "claim.update" || cc.operation == "claim.submit" {
		if e := s.preflightDraftAppointment(ctx, db, cc, payload); e != nil {
			return e
		}
	}
	// 票据控制命令在进入本地事务前复核真实原件归属与文件网关，避免只靠事务内结论承担外部文件权限。
	switch cc.operation {
	case "invoice.correction.request", "invoice.correction.decide", "claim.invoice.reconfirm", "invoice.source.append", "invoice.risk.record", "invoice.risk.close":
		return s.preflightInvoiceControl(ctx, db, cc, payload)
	}
	if cc.operation == "claim.reopen" || cc.operation == "claim.approved_void" || strings.HasPrefix(cc.operation, "payment.exception.") {
		return s.preflightFinanceChanges(ctx, db, cc, payload)
	}
	if cc.operation == "approval.recover" {
		return s.preflightApprovalRecovery(ctx, db, cc, payload)
	}
	if cc.operation == "approval.action" {
		return s.preflightTransfer(ctx, db, cc, payload)
	}
	if cc.operation == "invoice.verify" {
		var v VerificationInput
		if e := json.Unmarshal(payload, &v); e != nil {
			return e
		}
		if s.deps.Files == nil {
			return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件服务未配置", 503)
		}
		return s.deps.Files.Validate(ctx, cc.actor, v.EvidenceFileIDs, "reimbursement.invoice.original")
	}
	if cc.operation == "invoice.create" || cc.operation == "invoice.confirm" {
		var invoice domain.Invoice
		if e := json.Unmarshal(payload, &invoice); e != nil {
			return e
		}
		if s.deps.Files == nil {
			return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件服务尚未配置", 503)
		}
		return s.deps.Files.Validate(ctx, cc.actor, invoice.FileIDs, "reimbursement.invoice.original")
	}
	if cc.operation == "reversal.request" {
		var input ReversalInput
		if e := json.Unmarshal(payload, &input); e != nil {
			return e
		}
		if s.deps.Files == nil {
			return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件服务尚未配置", 503)
		}
		return s.deps.Files.Validate(ctx, cc.actor, input.EvidenceFileIDs, "reimbursement.payment.voucher")
	}
	if cc.operation != "claim.submit" && cc.operation != "payment.record" {
		return nil
	}
	c, _, e := loadClaim(db, cc.actor.TenantID, cc.resourceID, false)
	if e != nil {
		return e
	}
	if e = s.canRead(db, cc.actor, c); e != nil {
		return e
	}
	if e = verifyVersion(c.Version, cc.input.ExpectedVersion); e != nil {
		return e
	}
	if e = domain.ValidateResourceLimits(c); e != nil {
		return e
	}
	if cc.operation == "claim.submit" && c.Applicant != cc.actor.SubjectID {
		return domain.Fail("FORBIDDEN", "只能提交本人单据", 403)
	}
	if cc.operation == "claim.submit" && c.Status != "DRAFT" && c.Status != "RETURNED" {
		return domain.Fail("STATE_CONFLICT", "当前单据不能通过新政策补提交或补审批", 409)
	}
	if cc.operation == "payment.record" {
		if e = requireFinancial(db, cc.actor, c, "expense.payment.record"); e != nil {
			return e
		}
		if c.Status != "APPROVED_PENDING_PAYMENT" || c.ActivePaymentID != "" {
			return domain.Fail("STATE_CONFLICT", "当前单据不是可登记付款的待付状态", 409)
		}
	}
	if s.deps.Projects == nil {
		return domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "项目引用服务尚未配置", 503)
	}
	refs, e := s.deps.Projects.Validate(ctx, domain.Actor{TenantID: c.TenantID, SubjectID: c.Applicant}, c.ID, c.Version, claimReferences(c))
	if e != nil {
		return e
	}
	if _, e = referenceShape(c, refs); e != nil {
		return e
	}
	if cc.operation == "payment.record" {
		if e = validatePaymentProjectPolicies(db, c, refs); e != nil {
			return e
		}
	}
	cc.refs = refs
	if cc.operation == "claim.submit" {
		if e = s.preflightSubmissionApprovers(ctx, db, cc, c); e != nil {
			return e
		}
	}
	if e = s.validateClaimEvidence(ctx, c); e != nil {
		return e
	}
	if s.deps.Files == nil {
		return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件服务尚未配置", 503)
	}
	if cc.operation == "payment.record" {
		var input PaymentInput
		if e = json.Unmarshal(payload, &input); e != nil {
			return e
		}
		return s.deps.Files.Validate(ctx, cc.actor, input.VoucherFileIDs, "reimbursement.payment.voucher")
	}
	var invoiceRows []store.Invoice
	ids := invoiceIDs(c)
	if len(ids) > 0 {
		if e = db.Where("tenant_id = ? AND id IN ?", c.TenantID, ids).Order("id").Find(&invoiceRows).Error; e != nil {
			return e
		}
		if len(invoiceRows) != len(ids) {
			return domain.Fail("INVOICE_NOT_AVAILABLE", "存在不可用的发票", 422)
		}
	}
	for _, row := range invoiceRows {
		var invoice domain.Invoice
		if e = json.Unmarshal([]byte(row.ContentJSON), &invoice); e != nil {
			return e
		}
		if row.Owner != c.Applicant || len(invoice.FileIDs) == 0 {
			return domain.Fail("INVOICE_NOT_AVAILABLE", "发票缺少本人可使用的原件", 422)
		}
		if e = s.deps.Files.Validate(ctx, cc.actor, invoice.FileIDs, "reimbursement.invoice.original"); e != nil {
			return e
		}
	}
	return nil
}

// claimReferences 构造去重的项目、服务项与发生日期集合。
// 参数：c 为当前草稿。
// 返回值：稳定排序的远端复核引用。
func claimReferences(c domain.Claim) []domain.ReferenceRequest {
	var refs []domain.ReferenceRequest
	approvalOrg := ""
	if c.RouteType == "SALES" {
		approvalOrg = c.OrgID
	}
	seen := map[string]bool{}
	for _, item := range c.Items {
		for _, a := range item.Allocations {
			key := a.ProjectID + "\x00" + a.ServiceItemID + "\x00" + item.OccurredDate
			if !seen[key] {
				refs = append(refs, domain.ReferenceRequest{ApprovalOrgID: approvalOrg, ProjectID: a.ProjectID, ServiceItemID: a.ServiceItemID, OccurredDate: item.OccurredDate})
				seen[key] = true
			}
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].ProjectID+refs[i].ServiceItemID+refs[i].OccurredDate < refs[j].ProjectID+refs[j].ServiceItemID+refs[j].OccurredDate
	})
	return refs
}

// invoiceIDs 获取稳定排序的单据票据集合以固定锁顺序。
// 参数：c 为单据。
// 返回值：去重票据标识。
func invoiceIDs(c domain.Claim) []string {
	seen := map[string]bool{}
	ids := []string{}
	for _, item := range c.Items {
		for _, u := range item.InvoiceUsages {
			if !seen[u.InvoiceID] {
				ids = append(ids, u.InvoiceID)
				seen[u.InvoiceID] = true
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// editClaim 持久化本人草稿，所有只读字段由服务器重建。
// 参数：tx 为事务，cc 为可信命令，payload 为草稿可编辑字段。
// 返回值：持久化草稿、对象标识及错误。
func (s *Service) editClaim(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input domain.Claim
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", domain.Fail("INVALID_REQUEST", "草稿字段不正确", 422)
	}
	if e := validateClaimEditingFields(input); e != nil {
		return nil, "", e
	}
	if len(input.Title) > 200 || len(input.Items) > 100 {
		return nil, "", domain.Fail("INVALID_CLAIM", "标题或费用项超过上限", 422)
	}
	var c domain.Claim
	var e error
	previous := int64(0)
	if cc.operation == "claim.create" {
		id, err := newID()
		if err != nil {
			return nil, "", err
		}
		c = domain.Claim{ID: id, TenantID: cc.actor.TenantID, Applicant: cc.actor.SubjectID, Status: "DRAFT", Version: 1, CreatedAt: time.Now().UTC()}
	} else {
		c, _, e = loadClaim(tx, cc.actor.TenantID, cc.resourceID, true)
		if e != nil {
			return nil, "", e
		}
		if c.Applicant != cc.actor.SubjectID {
			return nil, "", domain.Fail("FORBIDDEN", "只能维护本人草稿", 403)
		}
		if c.Status != "DRAFT" && c.Status != "RETURNED" {
			return nil, "", domain.Fail("STATE_CONFLICT", "当前单据不可编辑", 409)
		}
		if e = verifyVersion(c.Version, cc.input.ExpectedVersion); e != nil {
			return nil, "", e
		}
		previous = c.Version
		c.Version++
	}
	if input.OrgID != "" {
		candidate := c
		candidate.OrgID = input.OrgID
		profile, err := employeeProfile(tx, candidate)
		if err != nil {
			return nil, "", err
		}
		if input.RouteType != "" && profile.RouteType != input.RouteType {
			return nil, "", domain.Fail("ROUTE_TYPE_MISMATCH", "费用线路与已审核人员资料不一致", 422)
		}
		input.RouteType = profile.RouteType
	}
	if e = populateDraftClaim(&c, input); e != nil {
		return nil, "", e
	}
	if e = verifyDraftAppointment(tx, cc, c); e != nil {
		return nil, "", e
	}
	if e = rebuildDraftClassificationEvidence(tx, &c); e != nil {
		return nil, "", e
	}
	c.UpdatedAt = time.Now().UTC()
	if previous == 0 {
		raw, err := encode(c)
		if err != nil {
			return nil, "", err
		}
		e = tx.Create(&store.Claim{ID: c.ID, TenantID: c.TenantID, Applicant: c.Applicant, OrgID: c.OrgID, Status: c.Status, Version: c.Version, ContentJSON: raw, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}).Error
	} else {
		e = saveClaim(tx, &c, previous)
	}
	if e == nil {
		e = indexClaimReferences(tx, c)
	}
	return c, c.ID, e
}

// populateDraftClaim 重建草稿可编辑字段并清除所有客户端政策结论。
// 参数：c 为已核验归属的草稿副本，input 为申请人编辑内容。
// 返回值：分摊、行程声明或标识生成错误。
func populateDraftClaim(c *domain.Claim, input domain.Claim) error {
	var e error
	c.Title = strings.TrimSpace(input.Title)
	c.ClaimType = input.ClaimType
	c.Remark = input.Remark
	c.InvoiceDateWarnings = nil
	c.OrgID = input.OrgID
	c.RouteType = input.RouteType
	c.Currency = "CNY"
	c.FinanceEntityID = input.FinanceEntityID
	c.PayeeProfileID = input.PayeeProfileID
	c.ArchiveRetention = nil
	c.Items = input.Items
	c.Payable = 0
	for i := range c.Items {
		if e = validateTravelDeclaration(c.Items[i].TravelDeclaration); e != nil {
			return e
		}
		if e = domain.NormalizeItemAllocations(&c.Items[i]); e != nil {
			return e
		}
		if c.Items[i].ID == "" {
			c.Items[i].ID, e = newID()
			if e != nil {
				return e
			}
		}
		c.Items[i].ClassificationEvidence = nil
		c.Items[i].FinanceReviewRequired = false
		c.Items[i].ReviewHints = nil
		c.Items[i].StandardPolicyVersion = 0
		c.Items[i].PolicyCompatibility = nil
		c.Items[i].HistoricalTailApplications = nil
		c.Items[i].TravelAssessment = nil
		c.Items[i].RequiresTravelFacts = false
		c.Items[i].OverStandard = false
		c.Items[i].StandardVersionID = ""
		c.Items[i].StandardLimit = 0
	}
	return nil
}

// validateClaimEditingFields 校验单据类型、备注与费用性质，保留历史零值兼容。
// 参数：input 为新建或编辑请求中的单据字段。
// 返回值：非法类型、费用性质或备注超过2000字符的422错误。
func validateClaimEditingFields(input domain.Claim) error {
	switch input.ClaimType {
	case "", "TRAVEL", "DAILY", "PROJECT_EXECUTION":
	default:
		return domain.Fail("INVALID_CLAIM_TYPE", "报销类型必须为差旅、日常或项目执行", 422)
	}
	for i, item := range input.Items {
		if err := validateExpenseNature(item.ExpenseNature, i); err != nil {
			return err
		}
	}
	if utf8.RuneCountInString(input.Remark) > 2000 {
		return domain.Fail("INVALID_CLAIM_REMARK", "报销备注最多2000个字符", 422)
	}
	return nil
}

// closeClaim 结束当前轮次并释放未消费占票，历史账本保持不可变。
// 参数：tx 为事务，cc 为撤回或作废命令。
// 返回值：新单据状态、对象与错误。
func (s *Service) closeClaim(tx *gorm.DB, cc *commandContext) (any, string, error) {
	c, _, e := loadClaim(tx, cc.actor.TenantID, cc.resourceID, true)
	if e != nil {
		return nil, "", e
	}
	if c.Applicant != cc.actor.SubjectID {
		return nil, "", domain.Fail("FORBIDDEN", "只能撤回或作废本人报销", 403)
	}
	if e = verifyVersion(c.Version, cc.input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if cc.operation == "claim.withdraw" {
		if c.Status != "IN_APPROVAL" {
			return nil, "", domain.Fail("STATE_CONFLICT", "当前状态不能撤回", 409)
		}
		c.Status = "DRAFT"
	} else {
		if c.Status != "DRAFT" && c.Status != "RETURNED" {
			return nil, "", domain.Fail("STATE_CONFLICT", "仅未批准单据可以直接作废", 409)
		}
		c.Status = "VOID"
	}
	if e = cancelTasks(tx, c); e != nil {
		return nil, "", e
	}
	if e = releaseInvoices(tx, c); e != nil {
		return nil, "", e
	}
	previous := c.Version
	c.Version++
	e = saveClaim(tx, &c, previous)
	return c, c.ID, e
}

// cancelTasks 取消轮次尚未完成的任务。
// 参数：tx 为事务，c 为本次轮次单据。
// 返回值：数据库错误。
func cancelTasks(tx *gorm.DB, c domain.Claim) error {
	return tx.Model(&store.Task{}).Where("tenant_id = ? AND claim_id = ? AND round = ? AND status IN ?", c.TenantID, c.ID, c.CurrentRound, []string{"PENDING", "WAITING"}).Updates(map[string]any{"status": "CANCELLED", "version": gorm.Expr("version + 1")}).Error
}

// releaseInvoices 仅释放当前单据保留且未消费的证据占用。
// 参数：tx 为事务，c 为结束审批的单据。
// 返回值：数据库错误。
func releaseInvoices(tx *gorm.DB, c domain.Claim) error {
	return tx.Model(&store.Invoice{}).Where("tenant_id = ? AND reservation_claim_id = ? AND reservation_state = ?", c.TenantID, c.ID, "RESERVED").Updates(map[string]any{"reservation_state": "AVAILABLE", "reservation_claim_id": "", "version": gorm.Expr("version + 1")}).Error
}

// indexClaimReferences 同事务维护项目检索索引，索引不能反向修改批准内容。
// 参数：tx 为草稿事务，c 为服务器归属已固定的单据。
// 返回值：索引替换失败错误。
func indexClaimReferences(tx *gorm.DB, c domain.Claim) error {
	if e := tx.Where("tenant_id = ? AND claim_id = ?", c.TenantID, c.ID).Delete(&store.ClaimReference{}).Error; e != nil {
		return e
	}
	for _, item := range c.Items {
		for _, a := range item.Allocations {
			if a.ProjectID == "" {
				continue
			}
			id, e := newID()
			if e != nil {
				return e
			}
			if e = tx.Create(&store.ClaimReference{ID: id, TenantID: c.TenantID, ClaimID: c.ID, ProjectID: a.ProjectID, ServiceItemID: a.ServiceItemID, ItemID: item.ID, Amount: int64(a.Amount)}).Error; e != nil {
				return e
			}
		}
	}
	return nil
}
