package application

import (
	"encoding/json"
	"fmt"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
	"time"
)

type approvalPlan struct {
	RouteDecision      *routeThresholdDecision            `json:"route_decision,omitempty"`
	HistoricalPolicies []domain.HistoricalTailApplication `json:"historical_policies,omitempty"`
	Qualifications     []domain.ApproverQualification     `json:"qualifications,omitempty"`
	Tasks              []domain.Task                      `json:"tasks"`
	References         []domain.ProjectReference          `json:"references"`
	RuleVersion        string                             `json:"rule_version"`
}

// submit 冻结本轮内容、版本化标准、审批责任和票据占用。
// 参数：tx 为事务，cc 为带权威复核引用的新提交命令。
// 返回值：提交后的单据、对象与错误。
func (s *Service) submit(tx *gorm.DB, cc *commandContext) (any, string, error) {
	c, _, e := loadClaim(tx, cc.actor.TenantID, cc.resourceID, true)
	if e != nil {
		return nil, "", e
	}
	if c.Applicant != cc.actor.SubjectID {
		return nil, "", domain.Fail("FORBIDDEN", "只能提交本人申请", 403)
	}
	if e = verifyVersion(c.Version, cc.input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if c.Status != "DRAFT" && c.Status != "RETURNED" {
		return nil, "", domain.Fail("STATE_CONFLICT", "当前单据不能提交", 409)
	}
	// 持有单据锁后先复核票据重大更正与红冲风险闭环；未核准前不得写入新轮次或占用票据。
	if e = VerifyInvoiceControls(tx, c, "SUBMIT"); e != nil {
		return nil, "", e
	}
	if e = domain.ValidateClaim(&c); e != nil {
		return nil, "", e
	}
	if _, e = referenceShape(c, cc.refs); e != nil {
		return nil, "", e
	}
	if e = applyStandards(tx, &c); e != nil {
		return nil, "", e
	}
	if e = applySubmissionProjectPolicies(tx, &c, cc.refs); e != nil {
		return nil, "", e
	}
	if e = validateProfiles(tx, &c); e != nil {
		return nil, "", e
	}
	if e = freezeArchiveRetention(tx, &c, time.Now().UTC()); e != nil {
		return nil, "", e
	}
	if e = reserveInvoices(tx, &c); e != nil {
		return nil, "", e
	}
	c.CurrentRound++
	if c.ClaimNo == "" {
		c.ClaimNo, e = allocateNumber(tx, c.TenantID)
		if e != nil {
			return nil, "", e
		}
	}

	plan, e := buildApprovalPlan(tx, c, cc.refs)
	if e != nil {
		return nil, "", e
	}
	if e = verifyDraftAppointment(tx, cc, c); e != nil {
		return nil, "", e
	}
	if e = bindSubmissionQualifications(tx, cc, c, &plan); e != nil {
		return nil, "", e
	}
	invoices, e := snapshotInvoices(tx, c)
	if e != nil {
		return nil, "", e
	}
	hash, e := approvedContentHash(c, plan, invoices)
	if e != nil {
		return nil, "", e
	}
	c.ContentHash = hash
	c.Status = "IN_APPROVAL"
	previous := c.Version
	c.Version++
	if c.ClaimNo == "" {
		c.ClaimNo, e = allocateNumber(tx, c.TenantID)
		if e != nil {
			return nil, "", e
		}
	}
	content, e := encode(approvalSnapshot{Claim: c, Approval: plan, Invoices: invoices})
	if e != nil {
		return nil, "", e
	}
	snapshotID, e := newID()
	if e != nil {
		return nil, "", e
	}
	if e = tx.Create(&store.Snapshot{ID: snapshotID, TenantID: c.TenantID, ClaimID: c.ID, Round: c.CurrentRound, ContentHash: hash, ContentJSON: content}).Error; e != nil {
		return nil, "", e
	}
	for _, task := range plan.Tasks {
		raw, err := encode(task)
		if err != nil {
			return nil, "", err
		}
		if err = tx.Create(&store.Task{ID: task.ID, TenantID: c.TenantID, ClaimID: c.ID, Round: c.CurrentRound, Node: task.Node, NodeCode: task.NodeCode, Assignee: task.Assignee, Status: task.Status, Version: 1, ContentJSON: raw}).Error; err != nil {
			return nil, "", err
		}
		if task.Status == "PENDING" {
			if err = appendOutbox(tx, c.TenantID, "notification", c.ID, map[string]any{"task_id": task.ID, "assignee_subject_id": task.Assignee, "round": c.CurrentRound, "claim_id": c.ID}); err != nil {
				return nil, "", err
			}
		}
	}
	e = saveClaim(tx, &c, previous)
	return c, c.ID, e
}

// applyStandards 按发生日期、组织与人员维度选择唯一最高优先级标准。
// 参数：tx 为事务，c 为待提交费用。
// 返回值：标准缺失、冲突或无票政策错误。
func applyStandards(tx *gorm.DB, c *domain.Claim) error {
	profile, e := employeeProfile(tx, *c)
	if e != nil {
		return e
	}
	if profile.RouteType != c.RouteType {
		return domain.Fail("ROUTE_TYPE_MISMATCH", "费用线路与审核后的人员资料不一致", 422)
	}
	rows, e := configs(tx, c.TenantID, "standards", true)
	if e != nil {
		return e
	}
	for i := range c.Items {
		item := &c.Items[i]
		if e = validateItemDimensions(*item, i); e != nil {
			return e
		}
		item.Grade = profile.Grade
		item.RequiresTravelFacts = false
		var match *domain.Standard
		matchCount := 0
		for _, row := range rows {
			var standard domain.Standard
			if e = json.Unmarshal([]byte(row.ContentJSON), &standard); e != nil {
				return e
			}
			if !standardDimensionsMatch(standard, *item) || standard.FeeType != item.FeeType || standard.OrgID != c.OrgID || standard.CityLevel != item.CityLevel || standard.Grade != item.Grade || item.OccurredDate < standard.ValidFrom || (standard.ValidUntil != "" && item.OccurredDate > standard.ValidUntil) {
				continue
			}
			if match != nil && match.Priority == standard.Priority {
				matchCount++
			}
			if match == nil || standard.Priority > match.Priority {
				copy := standard
				match = &copy
				matchCount = 1
			}
		}
		if matchCount > 1 {
			return domain.Fail("STANDARD_CONFLICT", "同一费用存在重复最高优先级标准", 422)
		}
		if match == nil {
			return itemPolicyFailure(i, "fee_type", "STANDARD_MISSING", "未配置适用于日期、组织、城市、职级及交通住宿分类的标准")
		}
		units := item.Units
		if units <= 0 {
			return itemPolicyFailure(i, "units", "INVALID_UNITS", "请填写大于零的计量数量")
		}
		if match.Limit <= 0 || units > int64(^uint64(0)>>1)/int64(match.Limit) {
			return domain.Fail("AMOUNT_OVERFLOW", "标准计算超出金额范围", 422)
		}
		item.StandardVersionID = match.ID
		item.StandardPolicyVersion = match.Version
		item.RequiresTravelFacts = match.RequiresTravelFacts
		item.StandardLimit = domain.Money(units) * match.Limit
		item.OverStandard = item.Requested > item.StandardLimit
		if (item.OverStandard || item.TravelException) && item.ExceptionReason == "" {
			return itemPolicyFailure(i, "exception_reason", "EXCEPTION_REASON_REQUIRED", "超标或差旅异常必须说明原因")
		}
		if len(item.InvoiceUsages) == 0 && (!match.AllowNoInvoice || item.NoInvoiceReason == "") {
			return itemPolicyFailure(i, "no_invoice_reason", "INVOICE_REQUIRED", "此标准不允许无票费用或缺少理由")
		}
		if e = applySpecialExpensePolicy(item, *match, i); e != nil {
			return e
		}
		if e = applyItemProofPolicy(item, *match, i); e != nil {
			return e
		}
	}
	return nil
}

// validateProfiles 固定财务主体及已审核的本人收款资料版本。
// 参数：tx 为事务，c 为待提交单据。
// 返回值：配置不存在或越组织使用错误。
func validateProfiles(tx *gorm.DB, c *domain.Claim) error {
	var f, p store.Config
	if e := tx.Where("tenant_id = ? AND id = ? AND kind = ? AND published = ?", c.TenantID, c.FinanceEntityID, "finance_entities", true).Where("id NOT IN (?)", tx.Model(&store.PolicyLifecycle{}).Select("config_id").Where("tenant_id = ?", c.TenantID)).First(&f).Error; e != nil {
		return domain.Fail("FINANCE_ENTITY_INVALID", "报销财务主体尚未有效配置", 422)
	}
	var entity domain.FinanceEntity
	if e := json.Unmarshal([]byte(f.ContentJSON), &entity); e != nil {
		return e
	}
	allowed := false
	for _, org := range entity.OrgIDs {
		if org == c.OrgID {
			allowed = true
		}
	}
	if !entity.Active || !allowed {
		return domain.Fail("FINANCE_ENTITY_INVALID", "财务主体不允许此费用组织", 422)
	}
	if e := tx.Where("tenant_id = ? AND id = ? AND kind = ? AND published = ?", c.TenantID, c.PayeeProfileID, "payee_profiles", true).Where("id NOT IN (?)", tx.Model(&store.PolicyLifecycle{}).Select("config_id").Where("tenant_id = ?", c.TenantID)).First(&p).Error; e != nil {
		return domain.Fail("PAYEE_PROFILE_INVALID", "收款资料尚未有效审核", 422)
	}
	var profile domain.PayeeProfile
	if e := json.Unmarshal([]byte(p.ContentJSON), &profile); e != nil {
		return e
	}
	if !profile.Active || !profile.Reviewed || profile.SubjectID != c.Applicant || profile.AccountRef == "" {
		return domain.Fail("PAYEE_PROFILE_INVALID", "只能使用已审核的本人有效收款资料", 422)
	}
	c.PayeeVersion = profile.Version
	c.FinanceEntitySnapshot = &entity
	c.PayeeProfileSnapshot = &profile
	return nil
}

// reserveInvoices 在稳定锁顺序下原子占用票据并复核总使用额。
// 参数：tx 为事务，c 为本次已重算费用。
// 返回值：复核、买方、使用额或并发抢占错误。
func reserveInvoices(tx *gorm.DB, c *domain.Claim) error {
	return validateClaimInvoices(tx, c, true)
}

// validateClaimInvoices 用途：复用正式票据校验，预览分支只读取事实。
// 参数：tx 为业务数据库，c 为重算草稿，reserve 决定是否实际锁定和占用票据。
// 返回值：票据、日期、主体或并发校验错误。
func validateClaimInvoices(tx *gorm.DB, c *domain.Claim, reserve bool) error {
	c.InvoiceDateWarnings = nil
	checkedAt := time.Now().UTC()
	used := map[string]domain.Money{}
	for _, item := range c.Items {
		for _, u := range item.InvoiceUsages {
			used[u.InvoiceID] += u.Used
		}
	}
	var entityRow store.Config
	if e := tx.Where("tenant_id = ? AND id = ?", c.TenantID, c.FinanceEntityID).First(&entityRow).Error; e != nil {
		return e
	}
	var entity domain.FinanceEntity
	if e := json.Unmarshal([]byte(entityRow.ContentJSON), &entity); e != nil {
		return e
	}
	consumedChecked := false
	for _, id := range invoiceIDs(*c) {
		var row store.Invoice
		query := tx.Where("tenant_id = ? AND id = ?", c.TenantID, id)
		if reserve {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if e := query.First(&row).Error; e != nil {
			return domain.Fail("INVOICE_NOT_AVAILABLE", "发票不存在或无权使用", 422)
		}
		if row.Owner != c.Applicant {
			return domain.Fail("INVOICE_NOT_AVAILABLE", "发票不存在或无权使用", 422)
		}
		var inv domain.Invoice
		if e := json.Unmarshal([]byte(row.ContentJSON), &inv); e != nil {
			return e
		}
		if err := invoiceUseStateFailure(row, inv, used[id]); err != nil {
			return invoiceUsageFailure(*c, id, err)
		}
		rule, ruleErr := invoiceRule(tx, c.TenantID, inv.InvoiceType)
		if ruleErr != nil {
			return ruleErr
		}
		warning, dateErr := invoiceDateWarning(inv, rule, checkedAt)
		if dateErr != nil {
			return invoiceUsageFailure(*c, id, dateErr)
		}
		if warning != nil {
			c.InvoiceDateWarnings = append(c.InvoiceDateWarnings, *warning)
		}
		if rule.RequiredExternalVerification && inv.VerificationStatus != "VERIFIED" {
			return invoiceUsageFailure(*c, id, domain.Fail("INVOICE_EXTERNAL_VERIFICATION_REQUIRED", "此票种政策要求正式外部查验结果，请先取得有效查验结果后再提交", 422))
		}
		if (rule.RequiredBuyerTaxID && inv.BuyerTaxID == "") || (inv.BuyerTaxID != "" && inv.BuyerTaxID != entity.TaxID) {
			return invoiceUsageFailure(*c, id, domain.Fail("INVOICE_BUYER_MISMATCH", "发票购买方与报销主体不匹配，请核对票面购买方及本单财务主体", 422))
		}
		if row.ReservationClaimID != "" && row.ReservationClaimID != c.ID {
			return invoiceUsageFailure(*c, id, domain.Fail("INVOICE_RESERVED", "发票已被其他申请占用，不能跨单拆票；请核对本人其他申请或更换可用票据", 409))
		}
		if row.ReservationState == "CONSUMED" && !consumedChecked {
			if e := consumedEvidenceReapproval(tx, *c); e != nil {
				return e
			}
			consumedChecked = true
		}
		if !reserve {
			continue
		}
		r := tx.Model(&store.Invoice{}).Where("tenant_id = ? AND id = ? AND version = ?", c.TenantID, id, row.Version).Updates(map[string]any{"reservation_claim_id": c.ID, "reservation_state": nextReservationState(row.ReservationState), "version": row.Version + 1})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return invoiceUsageFailure(*c, id, domain.Fail("INVOICE_RESERVED", "发票状态已被并发更新，请刷新票据状态后核对使用情况", 409))
		}
	}
	if !reserve {
		return nil
	}
	obsolete := tx.Model(&store.Invoice{}).Where("tenant_id = ? AND reservation_claim_id = ? AND reservation_state = ?", c.TenantID, c.ID, "RESERVED")
	ids := invoiceIDs(*c)
	if len(ids) > 0 {
		obsolete = obsolete.Where("id NOT IN ?", ids)
	}
	if e := obsolete.Updates(map[string]any{"reservation_claim_id": "", "reservation_state": "AVAILABLE", "version": gorm.Expr("version + 1")}).Error; e != nil {
		return e
	}
	return nil
}

// assignmentSubjects 解析当前业务日有效的明确角色负责人。
// 参数：tx 为事务，tenant/org/project/role 为配置定位边界。
// 返回值：去重受理人；缺失配置时报错。
func assignmentSubjects(tx *gorm.DB, tenant, org, project, role string) ([]string, error) {
	rows, e := configs(tx, tenant, "assignments", true)
	if e != nil {
		return nil, e
	}
	day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	set := map[string]bool{}
	for _, row := range rows {
		var a domain.Assignment
		if e = json.Unmarshal([]byte(row.ContentJSON), &a); e != nil {
			return nil, e
		}
		if a.Role == role && a.OrgID == org && a.ProjectID == project && day >= a.ValidFrom && (a.ValidUntil == "" || day <= a.ValidUntil) {
			set[a.SubjectID] = true
		}
	}
	out := []string{}
	for id := range set {
		if id != "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, domain.Fail("APPROVER_MISSING", "缺少当前组织与角色的有效负责人映射", 422)
	}
	return out, nil
}

// buildApprovalPlan 生成服务端固定技术或销售路线与多负责人 ALL 任务。
// 参数：tx 为事务，c 为单据，refs 为冻结项目责任引用。
// 返回值：有版本路线、各任务分摊范围或缺失受理人错误。
func buildApprovalPlan(tx *gorm.DB, c domain.Claim, refs []domain.ProjectReference) (approvalPlan, error) {
	rule, e := routeRule(tx, c)
	if e != nil {
		return approvalPlan{}, e
	}
	plan := approvalPlan{References: refs, RuleVersion: rule.ID}
	decision := evaluateRouteThresholds(c, rule)
	plan.RouteDecision = &decision
	for _, item := range c.Items {
		plan.HistoricalPolicies = append(plan.HistoricalPolicies, item.HistoricalTailApplications...)
	}
	baseRole := "PROJECT_EXPENSE_OWNER"
	if c.RouteType == "SALES" {
		baseRole = "SALES_DIRECTOR"
	}
	leadKeys, e := projectRoleKeys(c, refs, baseRole)
	if e != nil {
		return plan, e
	}
	if e := addNode(&plan, c, 1, "BUSINESS_REVIEW", "expense.approval.act", leadKeys); e != nil {
		return plan, e
	}
	node := 2
	financeNode := false
	if decision.Triggered {
		role := rule.ExtraRole
		financeNode = role == "FINANCE_DIRECTOR"
		keys := map[string][]string{}
		if role == "TECHNICAL_DIRECTOR" {
			keys, e = projectRoleKeys(c, refs, role)
		} else {
			var subjects []string
			subjects, e = assignmentSubjects(tx, c.TenantID, c.OrgID, "", role)
			for _, sub := range subjects {
				keys[sub] = []string{"*"}
			}
		}
		if e != nil {
			return plan, e
		}
		if e = addNode(&plan, c, node, role, "expense.approval.act", keys); e != nil {
			return plan, e
		}
		node++
	}
	if len(plan.HistoricalPolicies) > 0 && !financeNode {
		subjects, err := assignmentSubjects(tx, c.TenantID, c.OrgID, "", "FINANCE_DIRECTOR")
		if err != nil {
			return plan, err
		}
		keys := map[string][]string{}
		for _, subject := range subjects {
			keys[subject] = []string{"*"}
		}
		if err = addNode(&plan, c, node, "FINANCE_DIRECTOR", "expense.approval.act", keys); err != nil {
			return plan, err
		}
		node++
	}
	subjects, e := assignmentSubjects(tx, c.TenantID, c.OrgID, "", rule.AccountingRole)
	if e != nil {
		return plan, e
	}
	keys := map[string][]string{}
	for _, sub := range subjects {
		keys[sub] = []string{"*"}
	}
	e = addNode(&plan, c, node, "ACCOUNTANT", "expense.finance.review", keys)
	return plan, e
}

// addNode 创建禁止自审的 ALL 节点任务。
// 参数：plan 为路线，c 为单据，node/code/permission 为节点语义，subjects 为各受理人范围。
// 返回值：职责冲突或标识生成错误。
func addNode(plan *approvalPlan, c domain.Claim, node int, code, permission string, subjects map[string][]string) error {
	ids := []string{}
	for subject := range subjects {
		ids = append(ids, subject)
	}
	sort.Strings(ids)
	for _, subject := range ids {
		if subject == c.Applicant {
			return domain.Fail("SELF_APPROVAL_FORBIDDEN", "审批人与申请人重叠，请配置独立审批人", 422)
		}
		id, e := newID()
		if e != nil {
			return e
		}
		status := "WAITING"
		if node == 1 {
			status = "PENDING"
		}
		role := code
		if code == "BUSINESS_REVIEW" {
			role = "PROJECT_EXPENSE_OWNER"
			if c.RouteType == "SALES" {
				role = "SALES_DIRECTOR"
			}
		}
		plan.Tasks = append(plan.Tasks, domain.Task{NodeRole: role, ID: id, ClaimID: c.ID, Round: c.CurrentRound, Node: node, NodeCode: code, Assignee: subject, Status: status, Version: 1, AllocationKeys: subjects[subject], RequiredPermission: permission})
	}
	return nil
}

// allocateNumber 在本地事务中按北京业务年分配租户内编号。
// 参数：tx 为事务，tenant 为租户。
// 返回值：唯一业务单号或数据库错误。
func allocateNumber(tx *gorm.DB, tenant string) (string, error) {
	year := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Year()
	key := fmt.Sprintf("%s:%d", tenant, year)
	if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.Sequence{ID: key, Value: 0}).Error; e != nil {
		return "", e
	}
	var row store.Sequence
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", key).Error; e != nil {
		return "", e
	}
	r := tx.Model(&store.Sequence{}).Where("id = ? AND value = ?", key, row.Value).Update("value", row.Value+1)
	if r.Error != nil {
		return "", r.Error
	}
	if r.RowsAffected != 1 {
		return "", domain.Fail("VERSION_CONFLICT", "编号并发变化，请以同一命令重试", 409)
	}
	return fmt.Sprintf("BX-%d-%06d", year, row.Value+1), nil
}

// routeRule 冻结发生于提交日的结构化审批线路政策版本。
// 参数：tx 为事务，c 为租户、组织和业务线路。
// 返回值：唯一有效路线或配置冲突错误。
func routeRule(tx *gorm.DB, c domain.Claim) (domain.RouteRule, error) {
	rows, e := configs(tx, c.TenantID, "route_rules", true)
	if e != nil {
		return domain.RouteRule{}, e
	}
	day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	var found *domain.RouteRule
	for _, row := range rows {
		rule, err := decodeRouteRule(row.ContentJSON)
		if err != nil {
			return rule, err
		}
		if rule.RouteType == c.RouteType && rule.OrgID == c.OrgID && day >= rule.ValidFrom && (rule.ValidUntil == "" || day <= rule.ValidUntil) {
			if found != nil {
				return rule, domain.Fail("ROUTE_RULE_CONFLICT", "同组织线路存在多个有效审批规则", 422)
			}
			copy := rule
			copy.ID = row.ID
			copy.Version = row.Version
			found = &copy
		}
	}
	if found == nil {
		return domain.RouteRule{}, domain.Fail("ROUTE_RULE_MISSING", "请先发布当前组织线路的审批规则", 422)
	}
	return *found, nil
}

type approvalSnapshot struct {
	Claim    domain.Claim     `json:"claim"`
	Approval approvalPlan     `json:"approval"`
	Invoices []domain.Invoice `json:"invoices"`
}

// snapshotInvoices 冻结人工确认后的票面版本、来源和使用权，不依赖以后修改的实时台账。
// 参数：tx 为提交事务，c 为已原子占票的单据。
// 返回值：稳定顺序的确认票据快照或读取错误。
func snapshotInvoices(tx *gorm.DB, c domain.Claim) ([]domain.Invoice, error) {
	out := []domain.Invoice{}
	for _, id := range invoiceIDs(c) {
		var row store.Invoice
		if e := tx.Where("tenant_id = ? AND id = ?", c.TenantID, id).First(&row).Error; e != nil {
			return nil, e
		}
		var invoice domain.Invoice
		if e := json.Unmarshal([]byte(row.ContentJSON), &invoice); e != nil {
			return nil, e
		}
		invoice.ReservationClaimID = row.ReservationClaimID
		invoice.ReservationState = row.ReservationState
		out = append(out, invoice)
	}
	return out, nil
}

// approvedContentHash 仅摘要批准内容，排除付款和审批进度等可变运行字段。
// 参数：c 为费用与收款快照，plan 为冻结路线，invoices 为确认票面版本。
// 返回值：可以从不可变快照复算的批准内容摘要。
func approvedContentHash(c domain.Claim, plan approvalPlan, invoices []domain.Invoice) (string, error) {
	c.Status = ""
	c.Version = 0
	c.ContentHash = ""
	c.ActivePaymentID = ""
	c.ProjectionRevision = 0
	c.CreatedAt = time.Time{}
	c.UpdatedAt = time.Time{}
	return digest(approvalSnapshot{Claim: c, Approval: plan, Invoices: invoices})
}

// VerifyApprovalSnapshot 为归档和审计独立复算不可变批准内容摘要。
// 参数：raw 为完整冻结快照，expectedHash 为账本保存的批准内容摘要。
// 返回值：内容被改变、快照缺失或摘要不符时返回错误。
func VerifyApprovalSnapshot(raw json.RawMessage, expectedHash string) error {
	var snapshot approvalSnapshot
	if e := json.Unmarshal(raw, &snapshot); e != nil {
		return domain.Fail("APPROVAL_SNAPSHOT_INVALID", "审批快照格式不完整", 409)
	}
	if snapshot.Claim.ID == "" || snapshot.Claim.CurrentRound < 1 || expectedHash == "" {
		return domain.Fail("APPROVAL_SNAPSHOT_INVALID", "审批快照缺少关键内容", 409)
	}
	hash, e := approvedContentHash(snapshot.Claim, snapshot.Approval, snapshot.Invoices)
	if e != nil {
		return e
	}
	if hash != expectedHash || snapshot.Claim.ContentHash != expectedHash {
		return domain.Fail("APPROVAL_SNAPSHOT_MISMATCH", "审批快照摘要与不可变内容不一致", 409)
	}
	return nil
}

// nextReservationState 保留已消费证据归属，普通新占用只进入保留状态。
// 参数：current 为原单据证据状态。
// 返回值：提交后的当前证据使用状态。
func nextReservationState(current string) string {
	if current == "CONSUMED" {
		return current
	}
	return "RESERVED"
}
