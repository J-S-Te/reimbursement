package application

import (
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
	"strconv"
	"time"
)

type PaymentInput struct {
	CommandInput
	Amount           domain.Money `json:"amount_minor"`
	Currency         string       `json:"currency"`
	PaidDate         string       `json:"paid_date"`
	Method           string       `json:"method"`
	SourceAccountRef string       `json:"source_account_ref"`
	TransactionNo    string       `json:"transaction_no"`
	PayeeProfileID   string       `json:"payee_profile_id"`
	VoucherFileIDs   []string     `json:"voucher_file_ids"`
}
type PaymentResult struct {
	ProjectReferenceCheck []domain.ProjectReference `json:"project_reference_check,omitempty"`
	ID                    string                    `json:"id"`
	ClaimID               string                    `json:"claim_id"`
	ClaimVersion          int64                     `json:"claim_version"`
	Amount                domain.Money              `json:"amount_minor"`
	Currency              string                    `json:"currency"`
	PaidDate              string                    `json:"paid_date"`
	Method                string                    `json:"method"`
	SourceAccountRef      string                    `json:"source_account_ref"`
	TransactionNo         string                    `json:"transaction_no"`
	PayeeProfileID        string                    `json:"payee_profile_id"`
	PayeeVersion          int64                     `json:"payee_version"`
	VoucherFileIDs        []string                  `json:"voucher_file_ids"`
	ContentHash           string                    `json:"approved_content_hash"`
	Status                string                    `json:"status"`
}
type ReversalInput struct {
	CommandInput
	Reason              string   `json:"reason"`
	FactKind            string   `json:"fact_kind"`
	EvidenceFileIDs     []string `json:"evidence_file_ids"`
	TerminateObligation bool     `json:"terminate_obligation"`
	Approve             bool     `json:"approve"`
}
type ReversalData struct {
	ID                  string   `json:"id"`
	ClaimID             string   `json:"claim_id"`
	PaymentID           string   `json:"payment_id"`
	Version             int64    `json:"version"`
	Requester           string   `json:"requester_subject_id"`
	Approver            string   `json:"approver_subject_id,omitempty"`
	Status              string   `json:"status"`
	Reason              string   `json:"reason"`
	FactKind            string   `json:"fact_kind"`
	EvidenceFileIDs     []string `json:"evidence_file_ids"`
	TerminateObligation bool     `json:"terminate_obligation"`
	ClaimVersion        int64    `json:"claim_version"`
}

// recordPayment 用途：原子登记唯一有效全额付款及完整项目支出快照。
// 参数：tx 为事务，cc 为独立出纳命令，payload 为线下付款事实。
// 返回值：冻结付款回执、单据与错误。
func (s *Service) recordPayment(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input PaymentInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	c, _, e := loadClaim(tx, cc.actor.TenantID, cc.resourceID, true)
	if e != nil {
		return nil, "", e
	}
	if e = requireFinancial(tx, cc.actor, c, "expense.payment.record"); e != nil {
		return nil, "", e
	}
	if e = verifyVersion(c.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if c.Status != "APPROVED_PENDING_PAYMENT" || c.ActivePaymentID != "" {
		return nil, "", domain.Fail("STATE_CONFLICT", "单据并非待付款或已有有效付款", 409)
	}
	// 持有单据锁后、写任何付款事实前复核票据风险闭环与重大更正确认，未闭环一律阻断出款。
	if e = VerifyInvoiceControls(tx, c, "PAYMENT"); e != nil {
		return nil, "", e
	}
	if c.Applicant == cc.actor.SubjectID {
		return nil, "", domain.Fail("SELF_PAYMENT_FORBIDDEN", "申请人不能为本人登记付款", 403)
	}
	var accountants int64
	if e = tx.Model(&store.Action{}).Joins("JOIN exp_approval_task ON exp_approval_task.id = exp_approval_action.task_id AND exp_approval_task.tenant_id = exp_approval_action.tenant_id").Where("exp_approval_action.tenant_id = ? AND exp_approval_action.claim_id = ? AND exp_approval_action.round = ? AND exp_approval_action.actor = ? AND exp_approval_action.action = ? AND exp_approval_task.node_code = ?", c.TenantID, c.ID, c.CurrentRound, cc.actor.SubjectID, "APPROVE", "ACCOUNTANT").Count(&accountants).Error; e != nil {
		return nil, "", e
	}
	if accountants > 0 {
		return nil, "", domain.Fail("ACCOUNTANT_CASHIER_CONFLICT", "会计审核人与付款登记人必须独立", 403)
	}
	var snapshot store.Snapshot
	if e = tx.Where("tenant_id = ? AND claim_id = ? AND round = ?", c.TenantID, c.ID, c.CurrentRound).First(&snapshot).Error; e != nil {
		return nil, "", e
	}
	var frozen approvalSnapshot
	if e = json.Unmarshal([]byte(snapshot.ContentJSON), &frozen); e != nil {
		return nil, "", e
	}
	currentHash, err := approvedContentHash(c, frozen.Approval, frozen.Invoices)
	if err != nil {
		return nil, "", err
	}
	frozenHash, err := approvedContentHash(frozen.Claim, frozen.Approval, frozen.Invoices)
	if err != nil {
		return nil, "", err
	}
	if currentHash != snapshot.ContentHash || frozenHash != snapshot.ContentHash {
		return nil, "", domain.Fail("APPROVAL_SNAPSHOT_MISMATCH", "费用、分摊或收款资料与批准快照不一致", 409)
	}
	if snapshot.ContentHash != c.ContentHash || c.ContentHash == "" {
		return nil, "", domain.Fail("APPROVAL_SNAPSHOT_MISMATCH", "批准内容与当前应付不一致", 409)
	}
	var incomplete int64
	if e = tx.Model(&store.Task{}).Where("tenant_id = ? AND claim_id = ? AND round = ? AND status <> ?", c.TenantID, c.ID, c.CurrentRound, "APPROVED").Count(&incomplete).Error; e != nil {
		return nil, "", e
	}
	if incomplete != 0 {
		return nil, "", domain.Fail("APPROVAL_INCOMPLETE", "审批任务尚未全部完成", 409)
	}
	if e = validatePaymentFields(input, c); e != nil {
		return nil, "", e
	}
	// 与目录退休、替换串行，防止账户校验通过后被并发停用仍落入新付款。
	if e = lockPolicyTenant(tx, c.TenantID); e != nil {
		return nil, "", e
	}
	if e = validatePaymentSource(tx, c, input.SourceAccountRef); e != nil {
		return nil, "", e
	}
	if e = validatePaymentProjectPolicies(tx, c, cc.refs); e != nil {
		return nil, "", e
	}
	if _, e = s.reservePaymentSpend(tx, c, paymentEffect(c, "00000000000000000000000000", input.PaidDate)); e != nil {
		return nil, "", e
	}
	if e = consumeInvoices(tx, c); e != nil {
		return nil, "", e
	}
	id, e := newID()
	if e != nil {
		return nil, "", e
	}
	previous := c.Version
	c.Version++
	c.Status = "PAID"
	c.ActivePaymentID = id
	c.ProjectionRevision++
	payment := PaymentResult{ProjectReferenceCheck: cc.refs, ID: id, ClaimID: c.ID, ClaimVersion: c.Version, Amount: input.Amount, Currency: c.Currency, PaidDate: input.PaidDate, Method: input.Method, SourceAccountRef: input.SourceAccountRef, TransactionNo: input.TransactionNo, PayeeProfileID: c.PayeeProfileID, PayeeVersion: c.PayeeVersion, VoucherFileIDs: input.VoucherFileIDs, ContentHash: c.ContentHash, Status: "RECORDED"}
	raw, e := encode(payment)
	if e != nil {
		return nil, "", e
	}
	row := store.Payment{ID: id, TenantID: c.TenantID, ClaimID: c.ID,
		Actor: cc.actor.SubjectID, Method: input.Method, SourceAccountRef: input.SourceAccountRef,
		TransactionNo: input.TransactionNo, ContentJSON: raw}
	if e = tx.Create(&row).Error; e != nil {
		return nil, "", e
	}
	if e = acquirePaymentBinding(tx, row); e != nil {
		return nil, "", e
	}
	effect := paymentEffect(c, id, input.PaidDate)
	if e = storeEffect(tx, c, effect); e != nil {
		return nil, "", e
	}
	if e = s.publishSpend(tx, c, "PAYMENT_RECORDED"); e != nil {
		return nil, "", e
	}
	if e = createArchiveJob(tx, c); e != nil {
		return nil, "", e
	}
	e = saveClaim(tx, &c, previous)
	return payment, c.ID, e
}

// consumeInvoices 固定当前单据证据权，合法重付可继续使用本单已消费票据。
// 参数：tx 为事务，c 为保持同一批准内容的单据。
// 返回值：票据不再属于此单据时的冲突。
func consumeInvoices(tx *gorm.DB, c domain.Claim) error {
	for _, id := range invoiceIDs(c) {
		var row store.Invoice
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", c.TenantID, id).First(&row).Error; e != nil {
			return e
		}
		var invoice domain.Invoice
		if e := json.Unmarshal([]byte(row.ContentJSON), &invoice); e != nil {
			return e
		}
		if invoice.VerificationStatus == "MISMATCH" {
			return domain.Fail("INVOICE_VERIFICATION_MISMATCH", "票据出现核验异常，请财务核实后处理", 409)
		}
		if row.ReservationClaimID != c.ID || (row.ReservationState != "RESERVED" && row.ReservationState != "CONSUMED") {
			return domain.Fail("INVOICE_RESERVATION_LOST", "票据证据权与本单据不一致", 409)
		}
		if e := tx.Model(&store.Invoice{}).Where("id = ? AND version = ?", id, row.Version).Updates(map[string]any{"reservation_state": "CONSUMED", "version": row.Version + 1}).Error; e != nil {
			return e
		}
	}
	return nil
}

// paymentEffect 依据批准分摊生成不可变付款效果。
// 参数：c 为批准单据，id 为付款记录标识，date 为实际付款业务日。
// 返回值：规范化效果及按项目费用类型合并后的分摊。
func paymentEffect(c domain.Claim, id, date string) domain.SpendEffect {
	amounts := map[string]domain.Money{}
	allocations := map[string]domain.SpendAllocation{}
	for _, item := range c.Items {
		for _, a := range item.Allocations {
			var service *string
			if a.ServiceItemID != "" {
				value := a.ServiceItemID
				service = &value
			}
			key := domain.SpendKey(a.ProjectID, service, item.FeeType)
			amounts[key] += a.Amount
			allocations[key] = domain.SpendAllocation{ProjectID: a.ProjectID, ServiceItemID: service, FeeType: item.FeeType}
		}
	}
	rows := []domain.SpendAllocation{}
	for key, a := range allocations {
		a.AmountMinor = strconv.FormatInt(int64(amounts[key]), 10)
		rows = append(rows, a)
	}
	sort.Slice(rows, func(i, j int) bool {
		return domain.SpendKey(rows[i].ProjectID, rows[i].ServiceItemID, rows[i].FeeType) < domain.SpendKey(rows[j].ProjectID, rows[j].ServiceItemID, rows[j].FeeType)
	})
	at, _ := time.Parse("2006-01-02", date)
	return domain.SpendEffect{EffectID: id, PaymentRecordID: id, Kind: "PAYMENT_RECORDED", AmountMinor: strconv.FormatInt(int64(c.Payable), 10), OccurredAt: at.UTC().Format(time.RFC3339), RecordedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339), ApprovedContentHash: c.ContentHash, Allocations: rows}
}

// storeEffect 只插入不可变效果，不以更新重写旧付款。
// 参数：tx 为事务，c 为单据，effect 为正向或反向效果。
// 返回值：唯一性或持久化错误。
func storeEffect(tx *gorm.DB, c domain.Claim, effect domain.SpendEffect) error {
	raw, e := encode(effect)
	if e != nil {
		return e
	}
	return tx.Create(&store.Effect{ID: effect.EffectID, TenantID: c.TenantID, ClaimID: c.ID, PaymentID: effect.PaymentRecordID, Kind: effect.Kind, ContentHash: domain.SpendEffectHash(effect), ContentJSON: raw}).Error
}

// publishSpend 读取完整历史效果并冻结一致的当前净额快照。
// 参数：tx 为事务，c 为新财务状态，trigger 为付款或全额冲正。
// 返回值：协议限制、金额不一致或 outbox 写入错误。
func (s *Service) publishSpend(tx *gorm.DB, c domain.Claim, trigger string) error {
	effects, e := readSpendEffects(tx, c)
	if e != nil {
		return e
	}
	event, e := newID()
	if e != nil {
		return e
	}
	snapshot, e := s.buildSpendSnapshot(c, effects, trigger, event)
	if e != nil {
		return e
	}
	if e = domain.ValidateSpendSnapshot(snapshot); e != nil {
		return domain.Fail("SPEND_SNAPSHOT_INVALID", "支出快照超限或账本不一致，登记事务已回滚", 422)
	}
	raw, e := encode(snapshot)
	if e != nil {
		return e
	}
	return tx.Create(&store.Outbox{ID: event, EventID: event, TenantID: c.TenantID, Topic: "project", ResourceID: c.ID, PayloadJSON: raw, Status: "PENDING", Version: 1, NextAttemptAt: time.Now().UTC()}).Error
}

// reversePayment 以独立批准和明确资金事实完成全额冲正。
// 参数：tx 为事务，cc 为申请或批准命令，payload 为证据或决定。
// 返回值：冻结更正回执、单据与错误。
func (s *Service) reversePayment(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input ReversalInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	var request store.ReversalRequest
	var payment store.Payment
	var e error
	if cc.operation == "reversal.request" {
		e = tx.Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&payment).Error
	} else {
		e = tx.Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&request).Error
		if e == nil {
			e = tx.Where("tenant_id = ? AND id = ?", cc.actor.TenantID, request.PaymentID).First(&payment).Error
		}
	}
	if e != nil {
		return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "付款或冲正申请不存在", 404)
	}
	c, _, e := loadClaim(tx, cc.actor.TenantID, payment.ClaimID, true)
	if e != nil {
		return nil, "", e
	}
	permission := "expense.payment.reversal.request"
	if cc.operation == "reversal.decide" {
		permission = "expense.payment.reversal.approve"
	}
	if e = requireFinancial(tx, cc.actor, c, permission); e != nil {
		return nil, "", e
	}
	if cc.actor.SubjectID == c.Applicant {
		return nil, "", domain.Fail("SELF_REVERSAL_FORBIDDEN", "申请人不能更正本人的付款事实", 403)
	}
	if cc.operation == "reversal.request" {
		if e = verifyVersion(c.Version, input.ExpectedVersion); e != nil {
			return nil, "", e
		}
		if c.Status != "PAID" || c.ActivePaymentID != payment.ID {
			return nil, "", domain.Fail("STATE_CONFLICT", "只能申请冲正当前有效付款", 409)
		}
		if input.Reason == "" || (input.FactKind != "NO_PAYMENT" && input.FactKind != "FULL_REFUND") || len(input.EvidenceFileIDs) == 0 {
			return nil, "", domain.Fail("REVERSAL_EVIDENCE_REQUIRED", "仅实际未付或已全额退款且提供证据时可以申请冲正", 422)
		}
		id, err := newID()
		if err != nil {
			return nil, "", err
		}
		data := ReversalData{ID: id, ClaimID: c.ID, PaymentID: payment.ID, Version: 1, Requester: cc.actor.SubjectID, Status: "PENDING", Reason: input.Reason, FactKind: input.FactKind, EvidenceFileIDs: input.EvidenceFileIDs, TerminateObligation: input.TerminateObligation, ClaimVersion: c.Version}
		raw, err := encode(data)
		if err != nil {
			return nil, "", err
		}
		err = tx.Create(&store.ReversalRequest{ID: id, TenantID: c.TenantID, ClaimID: c.ID, PaymentID: payment.ID, Requester: cc.actor.SubjectID, Status: "PENDING", Version: 1, ContentJSON: raw}).Error
		return data, c.ID, err
	}
	if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", c.TenantID, request.ID).First(&request).Error; e != nil {
		return nil, "", e
	}
	if e = verifyVersion(request.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if request.Status != "PENDING" {
		return nil, "", domain.Fail("STATE_CONFLICT", "此冲正申请已经处理", 409)
	}
	if request.Requester == cc.actor.SubjectID || payment.Actor == cc.actor.SubjectID {
		return nil, "", domain.Fail("REVERSAL_APPROVER_CONFLICT", "冲正批准人必须独立于申请人与原付款人", 403)
	}
	var data ReversalData
	if e = json.Unmarshal([]byte(request.ContentJSON), &data); e != nil {
		return nil, "", e
	}
	if input.Reason == "" {
		return nil, "", domain.Fail("REASON_REQUIRED", "批准或拒绝需要原因", 422)
	}
	data.Approver = cc.actor.SubjectID
	data.Version++
	data.Status = "REJECTED"
	if input.Approve {
		data.Status = "APPROVED"
		if c.ActivePaymentID != payment.ID || c.Status != "PAID" {
			return nil, "", domain.Fail("ACTIVE_PAYMENT_CONFLICT", "当前有效付款已发生变化", 409)
		}
		var original store.Effect
		if e = tx.Where("tenant_id = ? AND id = ? AND kind = ?", c.TenantID, payment.ID, "PAYMENT_RECORDED").First(&original).Error; e != nil {
			return nil, "", e
		}
		var effect domain.SpendEffect
		if e = json.Unmarshal([]byte(original.ContentJSON), &effect); e != nil {
			return nil, "", e
		}
		reverseID, err := newID()
		if err != nil {
			return nil, "", err
		}
		effect.EffectID = reverseID
		effect.ReversesEffectID = payment.ID
		effect.Kind = "PAYMENT_FULLY_REVERSED"
		effect.AmountMinor = "-" + effect.AmountMinor
		effect.OccurredAt = time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
		effect.RecordedAt = effect.OccurredAt
		for i := range effect.Allocations {
			effect.Allocations[i].AmountMinor = "-" + effect.Allocations[i].AmountMinor
		}
		if e = storeEffect(tx, c, effect); e != nil {
			return nil, "", e
		}
		reversalJSON, err := encode(map[string]any{"request": data, "effect": effect, "decision_reason": input.Reason})
		if err != nil {
			return nil, "", err
		}
		if e = tx.Create(&store.Reversal{ID: reverseID, TenantID: c.TenantID, PaymentID: payment.ID, ClaimID: c.ID, ContentJSON: reversalJSON}).Error; e != nil {
			return nil, "", e
		}
		if e = releaseNoPaymentBinding(tx, payment, data); e != nil {
			return nil, "", e
		}
		previous := c.Version
		c.Version++
		c.ProjectionRevision++
		c.ActivePaymentID = ""
		c.Status = "APPROVED_PENDING_PAYMENT"
		if data.TerminateObligation {
			c.Status = "VOID"
		}
		if e = s.publishSpend(tx, c, "PAYMENT_FULLY_REVERSED"); e != nil {
			return nil, "", e
		}
		if e = createArchiveJob(tx, c); e != nil {
			return nil, "", e
		}
		if e = saveClaim(tx, &c, previous); e != nil {
			return nil, "", e
		}
	}
	data.ClaimVersion = c.Version
	raw, e := encode(data)
	if e != nil {
		return nil, "", e
	}
	e = tx.Model(&store.ReversalRequest{}).Where("id = ? AND version = ?", request.ID, request.Version).Updates(map[string]any{"status": data.Status, "version": data.Version, "content_json": raw}).Error
	if e != nil {
		return nil, "", e
	}
	if e = appendOutbox(tx, c.TenantID, "notification", c.ID, map[string]any{
		"kind": "REVERSAL_" + data.Status, "claim_id": c.ID,
		"assignee_subject_id": request.Requester, "requester_subject_id": request.Requester,
	}); e != nil {
		return nil, "", e
	}
	return data, c.ID, nil
}

// createArchiveJob 分配独立档案版本并冻结真实财务版本、当时文档和不可变历史引用。
// 参数：tx 为已持有claim锁的事务，c 为本次实际文档状态。
// 返回值：归档请求与任务写入错误，不生成新的资金效果。
func createArchiveJob(tx *gorm.DB, c domain.Claim) error {
	return createArchiveJobFrom(tx, c, "")
}

// createArchiveJobFrom 冻结新归档请求，并显式关联受控重试的前序失败包。
// 参数：tx为持有单据锁的事务；c为当前文档；predecessor为旧失败包ID，普通请求为空。
// 返回值：后继请求与任务落库错误，旧包及资金事实保持不变。
func createArchiveJobFrom(tx *gorm.DB, c domain.Claim, predecessor string) error {
	var latest int64
	if e := tx.Model(&store.Archive{}).Select("COALESCE(MAX(package_version),0)").Where("tenant_id = ? AND claim_id = ?", c.TenantID, c.ID).Scan(&latest).Error; e != nil {
		return e
	}
	if latest == int64(^uint64(0)>>1) {
		return domain.Fail("ARCHIVE_VERSION_LIMIT", "档案版本超过系统上限", 422)
	}
	version := latest + 1
	id, e := newID()
	if e != nil {
		return e
	}
	history, e := archiveHistoryReferences(tx, c)
	if e != nil {
		return e
	}
	if predecessor != "" {
		history["predecessor_package_id"] = predecessor
	}
	history["claim_id"] = c.ID
	history["projection_revision"] = c.ProjectionRevision
	history["content_hash"] = c.ContentHash
	history["package_id"] = id
	history["package_version"] = version
	history["current_approval_round"] = c.CurrentRound
	history["document_snapshot"] = c
	raw, e := encode(history)
	if e != nil {
		return e
	}
	if e = tx.Create(&store.Archive{ID: id, TenantID: c.TenantID, ClaimID: c.ID, PackageVersion: version, Status: "PENDING", ManifestJSON: raw, CreatedAt: time.Now().UTC()}).Error; e != nil {
		return e
	}
	jobID, e := newID()
	if e != nil {
		return e
	}
	return tx.Create(&store.Job{ID: jobID, TenantID: c.TenantID, Kind: "ARCHIVE", ResourceID: id, PayloadJSON: raw, Status: "PENDING", Version: 1, NextAttemptAt: time.Now().UTC()}).Error
}

// archiveRetry 复用进行中任务或为失败包创建显式后继版本，不改旧包截止边界。
// 参数：tx为单据锁事务；cc为有财务范围且携带当前版本的受控重试命令。
// 返回值：最新任务及新包关联回执；已封存或版本竞争返回冲突。
func (s *Service) archiveRetry(tx *gorm.DB, cc *commandContext) (any, string, error) {
	c, _, err := loadClaim(tx, cc.actor.TenantID, cc.resourceID, true)
	if err != nil {
		return nil, "", err
	}
	if err = requireFinancial(tx, cc.actor, c, "expense.archive.retry"); err != nil {
		return nil, "", err
	}
	var archive store.Archive
	err = tx.Where("tenant_id = ? AND claim_id = ?", c.TenantID, c.ID).Order("package_version DESC").Take(&archive).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || err == nil && archive.Status == "SEALED" {
		return nil, "", domain.Fail("STATE_CONFLICT", "最新档案无需重试", 409)
	}
	if err != nil {
		return nil, "", err
	}
	var live store.Job
	err = tx.Where("tenant_id = ? AND kind = ? AND resource_id = ? AND status IN ?", c.TenantID, "ARCHIVE", archive.ID, []string{"PENDING", "PROCESSING"}).Order("created_at,id").Take(&live).Error
	if err == nil && (archive.Status != "FAILED" || live.Status == "PROCESSING") {
		return archiveJobReceipt(c.ID, archive, live), c.ID, nil
	}
	// 复用既有任务不改变资料边界；创建后继前才需要当前单据版本。
	if versionErr := verifyVersion(c.Version, cc.input.ExpectedVersion); versionErr != nil {
		return nil, "", versionErr
	}
	if err == nil {
		// 只接管尚未领取的失败重投；已领取worker继续持有其租约。
		updated := tx.Model(&store.Job{}).Where("tenant_id=? AND id=? AND status=? AND version=?", c.TenantID, live.ID, "PENDING", live.Version).Updates(map[string]any{"status": "FAILED", "version": live.Version + 1, "lease_owner": "", "lease_until": nil, "last_error": "SUPERSEDED_BY_CONTROLLED_RETRY"})
		if updated.Error != nil {
			return nil, "", updated.Error
		}
		if updated.RowsAffected != 1 {
			return nil, "", domain.Fail("STATE_CONFLICT", "归档任务已被领取，请刷新", 409)
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, "", err
	}
	if archive.Status != "FAILED" {
		updated := tx.Model(&store.Archive{}).Where("tenant_id=? AND id=? AND status=?", c.TenantID, archive.ID, archive.Status).Update("status", "FAILED")
		if updated.Error != nil {
			return nil, "", updated.Error
		}
		if updated.RowsAffected != 1 {
			return nil, "", domain.Fail("STATE_CONFLICT", "旧档案状态已经改变，请刷新", 409)
		}
	}
	if err = createArchiveJobFrom(tx, c, archive.ID); err != nil {
		return nil, "", err
	}
	var successor store.Archive
	if err = tx.Where("tenant_id=? AND claim_id=?", c.TenantID, c.ID).Order("package_version DESC").Take(&successor).Error; err != nil {
		return nil, "", err
	}
	var job store.Job
	if err = tx.Where("tenant_id=? AND kind=? AND resource_id=?", c.TenantID, "ARCHIVE", successor.ID).Take(&job).Error; err != nil {
		return nil, "", err
	}
	return archiveJobReceipt(c.ID, successor, job), c.ID, nil
}

// archiveJobReceipt 返回真实持久任务和包版本，便于客户端区分后继归档与旧失败包。
// 参数：claimID为单据；archive/job为同租户同单的已校验包和任务。
// 返回值：排队状态与稳定关联标识。
func archiveJobReceipt(claimID string, archive store.Archive, job store.Job) map[string]any {
	return map[string]any{"job_id": job.ID, "archive_id": archive.ID, "package_version": archive.PackageVersion, "status": job.Status, "claim_id": claimID}
}
