package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type financeRecheckProof struct {
	TaskID       string            `json:"task_id"`
	TaskVersion  int64             `json:"task_version"`
	ClaimVersion int64             `json:"claim_version"`
	Round        int               `json:"round"`
	ContentHash  string            `json:"content_hash"`
	Invoices     map[string]string `json:"invoice_facts"`
	VerifiedAt   time.Time         `json:"verified_at"`
}

type financeComplianceIssue struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	ItemID    string `json:"item_id,omitempty"`
	InvoiceID string `json:"invoice_id,omitempty"`
	Facts     any    `json:"facts,omitempty"`
}

type financeRecheckResult struct {
	SnapshotHash string                   `json:"snapshot_hash"`
	Issues       []financeComplianceIssue `json:"issues"`
	Proof        *financeRecheckProof     `json:"external_preflight"`
}

// preflightFinanceRecheck 仅为当前会计通过动作核验人员和文件依赖，并绑定已核事实。
// 参数：ctx 为外部请求边界，db 为事务外只读库，cc/input 为原始审批命令。
// 返回值：权限、版本或依赖失败；不会写入业务事实或将故障变成退回。
func (s *Service) preflightFinanceRecheck(ctx context.Context, db *gorm.DB,
	cc *commandContext, input ApprovalInput) error {
	if input.Action != "APPROVE" {
		return nil
	}
	var task store.Task
	err := db.Where("tenant_id = ? AND id = ? AND assignee = ?", cc.actor.TenantID, cc.resourceID, cc.actor.SubjectID).First(&task).Error
	if err != nil {
		return err
	}
	var dto domain.Task
	if err = json.Unmarshal([]byte(task.ContentJSON), &dto); err != nil {
		return err
	}
	if err = financeTaskIdentity(task, dto); err != nil {
		return err
	}
	if dto.NodeCode != "ACCOUNTANT" {
		return nil
	}
	c, _, err := loadClaim(db, cc.actor.TenantID, task.ClaimID, false)
	if err != nil {
		return err
	}
	if task.Assignee == c.Applicant {
		return domain.Fail("FORBIDDEN", "申请人不能自行办理财务复核", 403)
	}
	if err = require(cc.actor, dto.RequiredPermission); err != nil {
		return err
	}
	if err = requireFinancial(db, cc.actor, c, "expense.finance.review"); err != nil {
		return err
	}
	stale := task.Version != input.TaskVersion || c.Version != input.ExpectedVersion ||
		task.Round != input.Round || task.Round != c.CurrentRound ||
		task.Status != "PENDING" || c.Status != "IN_APPROVAL"
	if stale {
		return domain.Fail("STALE_APPROVAL_TASK", "任务版本或状态已改变", 409)
	}
	if _, err = financeApprovalSnapshot(db, c); err != nil {
		return err
	}
	if s.deps.Personnel == nil {
		return domain.Fail("PERSONNEL_DIRECTORY_UNAVAILABLE", "财务人员目录未配置", 503)
	}
	verifiedAt := time.Now().UTC()
	if err = s.deps.Personnel.VerifyApprover(ctx, cc.actor, task.Assignee, "expense.finance.review"); err != nil {
		return err
	}
	if err = s.validateClaimEvidence(ctx, c); err != nil {
		return err
	}
	proof := &financeRecheckProof{
		TaskID: task.ID, TaskVersion: task.Version, ClaimVersion: c.Version,
		Round: c.CurrentRound, ContentHash: c.ContentHash,
		Invoices: map[string]string{}, VerifiedAt: verifiedAt,
	}
	for _, id := range invoiceIDs(c) {
		var row store.Invoice
		err = db.Where("tenant_id = ? AND id = ?", c.TenantID, id).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			proof.Invoices[id] = "MISSING"
			continue
		}
		if err != nil {
			return err
		}
		proof.Invoices[id], err = digest(row)
		if err != nil {
			return err
		}
		var inv domain.Invoice
		if err = json.Unmarshal([]byte(row.ContentJSON), &inv); err != nil {
			return err
		}
		if row.Owner != c.Applicant || len(inv.FileIDs) == 0 {
			continue
		}
		if s.deps.Files == nil {
			return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "发票原件服务未配置", 503)
		}
		owner := domain.Actor{TenantID: c.TenantID, SubjectID: c.Applicant}
		if err = s.deps.Files.Validate(ctx, owner, inv.FileIDs, "reimbursement.invoice.original"); err != nil {
			return err
		}
	}
	cc.finance = proof
	return nil
}

// financeTaskIdentity 拒绝会计任务索引与冻结语义不一致导致的复检绕过。
// 参数：row 为当前任务账本，task 为冻结节点内容；受理人与状态允许受控变化。
// 返回值：会计节点身份或固定能力损坏时返回409，普通节点不增加语义。
func financeTaskIdentity(row store.Task, task domain.Task) error {
	if row.NodeCode != "ACCOUNTANT" && task.NodeCode != "ACCOUNTANT" {
		return nil
	}
	if row.ID != task.ID || row.ClaimID != task.ClaimID || row.Round != task.Round ||
		row.Node != task.Node || row.NodeCode != task.NodeCode ||
		task.RequiredPermission != "expense.finance.review" {
		return domain.Fail("APPROVAL_TASK_INVALID", "会计任务账本与冻结节点身份不一致", 409)
	}
	return nil
}

// financeApprovalSnapshot 验证真实本轮快照及聚合批准内容，拒绝损坏或串单账本。
// 参数：db 为当前读取或命令事务，c 为可信租户内当前单据。
// 返回值：完整冻结批准事实；缺失、摘要或身份不一致返回409，存储故障原样返回。
func financeApprovalSnapshot(db *gorm.DB, c domain.Claim) (approvalSnapshot, error) {
	var row store.Snapshot
	var snapshot approvalSnapshot
	err := db.Where("tenant_id = ? AND claim_id = ? AND round = ?", c.TenantID, c.ID, c.CurrentRound).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return snapshot, domain.Fail("APPROVAL_SNAPSHOT_INVALID", "本轮审批快照缺失", 409)
	}
	if err != nil {
		return snapshot, err
	}
	if row.ContentHash != c.ContentHash {
		return snapshot, domain.Fail("APPROVAL_SNAPSHOT_MISMATCH", "本轮批准摘要不一致", 409)
	}
	if err = VerifyApprovalSnapshot(json.RawMessage(row.ContentJSON), row.ContentHash); err != nil {
		return snapshot, err
	}
	if err = json.Unmarshal([]byte(row.ContentJSON), &snapshot); err != nil {
		return snapshot, err
	}
	if snapshot.Claim.ID != c.ID || snapshot.Claim.TenantID != c.TenantID || snapshot.Claim.CurrentRound != c.CurrentRound {
		return snapshot, domain.Fail("APPROVAL_SNAPSHOT_MISMATCH", "审批快照身份或轮次不一致", 409)
	}
	hash, err := approvedContentHash(c, snapshot.Approval, snapshot.Invoices)
	if err != nil {
		return snapshot, err
	}
	if hash != c.ContentHash {
		return snapshot, domain.Fail("APPROVAL_SNAPSHOT_MISMATCH", "当前批准内容偏离冻结快照", 409)
	}
	return snapshot, nil
}

// financeRecheck 在权限和版本通过后复验冻结标准及当前票据事实。
// 参数：tx 为持有单据锁的事务，cc 为外部证明，c/task 为锁定审批状态。
// 返回值：确定性合规问题；证明过期、快照损坏或存储错误直接中止命令。
func financeRecheck(tx *gorm.DB, cc *commandContext, c domain.Claim,
	task store.Task) (*financeRecheckResult, error) {
	p := cc.finance
	if p == nil || p.TaskID != task.ID || p.TaskVersion != task.Version ||
		p.ClaimVersion != c.Version || p.Round != c.CurrentRound ||
		p.ContentHash != c.ContentHash || time.Since(p.VerifiedAt) < 0 ||
		time.Since(p.VerifiedAt) > time.Minute {
		return nil, domain.Fail("FINANCE_RECHECK_STALE", "财务复检证明缺失或过期，请刷新后重试", 409)
	}
	snapshot, err := financeApprovalSnapshot(tx, c)
	if err != nil {
		return nil, err
	}
	result := &financeRecheckResult{
		SnapshotHash: c.ContentHash, Issues: []financeComplianceIssue{}, Proof: p,
	}
	copy := snapshot.Claim
	if err = domain.ValidateClaim(&copy); err != nil {
		if err = result.businessIssue(err, "", ""); err != nil {
			return nil, err
		}
	}
	if copy.Payable != c.Payable {
		result.Issues = append(result.Issues, financeComplianceIssue{Code: "CLAIM_AMOUNT_MISMATCH", Message: "费用总额与冻结应付金额不一致"})
	}
	if err = result.frozenStandards(tx, c); err != nil {
		return nil, err
	}
	if err = result.currentInvoices(tx, c, snapshot.Invoices); err != nil {
		return nil, err
	}
	return result, nil
}

// businessIssue 只将明确白名单业务校验转换为合规问题，其他错误保持失败关闭。
// 参数：err 为真实业务校验结果，itemID/invoiceID 为可追溯对象。
// 返回值：白名单问题追加后为空；权限、故障和未知错误原样返回。
func (r *financeRecheckResult) businessIssue(err error, itemID, invoiceID string) error {
	if err == nil {
		return nil
	}
	var p *domain.Problem
	if !errors.As(err, &p) {
		return err
	}
	switch p.Code {
	case "INVALID_CLAIM", "INVALID_ITEM", "INVALID_DATE", "ALLOCATION_MISMATCH", "INVOICE_COVERAGE_MISMATCH",
		"NO_INVOICE_PROOF_REQUIRED", "PREPAYMENT_NOT_ALLOWED", "PREPAYMENT_REASON_REQUIRED", "PREPAYMENT_EVIDENCE_REQUIRED",
		"INVOICE_RISK_REVIEW_REQUIRED", "INVOICE_RECONFIRMATION_REQUIRED":
		r.Issues = append(r.Issues, financeComplianceIssue{Code: p.Code, Message: p.Message, ItemID: itemID, InvoiceID: invoiceID})
		return nil
	}
	return err
}

// frozenStandards 按提交采用的不可变标准标识复算金额和无票政策。
// 参数：tx 为命令事务，c 为已验证冻结内容的当前单据。
// 返回值：真实政策不合规追加证据；冻结配置损坏或缺失中止命令。
func (r *financeRecheckResult) frozenStandards(tx *gorm.DB, c domain.Claim) error {
	for i, item := range c.Items {
		var row store.Config
		err := tx.Where("tenant_id = ? AND id = ? AND kind = ?", c.TenantID, item.StandardVersionID, "standards").First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.Fail("FROZEN_STANDARD_INVALID", "提交采用的标准版本缺失，须核对账本", 409)
		}
		if err != nil {
			return err
		}
		var standard domain.Standard
		if err = json.Unmarshal([]byte(row.ContentJSON), &standard); err != nil {
			return err
		}
		// 遗留快照尚无版本字段，仍按发布后不可改的行ID及冻结金额核对原政策。
		versionMismatch := item.StandardPolicyVersion != 0 &&
			standard.Version != item.StandardPolicyVersion
		if !row.Published || standard.ID != item.StandardVersionID || versionMismatch {
			return domain.Fail("FROZEN_STANDARD_INVALID", "提交采用的不可变标准版本不一致", 409)
		}
		if standard.Limit <= 0 {
			return domain.Fail("FROZEN_STANDARD_INVALID", "原已发布标准金额损坏，须核对账本", 409)
		}
		if item.Units <= 0 || item.Units > math.MaxInt64/int64(standard.Limit) {
			r.Issues = append(r.Issues, financeComplianceIssue{Code: "STANDARD_AMOUNT_INVALID", Message: "冻结标准计量数量或金额不合法", ItemID: item.ID})
			continue
		}
		limit := domain.Money(item.Units) * standard.Limit
		if limit != item.StandardLimit || item.OverStandard != (item.Requested > limit) {
			return domain.Fail("FROZEN_STANDARD_INVALID", "冻结标准金额与原政策不一致，须核对账本", 409)
		}
		if (item.Requested > limit || item.TravelException) && strings.TrimSpace(item.ExceptionReason) == "" {
			r.Issues = append(r.Issues, financeComplianceIssue{Code: "EXCEPTION_REASON_REQUIRED", Message: "超标准或差旅异常缺少说明", ItemID: item.ID, Facts: map[string]any{"standard_id": standard.ID, "standard_version": standard.Version, "limit_minor": limit}})
		}
		if len(item.InvoiceUsages) == 0 && (!standard.AllowNoInvoice || strings.TrimSpace(item.NoInvoiceReason) == "") {
			r.Issues = append(r.Issues, financeComplianceIssue{Code: "INVOICE_REQUIRED", Message: "冻结标准不允许此无票费用", ItemID: item.ID})
		}
		if err = r.businessIssue(applyItemProofPolicy(&item, standard, i), item.ID, ""); err != nil {
			return err
		}
		if err = r.businessIssue(applySpecialExpensePolicy(&item, standard, i), item.ID, ""); err != nil {
			return err
		}
	}
	return nil
}

// currentInvoices 锁定已外部复核票据并检查当前控制、核验和唯一占用。
// 参数：tx 为单据锁内事务，c 为冻结单据，frozen 为提交时票面。
// 返回值：确定性风险写入结果；外部核验后事实改变或存储异常中止命令。
func (r *financeRecheckResult) currentInvoices(tx *gorm.DB, c domain.Claim,
	frozen []domain.Invoice) error {
	used := map[string]domain.Money{}
	original := map[string]domain.Invoice{}
	for _, inv := range frozen {
		original[inv.ID] = inv
	}
	for _, item := range c.Items {
		for _, usage := range item.InvoiceUsages {
			if usage.Used <= 0 || used[usage.InvoiceID] > domain.Money(math.MaxInt64)-usage.Used {
				return domain.Fail("APPROVAL_SNAPSHOT_INVALID", "冻结票据使用金额不合法", 409)
			}
			used[usage.InvoiceID] += usage.Used
		}
	}
	for _, id := range invoiceIDs(c) {
		var row store.Invoice
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", c.TenantID, id).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if r.Proof.Invoices[id] != "MISSING" {
				return domain.Fail("FINANCE_RECHECK_STALE", "外部复核后票据状态已改变", 409)
			}
			r.Issues = append(r.Issues, financeComplianceIssue{Code: "INVOICE_NOT_AVAILABLE", Message: "关联票据已不存在", InvoiceID: id})
			continue
		}
		if err != nil {
			return err
		}
		hash, err := digest(row)
		if err != nil {
			return err
		}
		if hash != r.Proof.Invoices[id] {
			return domain.Fail("FINANCE_RECHECK_STALE", "外部复核后票据版本已改变", 409)
		}
		var inv domain.Invoice
		if err = json.Unmarshal([]byte(row.ContentJSON), &inv); err != nil {
			return err
		}
		before, ok := original[id]
		if !ok || inv.ID != id || inv.TenantID != c.TenantID {
			return domain.Fail("APPROVAL_SNAPSHOT_INVALID", "票据身份与批准快照不一致", 409)
		}
		r.invoiceFacts(c, row, inv, before, used[id])
		if err = r.controlFacts(tx, c, inv); err != nil {
			return err
		}
	}
	return nil
}

// controlFacts 以当前锁定读复核重大更正和风险，并保存触发案件的真实标识及状态。
// 参数：tx 为持有申请与票据锁的事务，c 为冻结申请，inv 为当前票面。
// 返回值：读取或历史超限错误；确定业务问题追加到可审计结论。
func (r *financeRecheckResult) controlFacts(tx *gorm.DB, c domain.Claim, inv domain.Invoice) error {
	// 保留当前锁定读，同时让控制表的每个查询独立克隆语句，避免跨表复用条件。
	current := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Session(&gorm.Session{})
	impacts, risks, err := invoiceControlRecords(current, c.TenantID, inv.ID)
	if err != nil {
		return err
	}
	for _, risk := range risks {
		if risk.Status == "CLOSED" && risk.Resolution == "DISMISSED" {
			continue
		}
		r.Issues = append(r.Issues, financeComplianceIssue{
			Code: "INVOICE_RISK_REVIEW_REQUIRED", InvoiceID: inv.ID,
			Message: "关联票据存在尚未解除的红字、作废、退款或财务复核风险", Facts: risk,
		})
	}
	for _, impact := range impacts {
		if impact.ClaimID != c.ID || impact.Status == "REJECTED" || impact.Status == "RESOLVED" {
			continue
		}
		if impact.Status == "RECONFIRMED" && c.CurrentRound > impact.OriginalRound && inv.Version == impact.InvoiceVersion {
			continue
		}
		r.Issues = append(r.Issues, financeComplianceIssue{
			Code: "INVOICE_RECONFIRMATION_REQUIRED", InvoiceID: inv.ID,
			Message: "票据重大更正须由申请人重新确认并完成新轮次审批", Facts: impact,
		})
	}
	return nil
}

// invoiceFacts 将当前票据的确定性业务问题保存为可审计事实。
// 参数：c 为冻结申请，row/inv 为当前票据，before 为原票面，used 为本单累计用额。
// 返回值：无。
func (r *financeRecheckResult) invoiceFacts(c domain.Claim, row store.Invoice,
	inv, before domain.Invoice, used domain.Money) {
	add := func(code, message string) {
		r.Issues = append(r.Issues, financeComplianceIssue{
			Code: code, Message: message, InvoiceID: row.ID,
			Facts: map[string]any{
				"invoice_version": row.Version, "content_version": inv.Version,
				"verification_status":  inv.VerificationStatus,
				"reservation_claim_id": row.ReservationClaimID,
				"reservation_state":    row.ReservationState,
				"used_minor":           used, "total_minor": inv.Total,
			},
		})
	}
	unavailable := row.Owner != c.Applicant || inv.Owner != c.Applicant ||
		inv.ReviewStatus != "CONFIRMED" || row.IdentityKey == nil ||
		*row.IdentityKey == "" || inv.IdentityKey != *row.IdentityKey ||
		inv.MergedIntoInvoiceID != ""
	if unavailable {
		add("INVOICE_NOT_AVAILABLE", "票据未确认、唯一身份或本人使用权不符合要求")
	}
	if inv.VerificationStatus == "MISMATCH" || (before.VerificationStatus == "VERIFIED" && inv.VerificationStatus != "VERIFIED") {
		add("INVOICE_VERIFICATION_INVALID", "票据当前核验不一致或已失去原正式核验结果")
	}
	if inv.Total <= 0 || used > inv.Total {
		add("INVOICE_COVERAGE_MISMATCH", "本单票据使用额超过当前票面金额")
	}
	if len(inv.FileIDs) == 0 {
		add("INVOICE_ORIGINAL_REQUIRED", "票据缺少可复核原件")
	}
	if c.FinanceEntitySnapshot != nil && inv.BuyerTaxID != "" && inv.BuyerTaxID != c.FinanceEntitySnapshot.TaxID {
		add("INVOICE_BUYER_MISMATCH", "当前票据购买方与冻结财务主体不一致")
	}
	if row.ReservationClaimID != c.ID || (row.ReservationState != "RESERVED" && row.ReservationState != "CONSUMED") {
		add("INVOICE_RESERVATION_INVALID", "票据没有由本申请唯一占用")
	}
	faceChanged := inv.IdentityKey != before.IdentityKey || inv.Total != before.Total ||
		inv.BuyerTaxID != before.BuyerTaxID || inv.InvoiceNumber != before.InvoiceNumber ||
		inv.InvoiceCode != before.InvoiceCode || inv.InvoiceDate != before.InvoiceDate ||
		inv.InvoiceType != before.InvoiceType || inv.SellerTaxID != before.SellerTaxID ||
		!sameInvoiceTravelFacts(inv.TravelFacts, before.TravelFacts) ||
		!sameInvoiceContent(inv.Content, before.Content)
	if faceChanged {
		add("INVOICE_RECONFIRMATION_REQUIRED", "当前票面已偏离提交时批准内容，须申请人重确认后重新审批")
	}
}

// complianceSummary 生成稳定去重错误码和有界中文退回原因。
// 参数：无。
// 返回值：确定性错误码集合及可供申请人处理的原因。
func (r *financeRecheckResult) complianceSummary() ([]string, string) {
	codes := []string{}
	messages := []string{}
	seen := map[string]bool{}
	for _, issue := range r.Issues {
		if seen[issue.Code] {
			continue
		}
		seen[issue.Code] = true
		codes = append(codes, issue.Code)
		messages = append(messages, issue.Message)
	}
	reason := fmt.Sprintf("会计合规复检自动退回：%s", strings.Join(messages, "；"))
	if len([]rune(reason)) > 600 {
		reason = string([]rune(reason)[:600])
	}
	return codes, reason
}
