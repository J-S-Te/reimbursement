package application

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"sort"
	"strconv"
	"time"
)

// pageArgs 将客户端分页限制在有限范围。
// 参数：params 为查询参数。
// 返回值：页码、页大小和偏移。
func pageArgs(params map[string]string) (int, int, int) {
	page, _ := strconv.Atoi(params["page"])
	size, _ := strconv.Atoi(params["page_size"])
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	if page > 100000 {
		page = 100000
	}
	return page, size, (page - 1) * size
}

// scopeOrganizations 读取显式已发布财务范围，不从平台管理员角色推导。
// 参数：tx 为数据库，a 为当前主体。
// 返回值：组织集合、租户全量标志及错误。
func scopeOrganizations(tx *gorm.DB, a domain.Actor) ([]string, bool, error) {
	if a.TenantID != "" && a.SubjectID != "" && a.Has("expense.scope.tenant_all") {
		return nil, true, nil
	}
	rows, e := configs(tx, a.TenantID, "scopes", true)
	if e != nil {
		return nil, false, e
	}
	orgs := []string{}
	for _, row := range rows {
		var scope domain.Scope
		if e = json.Unmarshal([]byte(row.ContentJSON), &scope); e != nil {
			return nil, false, e
		}
		if scope.SubjectID != a.SubjectID {
			continue
		}
		if scope.TenantAll {
			return nil, true, nil
		}
		orgs = append(orgs, scope.OrgIDs...)
	}
	return orgs, false, nil
}

// readableClaims 构造服务端对象范围过滤，SELF 是缺省范围。
// 参数：db 为当前查询，a 为身份，tasks 表示可包含本人任务覆盖单据。
// 返回值：带租户与范围条件的查询或配置错误。
func readableClaims(db *gorm.DB, a domain.Actor, tasks bool) (*gorm.DB, error) {
	q := db.Model(&store.Claim{}).Where("tenant_id = ?", a.TenantID)
	orgs, all, e := scopeOrganizations(db, a)
	if e != nil {
		return nil, e
	}
	finance := hasFinancialReadAbility(a)
	if finance && all {
		return q, nil
	}
	conditions := db.Where("applicant = ?", a.SubjectID)
	if finance && len(orgs) > 0 {
		conditions = conditions.Or("org_id IN ?", orgs)
	}
	if tasks && a.Has("expense.approval.act") {
		sub := db.Model(&store.Task{}).Select("claim_id").Where("tenant_id = ? AND assignee = ?", a.TenantID, a.SubjectID)
		conditions = conditions.Or("id IN (?)", sub)
	}
	return q.Where(conditions), nil
}

// claimView 仅返回受理人实际覆盖的分摊和必要公共字段。
// 参数：tx 为数据库，a 为身份，c 为单据内容。
// 返回值：完整或最小范围单据及错误。
func (s *Service) claimView(tx *gorm.DB, a domain.Actor, c domain.Claim) (any, error) {
	full, e := s.fullRead(tx, a, c)
	if e != nil {
		return nil, e
	}
	actions := []string{}
	if c.Applicant == a.SubjectID {
		if (c.Status == "DRAFT" || c.Status == "RETURNED") && a.Has("expense.claim.submit") {
			actions = append(actions, "SUBMIT")
		}
		if c.Status == "IN_APPROVAL" && a.Has("expense.claim.withdraw") {
			actions = append(actions, "WITHDRAW")
		}
		if (c.Status == "DRAFT" || c.Status == "RETURNED") && a.Has("expense.claim.void") {
			actions = append(actions, "VOID")
		}
	}
	if full && c.Status == "APPROVED_PENDING_PAYMENT" && c.ActivePaymentID == "" && a.SubjectID != c.Applicant {
		if a.Has("expense.claim.reopen") {
			actions = append(actions, "REOPEN")
		}
		if a.Has("expense.claim.approved_void") {
			actions = append(actions, "APPROVED_VOID")
		}
	}
	if full {
		raw, e := encode(c)
		if e != nil {
			return nil, e
		}
		var result map[string]any
		if e = json.Unmarshal([]byte(raw), &result); e != nil {
			return nil, e
		}
		result["allowed_actions"] = actions
		return enrichClaimQueryView(tx, a, c, result)
	}
	if e = s.canRead(tx, a, c); e != nil {
		return nil, e
	}
	var rows []store.Task
	if e = tx.Where("tenant_id = ? AND claim_id = ? AND round = ? AND assignee = ?", c.TenantID, c.ID, c.CurrentRound, a.SubjectID).Find(&rows).Error; e != nil {
		return nil, e
	}
	keys := map[string]bool{}
	for _, row := range rows {
		var task domain.Task
		if e = json.Unmarshal([]byte(row.ContentJSON), &task); e != nil {
			return nil, e
		}
		for _, key := range task.AllocationKeys {
			keys[key] = true
		}
	}
	items := []domain.Item{}
	total := domain.Money(0)
	for _, item := range c.Items {
		selected := []domain.Allocation{}
		amount := domain.Money(0)
		for _, allocation := range item.Allocations {
			if keys["*"] || keys[item.ID+"/"+allocation.ProjectID+"/"+allocation.ServiceItemID] {
				// 原比例以完整费用为分母，局部金额与该比例组合会反推出隐藏金额。
				allocation.RatioBasisPoints = 0
				selected = append(selected, allocation)
				amount += allocation.Amount
			}
		}
		if len(selected) > 0 {
			item.Allocations = selected
			item.AllocationMode = ""
			item.AllocationRuleVersion = ""
			item.TravelDeclaration = nil
			item.Requested = amount
			item.InvoiceUsages = nil
			// 归类依据含完整票面及用票标识，局部分摊读取不能扩大到整票。
			item.ClassificationEvidence = nil
			item.Reason = ""
			item.ExceptionReason = ""
			item.NoInvoiceReason = ""
			item.HistoricalTailReason = ""
			item.HistoricalTailApplications = scopedTailApplications(item.HistoricalTailApplications, selected)
			item.HistoricalTailEvidenceFileIDs = scopedEvidenceIDs(item.HistoricalTailEvidenceFileIDs, c, keys)
			if item.TravelConfirmation != nil {
				confirmation := *item.TravelConfirmation
				confirmation.Reason = ""
				confirmation.EvidenceFileIDs = scopedEvidenceIDs(confirmation.EvidenceFileIDs, c, keys)
				item.TravelConfirmation = &confirmation
			}
			if item.TravelAssessment != nil {
				item.TravelAssessment = scopedTravelAssessmentForQuery(*item.TravelAssessment, selected)
			}
			item.Grade = ""
			item.StandardVersionID = ""
			item.CityLevel = ""
			item.StandardLimit = 0
			items = append(items, item)
			total += amount
		}
	}
	return enrichClaimQueryView(tx, a, c, map[string]any{"id": c.ID, "claim_no": c.ClaimNo, "title": c.Title, "route_type": c.RouteType, "status": c.Status, "version": c.Version, "current_round": c.CurrentRound, "currency": c.Currency, "payable_minor": total, "items": items, "scope_limited": true, "allowed_actions": actions})
}

// scopedTravelAssessmentForQuery 隐藏局部受理人无权读取的整项声明及用票交叉来源。
// 参数：assessment为完整冻结结论，allocations为当前主体可见项目分摊。
// 返回值：仅含可见项目的投影；交叉依据被裁剪时明确标记范围受限UNKNOWN。
func scopedTravelAssessmentForQuery(assessment domain.TravelAssessment,
	allocations []domain.Allocation) *domain.TravelAssessment {
	projected := scopedTravelAssessment(assessment, allocations)
	redacted := false
	for i := range projected.References {
		ref := &projected.References[i]
		if ref.CrossCheck == nil {
			continue
		}
		// 单项目引用中的CrossCheck仍包含整项所有票据，不能只按ProjectID过滤。
		ref.CrossCheck = nil
		ref.Status = "UNKNOWN"
		ref.ReasonCodes = []string{"SCOPE_LIMITED_TRAVEL_DETAILS"}
		redacted = true
	}
	if !redacted {
		return projected
	}
	returnAssessment := aggregateTravelAssessment(projected.References)
	return &returnAssessment
}

// Query 读取当前授权范围中的业务对象，所有列表均分页。
// 参数：ctx 为上下文，a 为可信身份，kind/id 为查询目标，params 为有限筛选条件。
// 返回值：响应 data JSON 或公开业务错误。
func (s *Service) Query(ctx context.Context, a domain.Actor, kind, id string, params map[string]string) (json.RawMessage, error) {
	if a.TenantID == "" || a.SubjectID == "" {
		return nil, domain.Fail("SESSION_EXPIRED", "请重新登录", 401)
	}
	db := s.db.WithContext(ctx)
	var value any
	var e error
	switch kind {
	case "payment-exception", "payment-exceptions":
		value, e = s.paymentExceptionQuery(db, a, kind, id, params)
	case "claim-options":
		value, e = s.claimOptions(db, a)
	case "claim":
		if e = require(a, "expense.claim.read"); e != nil {
			break
		}
		var c domain.Claim
		c, _, e = loadClaim(db, a.TenantID, id, false)
		if e == nil {
			value, e = s.claimView(db, a, c)
		}
	case "claims":
		value, e = s.listClaims(db, a, params)
	case "approval-history":
		value, e = s.approvalHistory(db, a, id)
	case "approval-tasks", "approval-task":
		value, e = s.listTasks(db, a, id, params)
	case "invoices", "invoice":
		value, e = s.listInvoices(db, a, id, params)
	case "parse-exceptions":
		value, e = s.parseExceptions(db, a, params)
	case "configurations":
		if !a.Has("expense.config.manage") && !a.Has("expense.policy.publish") {
			e = domain.Fail("FORBIDDEN", "无配置查阅权限", 403)
		}
		if e == nil {
			value, e = listConfigs(db, a, id, params)
		}
	case "commands":
		value, e = s.commandResult(db, a, id, params["operation"])
	case "dashboard":
		value, e = s.dashboard(db, a)
	case "payment", "payments", "reversal-requests", "archives":
		value, e = s.financialQuery(db, a, kind, id, params)
	case "documents", "jobs", "exports":
		value, e = s.resourceQuery(db, a, kind, id, params)
	default:
		e = domain.Fail("RESOURCE_NOT_FOUND", "查询接口不存在", 404)
	}
	if e != nil {
		return nil, e
	}
	raw, e := json.Marshal(value)
	return json.RawMessage(raw), e
}

// listClaims 在数据库范围过滤后分页，避免越界读取后再隐藏。
// 参数：db 为数据库，a 为身份，params 为合法组合筛选和分页。
// 返回值：授权单据页或错误。
func (s *Service) listClaims(db *gorm.DB, a domain.Actor, params map[string]string) (any, error) {
	if e := require(a, "expense.claim.read"); e != nil {
		return nil, e
	}
	q, e := readableClaims(db, a, true)
	if e != nil {
		return nil, e
	}
	filters, e := parseQueryFilters(params, "claims")
	if e != nil {
		return nil, e
	}
	q, e = applyClaimFilters(db, q, a, filters)
	if e != nil {
		return nil, e
	}
	if params["mine"] == "true" {
		q = q.Where("applicant = ?", a.SubjectID)
	}
	var total int64
	if e = q.Count(&total).Error; e != nil {
		return nil, e
	}
	page, size, offset := pageArgs(params)
	var rows []store.Claim
	if e = q.Order("created_at DESC, id DESC").Limit(size).Offset(offset).Find(&rows).Error; e != nil {
		return nil, e
	}
	items := []any{}
	for _, row := range rows {
		var c domain.Claim
		if e = json.Unmarshal([]byte(row.ContentJSON), &c); e != nil {
			return nil, e
		}
		if c.CreatedAt.IsZero() {
			c.CreatedAt = row.CreatedAt
		}
		view, err := s.claimView(db, a, c)
		if err != nil {
			return nil, err
		}
		items = append(items, view)
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}, nil
}

// listTasks 返回本人任务及按责任范围裁剪的固定审批快照。
// 参数：db 为数据库，a 为身份，id 可定位单任务，params 为任务状态、单据条件和分页。
// 返回值：任务详情或任务页。
func (s *Service) listTasks(db *gorm.DB, a domain.Actor, id string, params map[string]string) (any, error) {
	if e := require(a, "expense.approval.act"); e != nil {
		return nil, e
	}
	filters, e := parseQueryFilters(params, "tasks")
	if e != nil {
		return nil, e
	}
	q := db.Where("tenant_id = ? AND assignee = ?", a.TenantID, a.SubjectID)
	if id != "" {
		q = q.Where("id = ?", id)
	}
	if filters.Status != "" {
		q = q.Where("status = ?", filters.Status)
	}
	// 任务状态属于任务自身，其余条件按受控单据范围筛选。
	filters.Status = ""
	claims, e := readableClaims(db, a, true)
	if e != nil {
		return nil, e
	}
	claims, e = applyClaimFilters(db, claims, a, filters)
	if e != nil {
		return nil, e
	}
	q = q.Where("claim_id IN (?)", claims.Select("id"))
	var total int64
	if e := q.Model(&store.Task{}).Count(&total).Error; e != nil {
		return nil, e
	}
	page, size, offset := pageArgs(params)
	var rows []store.Task
	if e := q.Order("created_at DESC").Limit(size).Offset(offset).Find(&rows).Error; e != nil {
		return nil, e
	}
	items := []any{}
	for _, row := range rows {
		var task domain.Task
		if e := json.Unmarshal([]byte(row.ContentJSON), &task); e != nil {
			return nil, e
		}
		task.Status = row.Status
		task.Version = row.Version
		task.Assignee = row.Assignee
		c, _, e := loadClaim(db, a.TenantID, row.ClaimID, false)
		if e != nil {
			return nil, e
		}
		raw, e := encode(task)
		if e != nil {
			return nil, e
		}
		var item map[string]any
		if e = json.Unmarshal([]byte(raw), &item); e != nil {
			return nil, e
		}
		item["created_at"] = row.CreatedAt
		item["claim_version"] = c.Version
		item["route_type"] = c.RouteType
		enrichTaskUrgency(item, row, c, time.Now().UTC())
		actions := []string{}
		if row.Status == "PENDING" && row.Round == c.CurrentRound && c.Status == "IN_APPROVAL" && a.Has(task.RequiredPermission) && a.SubjectID != c.Applicant {
			actions = []string{"APPROVE", "RETURN"}
			if a.Has("expense.approval.transfer") {
				actions = append(actions, "TRANSFER", "ADDSIGN")
			}
		}
		item["allowed_actions"] = actions
		var snapshot store.Snapshot
		if e = db.Where("tenant_id = ? AND claim_id = ? AND round = ?", a.TenantID, c.ID, row.Round).First(&snapshot).Error; e != nil {
			return nil, e
		}
		var content struct {
			Claim domain.Claim `json:"claim"`
		}
		if e = json.Unmarshal([]byte(snapshot.ContentJSON), &content); e != nil {
			return nil, e
		}
		view, e := s.claimView(db, a, content.Claim)
		if e != nil {
			return nil, e
		}
		item["snapshot"] = view
		items = append(items, item)
	}
	if id != "" {
		if len(items) == 0 {
			return nil, domain.Fail("RESOURCE_NOT_FOUND", "任务不存在或无权访问", 404)
		}
		return items[0], nil
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}, nil
}

// listInvoices 隔离票据归属，任务受理人不会自动获得共享原件。
// 参数：db 为数据库，a 为身份，id 为可选票据，params 为票面条件、关联单据条件和分页。
// 返回值：票据详情或台账。
func (s *Service) listInvoices(db *gorm.DB, a domain.Actor, id string, params map[string]string) (any, error) {
	if e := require(a, "expense.invoice.read"); e != nil {
		return nil, e
	}
	filters, e := parseQueryFilters(params, "invoices")
	if e != nil {
		return nil, e
	}
	claims, e := readableClaims(db, a, false)
	if e != nil {
		return nil, e
	}
	q := readableInvoiceRows(db, a, claims)
	if id != "" {
		q = q.Where("id = ?", id)
	}
	q = applyInvoiceFilters(db, q, filters)
	if filters.Project != "" || filters.ClaimType != "" {
		claimFilters := queryFilters{Project: filters.Project, ClaimType: filters.ClaimType}
		linked, e := applyClaimFilters(db, claims, a, claimFilters)
		if e != nil {
			return nil, e
		}
		q = q.Where("reservation_claim_id IN (?)", linked.Select("id"))
	}
	page, size, offset := pageArgs(params)
	var total int64
	if e := q.Count(&total).Error; e != nil {
		return nil, e
	}
	var rows []store.Invoice
	if e := q.Order("created_at DESC").Limit(size).Offset(offset).Find(&rows).Error; e != nil {
		return nil, e
	}
	items := []any{}
	for _, row := range rows {
		var inv domain.Invoice
		if e := json.Unmarshal([]byte(row.ContentJSON), &inv); e != nil {
			return nil, e
		}
		inv.Version = row.Version
		inv.ReservationClaimID = row.ReservationClaimID
		inv.ReservationState = row.ReservationState
		raw, e := encode(inv)
		if e != nil {
			return nil, e
		}
		var view map[string]any
		if e = json.Unmarshal([]byte(raw), &view); e != nil {
			return nil, e
		}
		used := domain.Money(0)
		if row.ReservationClaimID != "" {
			c, _, err := loadClaim(db, a.TenantID, row.ReservationClaimID, false)
			if err != nil {
				return nil, err
			}
			for _, item := range c.Items {
				for _, u := range item.InvoiceUsages {
					if u.InvoiceID == inv.ID {
						used += u.Used
					}
				}
			}
		}
		view["created_at"] = row.CreatedAt
		view["used_minor"] = used
		view["remaining_minor"] = inv.Total - used
		view["remaining_scope"] = "CURRENT_CLAIM_ONLY"
		items = append(items, view)
	}
	if id != "" {
		if len(items) == 0 {
			return nil, domain.Fail("RESOURCE_NOT_FOUND", "发票不存在或无权访问", 404)
		}
		view := items[0].(map[string]any)
		if e := addInvoiceDuplicateWarnings(db, a, id, view); e != nil {
			return nil, e
		}
		return view, nil
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}, nil
}

// listConfigs 读取管理员可见的配置页，保留历史发布状态。
// 参数：db 为数据库，a 为身份，kind 为配置类型，params 为分页。
// 返回值：版本页或读取错误。
func listConfigs(db *gorm.DB, a domain.Actor, kind string, params map[string]string) (any, error) {
	page, size, offset := pageArgs(params)
	q := db.Model(&store.Config{}).Where("tenant_id = ? AND kind = ?", a.TenantID, kind)
	var total int64
	if e := q.Count(&total).Error; e != nil {
		return nil, e
	}
	var rows []store.Config
	if e := q.Order("created_at DESC").Limit(size).Offset(offset).Find(&rows).Error; e != nil {
		return nil, e
	}
	items := []any{}
	ids := []string{}
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	var events []store.PolicyLifecycle
	if len(ids) > 0 {
		if e := db.Where("tenant_id = ? AND config_id IN ?", a.TenantID, ids).Find(&events).Error; e != nil {
			return nil, e
		}
	}
	retired := map[string]store.PolicyLifecycle{}
	for _, event := range events {
		retired[event.ConfigID] = event
	}
	for _, row := range rows {
		var view map[string]any
		if e := json.Unmarshal([]byte(row.ContentJSON), &view); e != nil {
			return nil, e
		}
		view["content_version"] = view["version"]
		view["version"] = row.Version
		view["lifecycle_status"] = "DRAFT"
		if row.Published {
			view["lifecycle_status"] = "ACTIVE"
		}
		if event, ok := retired[row.ID]; ok {
			view["lifecycle_status"] = "RETIRED"
			view["retired_at"] = event.EffectiveAt
			view["replaced_by_id"] = event.ReplacementID
			view["retirement_reason"] = event.Reason
			view["retirement_actor_subject_id"] = event.Actor
		}
		items = append(items, view)
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}, nil
}

// commandResult 仅查询当前主体的冻结结果并重新校验可见性。
// 参数：db 为数据库，a 为主体，id 为命令标识，operation 为可选消歧动作。
// 返回值：原成功业务结果或不存在、歧义错误。
func (s *Service) commandResult(db *gorm.DB, a domain.Actor, id, operation string) (any, error) {
	q := db.Where("tenant_id = ? AND actor = ? AND command_id = ?", a.TenantID, a.SubjectID, id)
	if operation != "" {
		q = q.Where("operation = ?", operation)
	}
	var rows []store.Command
	if e := q.Limit(2).Find(&rows).Error; e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return s.QueryRecoveryCommand(db.Statement.Context, a, id)
	}
	if len(rows) > 1 {
		return nil, domain.Fail("COMMAND_OPERATION_REQUIRED", "同一标识对应多个动作，请指定operation", 409)
	}
	if e := s.resultVisible(db, a, rows[0]); e != nil {
		return nil, e
	}
	return json.RawMessage(rows[0].ResultJSON), nil
}

// dashboard 汇总当前对象范围的工作台数字。
// 参数：db 为数据库，a 为当前主体。
// 返回值：真实范围计数或权限错误。
func (s *Service) dashboard(db *gorm.DB, a domain.Actor) (any, error) {
	if e := require(a, "expense.claim.read"); e != nil {
		return nil, e
	}
	var mine, payments, review int64
	if e := db.Model(&store.Claim{}).Where("tenant_id = ? AND applicant = ?", a.TenantID, a.SubjectID).Count(&mine).Error; e != nil {
		return nil, e
	}
	pending, tasks, e := s.dashboardPendingTasks(db, a)
	if e != nil {
		return nil, e
	}
	paid, e := dashboardPaidMonth(db, a, time.Now())
	if e != nil {
		return nil, e
	}
	if a.Has("expense.payment.record") {
		q, e := readableClaims(db, a, false)
		if e != nil {
			return nil, e
		}
		if e = q.Where("status = ? AND applicant <> ?", "APPROVED_PENDING_PAYMENT", a.SubjectID).Count(&payments).Error; e != nil {
			return nil, e
		}
	}
	if a.Has("expense.invoice.read") {
		status := queryJSONText(db, "content_json", "review_status")
		if e = db.Model(&store.Invoice{}).Where("tenant_id = ? AND owner = ?", a.TenantID, a.SubjectID).Where("COALESCE("+status+", '') <> ?", "CONFIRMED").Count(&review).Error; e != nil {
			return nil, e
		}
	}
	return map[string]any{"my_claims": mine, "pending_approvals": pending, "pending_payments": payments, "invoice_review_required": review, "my_paid_this_month": paid, "pending_tasks": tasks}, nil
}

// financialQuery 在整单数据范围内查询付款、更正与归档账本。
// 参数：db 为数据库，a 为身份，kind/id 为目标，params 为筛选参数。
// 返回值：受控账本或错误。
func (s *Service) financialQuery(db *gorm.DB, a domain.Actor, kind, id string, params map[string]string) (any, error) {
	permission := "expense.payment.read"
	if kind == "archives" {
		permission = "expense.archive.read"
	}
	if e := require(a, permission); e != nil {
		return nil, e
	}
	claimID := id
	var payment store.Payment
	if kind == "payment" {
		if e := db.Where("tenant_id = ? AND id = ?", a.TenantID, id).First(&payment).Error; e != nil {
			return nil, domain.Fail("RESOURCE_NOT_FOUND", "付款不存在或无权访问", 404)
		}
		claimID = payment.ClaimID
	}
	if claimID == "" {
		claimID = params["claim_id"]
	}
	if claimID == "" {
		return nil, domain.Fail("CLAIM_ID_REQUIRED", "请指定需要查看的报销单", 422)
	}
	c, _, e := loadClaim(db, a.TenantID, claimID, false)
	if e != nil {
		return nil, e
	}
	full, e := s.fullRead(db, a, c)
	if e != nil {
		return nil, e
	}
	if !full {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "没有完整财务账本访问权限", 404)
	}
	if kind == "payment" {
		return json.RawMessage(payment.ContentJSON), nil
	}
	page, size, offset := pageArgs(params)
	items := []any{}
	var total int64
	switch kind {
	case "payments":
		var rows []store.Payment
		q := db.Model(&store.Payment{}).Where("tenant_id = ? AND claim_id = ?", a.TenantID, c.ID)
		if e = q.Count(&total).Error; e != nil {
			return nil, e
		}
		if e = q.Order("created_at DESC").Limit(size).Offset(offset).Find(&rows).Error; e != nil {
			return nil, e
		}
		for _, row := range rows {
			items = append(items, json.RawMessage(row.ContentJSON))
		}
	case "reversal-requests":
		var rows []store.ReversalRequest
		q := db.Model(&store.ReversalRequest{}).Where("tenant_id = ? AND claim_id = ?", a.TenantID, c.ID)
		if e = q.Count(&total).Error; e != nil {
			return nil, e
		}
		if e = q.Order("created_at DESC").Limit(size).Offset(offset).Find(&rows).Error; e != nil {
			return nil, e
		}
		for _, row := range rows {
			items = append(items, json.RawMessage(row.ContentJSON))
		}
	case "archives":
		var rows []store.Archive
		q := db.Model(&store.Archive{}).Where("tenant_id = ? AND claim_id = ?", a.TenantID, c.ID)
		if e = q.Count(&total).Error; e != nil {
			return nil, e
		}
		if e = q.Order("package_version DESC").Limit(size).Offset(offset).Find(&rows).Error; e != nil {
			return nil, e
		}
		for _, row := range rows {
			items = append(items, map[string]any{"id": row.ID, "claim_id": c.ID, "package_version": row.PackageVersion, "status": row.Status, "manifest_hash": row.ManifestHash, "manifest": json.RawMessage(row.ManifestJSON), "created_at": row.CreatedAt})
		}
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}, nil
}

// resourceQuery 读取本人上传和异步任务，返回真实状态。
// 参数：db 为数据库，a 为身份，kind/id 为资源，params 为分页。
// 返回值：文档或后台任务及错误。
func (s *Service) resourceQuery(db *gorm.DB, a domain.Actor, kind, id string, params map[string]string) (any, error) {
	if kind == "documents" {
		var row store.Document
		e := db.Where("tenant_id = ? AND id = ? AND owner = ?", a.TenantID, id, a.SubjectID).First(&row).Error
		if e != nil {
			return nil, domain.Fail("RESOURCE_NOT_FOUND", "文档不存在或无权访问", 404)
		}
		result := map[string]any{"id": row.ID, "document_id": row.ID, "file_id": row.FileID, "status": row.Status, "version": row.Version, "detail": json.RawMessage(row.ContentJSON)}
		var run store.InvoiceParseRun
		runErr := db.Where("tenant_id = ? AND document_id = ?", a.TenantID, row.ID).Order("created_at DESC, attempt DESC, id DESC").First(&run).Error
		if runErr == nil {
			result["parse_run_id"] = run.ID
		} else if runErr != gorm.ErrRecordNotFound {
			return nil, runErr
		}
		return result, nil
	}
	var row store.Job
	if e := db.Where("tenant_id = ? AND id = ?", a.TenantID, id).First(&row).Error; e != nil {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "任务不存在或无权访问", 404)
	}
	var input struct {
		Actor domain.Actor `json:"actor"`
	}
	if e := json.Unmarshal([]byte(row.PayloadJSON), &input); e != nil {
		return nil, e
	}
	if kind == "exports" {
		if e := require(a, "expense.archive.export"); e != nil {
			return nil, e
		}
		var exported struct {
			Result struct {
				ClaimIDs []string `json:"claim_ids"`
				Content  string   `json:"content"`
			} `json:"result"`
		}
		if e := json.Unmarshal([]byte(row.PayloadJSON), &exported); e != nil {
			return nil, e
		}
		if row.Status == "DONE" && len(exported.Result.ClaimIDs) == 0 && exported.Result.Content != "" {
			return nil, domain.Fail("EXPORT_SCOPE_UNKNOWN", "导出缺少完整范围证据，请重新生成", 409)
		}
		for _, claimID := range exported.Result.ClaimIDs {
			claim, _, e := loadClaim(db, a.TenantID, claimID, false)
			if e != nil {
				return nil, e
			}
			allowed, e := s.fullRead(db, a, claim)
			if e != nil {
				return nil, e
			}
			if !allowed {
				return nil, domain.Fail("FORBIDDEN", "导出中部分单据的当前授权已撤销", 403)
			}
		}
		if input.Actor.SubjectID != a.SubjectID {
			return nil, domain.Fail("RESOURCE_NOT_FOUND", "导出任务不存在", 404)
		}
	} else {
		var doc store.Document
		if e := db.Where("tenant_id = ? AND id = ? AND owner = ?", a.TenantID, row.ResourceID, a.SubjectID).First(&doc).Error; e != nil {
			return nil, domain.Fail("RESOURCE_NOT_FOUND", "任务不存在", 404)
		}
	}
	var output any = json.RawMessage(row.PayloadJSON)
	if kind == "exports" {
		var data map[string]json.RawMessage
		if e := json.Unmarshal([]byte(row.PayloadJSON), &data); e != nil {
			return nil, e
		}
		if value, ok := data["result"]; ok {
			output = value
		} else {
			output = nil
		}
	}
	return map[string]any{"id": row.ID, "status": row.Status, "attempt": row.Attempt, "version": row.Version, "last_error": row.LastError, "result": output}, nil
}

// claimOptions 提供本人组织可使用的财务主体、收款资料和费用标准。
// 参数：db 为数据库，a 为当前申请人。
// 返回值：最小业务选项或权限错误。
func (s *Service) claimOptions(db *gorm.DB, a domain.Actor) (any, error) {
	if !a.Has("expense.claim.create") && !a.Has("expense.claim.update") {
		return nil, domain.Fail("FORBIDDEN", "没有草稿操作权限", 403)
	}
	organizations := map[string]bool{}
	employeeRows, err := configs(db, a.TenantID, "employee_profiles", true)
	if err != nil {
		return nil, err
	}
	for _, row := range employeeRows {
		var p domain.EmployeeProfile
		if e := json.Unmarshal([]byte(row.ContentJSON), &p); e != nil {
			return nil, e
		}
		if p.SubjectID == a.SubjectID && p.Active {
			organizations[p.OrgID] = true
		}
	}
	standards := []domain.Standard{}
	tails := []domain.HistoricalTailPolicy{}
	entities := []domain.FinanceEntity{}
	payees := []domain.PayeeProfile{}
	profiles := []domain.EmployeeProfile{}
	fees := map[string]bool{}
	for _, kind := range []string{"finance_entities", "payee_profiles", "employee_profiles", "standards", "historical_tail_policies"} {
		rows, e := configs(db, a.TenantID, kind, true)
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			switch kind {
			case "finance_entities":
				var entity domain.FinanceEntity
				if e = json.Unmarshal([]byte(row.ContentJSON), &entity); e != nil {
					return nil, e
				}
				allowed := false
				for _, org := range entity.OrgIDs {
					allowed = allowed || organizations[org]
				}
				if entity.Active && allowed {
					entities = append(entities, entity)
				}
			case "payee_profiles":
				var profile domain.PayeeProfile
				if e = json.Unmarshal([]byte(row.ContentJSON), &profile); e != nil {
					return nil, e
				}
				if profile.Active && profile.Reviewed && profile.SubjectID == a.SubjectID {
					payees = append(payees, profile)
				}
			case "employee_profiles":
				var profile domain.EmployeeProfile
				if e = json.Unmarshal([]byte(row.ContentJSON), &profile); e != nil {
					return nil, e
				}
				if profile.Active && profile.SubjectID == a.SubjectID && organizations[profile.OrgID] {
					profiles = append(profiles, profile)
				}
			case "historical_tail_policies":
				var policy domain.HistoricalTailPolicy
				if e = json.Unmarshal([]byte(row.ContentJSON), &policy); e != nil {
					return nil, e
				}
				if organizations[policy.OrgID] {
					tails = append(tails, policy)
				}
			case "standards":
				var standard domain.Standard
				if e = json.Unmarshal([]byte(row.ContentJSON), &standard); e != nil {
					return nil, e
				}
				if organizations[standard.OrgID] {
					fees[standard.FeeType] = true
					standards = append(standards, standard)
				}
			}
		}
	}
	types := []string{}
	for kind := range fees {
		types = append(types, kind)
	}
	return map[string]any{"finance_entities": entities, "payee_profiles": payees, "employee_profiles": profiles, "fee_types": types, "standards": standards, "historical_tail_policies": tails, "org_ids": mapKeys(organizations)}, nil
}

// ProjectOptions 仅以当前会话主体代理授权项目搜索。
// 参数：ctx 为请求上下文，a 为可信主体，keyword/cursor 为搜索，size 为页大小。
// 返回值：项目权威最小结果或依赖错误。
func (s *Service) ProjectOptions(ctx context.Context, a domain.Actor, keyword, cursor string, size int) (json.RawMessage, error) {
	if e := require(a, "expense.project.reference"); e != nil {
		return nil, e
	}
	if s.deps.Projects == nil {
		return nil, domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "项目引用服务尚未配置", 503)
	}
	if size < 1 {
		size = 20
	}
	if size > 50 {
		size = 50
	}
	return s.deps.Projects.Search(ctx, a, keyword, cursor, size)
}

// ValidateProjects 复核本人草稿的权威项目资格，不信任浏览器的 actor 或租户字段。
// 参数：ctx 为上下文，a 为可信身份，claimID/version 为当前草稿。
// 返回值：仅供展示的引用结果，提交时仍重新复核。
func (s *Service) ValidateProjects(ctx context.Context, a domain.Actor, claimID string, version int64) (json.RawMessage, error) {
	if e := require(a, "expense.project.reference"); e != nil {
		return nil, e
	}
	c, _, e := loadClaim(s.db.WithContext(ctx), a.TenantID, claimID, false)
	if e != nil {
		return nil, e
	}
	if c.Applicant != a.SubjectID {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "草稿不存在", 404)
	}
	if e = verifyVersion(c.Version, version); e != nil {
		return nil, e
	}
	if s.deps.Projects == nil {
		return nil, domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "项目引用服务尚未配置", 503)
	}
	refs, e := s.deps.Projects.Validate(ctx, a, c.ID, c.Version, claimReferences(c))
	if e != nil {
		return nil, e
	}
	return json.Marshal(map[string]any{"items": refs})
}

// DownloadInvoice 对票据原件实施当前归属和权限复核后签发网关票据。
// 参数：ctx 为上下文，a 为当前主体，invoiceID/fileID 为票据及其已绑定原件。
// 返回值：一次性网关下载响应。
func (s *Service) DownloadInvoice(ctx context.Context, a domain.Actor, invoiceID, fileID string) (json.RawMessage, error) {
	if e := require(a, "expense.invoice.read"); e != nil {
		return nil, e
	}
	var row store.Invoice
	if e := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", a.TenantID, invoiceID).First(&row).Error; e != nil {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "发票不存在", 404)
	}
	allowed := row.Owner == a.SubjectID || a.Has("expense.scope.tenant_all")
	if !allowed && row.ReservationClaimID != "" {
		c, _, e := loadClaim(s.db.WithContext(ctx), a.TenantID, row.ReservationClaimID, false)
		if e != nil {
			return nil, e
		}
		allowed, e = s.fullRead(s.db.WithContext(ctx), a, c)
		if e != nil {
			return nil, e
		}
	}
	if !allowed {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "没有共享票据原件访问权", 404)
	}
	var invoice domain.Invoice
	if e := json.Unmarshal([]byte(row.ContentJSON), &invoice); e != nil {
		return nil, e
	}
	found := false
	for _, id := range invoice.FileIDs {
		found = found || id == fileID
	}
	if !found {
		var count int64
		err := s.db.WithContext(ctx).Model(&store.InvoiceSource{}).Joins("JOIN exp_invoice_document doc ON doc.id = exp_invoice_source.document_id AND doc.tenant_id = exp_invoice_source.tenant_id").Where("exp_invoice_source.tenant_id = ? AND exp_invoice_source.invoice_id = ? AND doc.file_id = ?", a.TenantID, invoiceID, fileID).Count(&count).Error
		if err != nil {
			return nil, err
		}
		found = count > 0
	}
	if !found {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "原件未与此发票绑定", 404)
	}
	if s.deps.Files == nil {
		return nil, domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件服务尚未配置", 503)
	}
	return s.deps.Files.Download(ctx, a, fileID)
}

// mapKeys 生成组织范围选项的稳定列表。
// 参数：values 为允许组织集合。
// 返回值：组织标识列表。
func mapKeys(values map[string]bool) []string {
	out := []string{}
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
