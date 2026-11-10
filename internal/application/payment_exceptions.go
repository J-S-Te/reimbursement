package application

import (
	"context"
	"encoding/json"
	"errors"
	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

type PaymentExceptionInput struct {
	PaymentInput
	Reason string `json:"reason"`
}
type PaymentExceptionReviewInput struct {
	CommandInput
	ExpectedClaimVersion int64    `json:"expected_claim_version"`
	Decision             string   `json:"decision"`
	Reason               string   `json:"reason"`
	EvidenceFileIDs      []string `json:"evidence_file_ids"`
	LinkedPaymentID      string   `json:"linked_payment_id"`
	LinkedReversalID     string   `json:"linked_reversal_id"`
}

// loadException 读取当前租户的不可变外部付款事实。
// 参数：tx 为数据库，tenant/id 为可信资源定位。
// 返回值：原始事实及数据库行，不存在时隐藏对象。
func loadException(tx *gorm.DB, tenant, id string) (domain.PaymentExceptionFact, store.PaymentExceptionFact, error) {
	var row store.PaymentExceptionFact
	var fact domain.PaymentExceptionFact
	e := tx.Where("tenant_id = ? AND id = ?", tenant, id).First(&row).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return fact, row, domain.Fail("RESOURCE_NOT_FOUND", "付款异常不存在或无权访问", 404)
	}
	if e == nil {
		e = json.Unmarshal([]byte(row.ContentJSON), &fact)
		if e == nil {
			hash, err := digest(fact)
			if err != nil {
				return fact, row, err
			}
			if hash != row.ContentHash {
				return fact, row, domain.Fail("EXCEPTION_FACT_HASH_MISMATCH", "异常原始事实摘要不一致", 409)
			}
		}
	}
	return fact, row, e
}

// preflightFinanceChanges 将新控制命令的范围及证据验证置于本地事务外。
// 参数：ctx 为请求边界，db 为数据库，cc 为可信命令，payload 为状态控制或外部事实。
// 返回值：权限、范围、版本、文件或事实格式错误。
func (s *Service) preflightFinanceChanges(ctx context.Context, db *gorm.DB, cc *commandContext, payload json.RawMessage) error {
	claimID := cc.resourceID
	permission := map[string]string{"claim.reopen": "expense.claim.reopen", "claim.approved_void": "expense.claim.approved_void", "payment.exception.record": "expense.payment.exception.record", "payment.exception.review": "expense.payment.exception.review"}[cc.operation]
	var fact domain.PaymentExceptionFact
	var e error
	if cc.operation == "payment.exception.review" {
		fact, _, e = loadException(db, cc.actor.TenantID, cc.resourceID)
		if e != nil {
			return e
		}
		claimID = fact.ClaimID
	}
	c, _, e := loadClaim(db, cc.actor.TenantID, claimID, false)
	if e != nil {
		return e
	}
	if e = requireFinancial(db, cc.actor, c, permission); e != nil {
		return e
	}
	if strings.HasPrefix(cc.operation, "claim.") {
		var input ClaimControlInput
		if e = json.Unmarshal(payload, &input); e != nil {
			return e
		}
		return validateClaimControl(c, cc.actor, input)
	}
	var ids []string
	if cc.operation == "payment.exception.record" {
		var input PaymentExceptionInput
		if e = json.Unmarshal(payload, &input); e != nil {
			return e
		}
		if e = verifyVersion(c.Version, input.ExpectedVersion); e != nil {
			return e
		}
		if e = validateExceptionFact(input); e != nil {
			return e
		}
		ids = input.VoucherFileIDs
	} else {
		var input PaymentExceptionReviewInput
		if e = json.Unmarshal(payload, &input); e != nil {
			return e
		}
		if e = validateExceptionReviewer(c, fact, cc.actor, input); e != nil {
			return e
		}
		var cursor store.PaymentExceptionCursor
		if e = db.Where("tenant_id = ? AND id = ?", c.TenantID, fact.ID).First(&cursor).Error; e != nil {
			return e
		}
		if e = verifyVersion(cursor.Version, input.ExpectedVersion); e != nil {
			return e
		}
		if cursor.Status == "RESOLVED" || cursor.Status == "DISMISSED" {
			return domain.Fail("EXCEPTION_ALREADY_FINAL", "此异常已经终结", 409)
		}
		ids = input.EvidenceFileIDs
	}
	if s.deps.Files == nil {
		return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "财务证据文件服务未配置", 503)
	}
	return s.deps.Files.Validate(ctx, cc.actor, ids, "reimbursement.payment.voucher")
}

// validateExceptionFact 校验外部已发生付款的最小事实，不将异常金额强行改成批准额。
// 参数：input 为财务操作者提供的原始事实和证据。
// 返回值：缺失或不合法字段错误。
func validateExceptionFact(input PaymentExceptionInput) error {
	if input.Amount <= 0 || input.Currency != "CNY" || (input.Method != "BANK" && input.Method != "CASH") || strings.TrimSpace(input.SourceAccountRef) == "" || len(input.SourceAccountRef) > 128 || strings.TrimSpace(input.TransactionNo) == "" || len(input.TransactionNo) > 128 || strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 2000 || len(input.PayeeProfileID) > 64 {
		return domain.Fail("INVALID_PAYMENT_EXCEPTION", "请完整记录外部正向付款金额、币种、来源、流水和异常原因", 422)
	}
	if _, e := time.Parse("2006-01-02", input.PaidDate); e != nil {
		return domain.Fail("INVALID_DATE", "外部付款日期必须为有效业务日期", 422)
	}
	return nil
}

// recordPaymentException 只追加外部付款事实和核实游标，不创建付款效果或项目事件。
// 参数：tx 为本地事务，cc 为财务记录人，payload 为真实外部事实。
// 返回值：原始事实与独立待核实状态。
func (s *Service) recordPaymentException(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input PaymentExceptionInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	c, _, e := loadClaim(tx, cc.actor.TenantID, cc.resourceID, true)
	if e != nil {
		return nil, "", e
	}
	if e = requireFinancial(tx, cc.actor, c, "expense.payment.exception.record"); e != nil {
		return nil, "", e
	}
	if e = verifyVersion(c.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if e = validateExceptionFact(input); e != nil {
		return nil, "", e
	}
	id, e := newID()
	if e != nil {
		return nil, "", e
	}
	fact := domain.PaymentExceptionFact{ID: id, TenantID: c.TenantID, ClaimID: c.ID, Recorder: cc.actor.SubjectID, ObservedClaimStatus: c.Status, ObservedClaimVersion: c.Version, Amount: input.Amount, Currency: input.Currency, PaidDate: input.PaidDate, Method: input.Method, SourceAccountRef: input.SourceAccountRef, TransactionNo: input.TransactionNo, PayeeProfileID: input.PayeeProfileID, VoucherFileIDs: input.VoucherFileIDs, Reason: input.Reason, CreatedAt: time.Now().UTC()}
	raw, e := encode(fact)
	if e != nil {
		return nil, "", e
	}
	hash, e := digest(fact)
	if e != nil {
		return nil, "", e
	}
	var duplicated int64
	if e = tx.Model(&store.PaymentExceptionFact{}).Where("tenant_id = ? AND method = ? AND source_account_ref = ? AND transaction_no = ?", c.TenantID, fact.Method, fact.SourceAccountRef, fact.TransactionNo).Count(&duplicated).Error; e != nil {
		return nil, "", e
	}
	if duplicated > 0 {
		return nil, "", domain.Fail("PAYMENT_EXCEPTION_TRANSACTION_CONFLICT", "此渠道、来源账户和流水已有异常事实，请核对原记录", 409)
	}
	if e = tx.Create(&store.PaymentExceptionFact{ID: id, TenantID: c.TenantID, ClaimID: c.ID, Recorder: fact.Recorder, Method: fact.Method, SourceAccountRef: fact.SourceAccountRef, TransactionNo: fact.TransactionNo, ContentHash: hash, ContentJSON: raw, CreatedAt: fact.CreatedAt}).Error; e != nil {
		var sqlError *driver.MySQLError
		if errors.As(e, &sqlError) && sqlError.Number == 1062 {
			return nil, "", domain.Fail("PAYMENT_EXCEPTION_TRANSACTION_CONFLICT", "此交易已有异常事实，请核对原记录", 409)
		}
		return nil, "", e
	}
	cursor := store.PaymentExceptionCursor{ID: id, TenantID: c.TenantID, ClaimID: c.ID, Status: "PENDING", Version: 1}
	if e = tx.Create(&cursor).Error; e != nil {
		return nil, "", e
	}
	if e = appendFinancialHistoryArchive(tx, c); e != nil {
		return nil, "", e
	}
	view, e := exceptionView(fact, c, cursor, nil)
	return view, c.ID, e
}

// validateExceptionReviewer 检查当前单据版本、独立核实人、决议和证据。
// 参数：c 为单据，fact 为原事实，actor 为当前核实人，input 为核实命令。
// 返回值：自审、旧版本或无效决定错误。
func validateExceptionReviewer(c domain.Claim, fact domain.PaymentExceptionFact, actor domain.Actor, input PaymentExceptionReviewInput) error {
	if actor.SubjectID == fact.Recorder || actor.SubjectID == c.Applicant {
		return domain.Fail("EXCEPTION_REVIEWER_CONFLICT", "核实人必须独立于事实记录人和申请人", 403)
	}
	if e := verifyVersion(c.Version, input.ExpectedClaimVersion); e != nil {
		return e
	}
	if strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 2000 {
		return domain.Fail("REASON_REQUIRED", "请填写独立核实依据", 422)
	}
	switch input.Decision {
	case "CONFIRM_CONFLICT", "DISMISS":
		if input.LinkedPaymentID != "" || input.LinkedReversalID != "" {
			return domain.Fail("INVALID_EXCEPTION_RESOLUTION", "此决定不能携带解除冲突的财务记录", 422)
		}
	case "RESOLVE_WITH_PAYMENT":
		if input.LinkedPaymentID == "" || input.LinkedReversalID != "" {
			return domain.Fail("INVALID_EXCEPTION_RESOLUTION", "请仅关联已经合法登记的有效付款", 422)
		}
	case "RESOLVE_WITH_REVERSAL":
		if input.LinkedPaymentID == "" || input.LinkedReversalID == "" {
			return domain.Fail("INVALID_EXCEPTION_RESOLUTION", "请关联原付款及已批准全额冲正记录", 422)
		}
	default:
		return domain.Fail("INVALID_EXCEPTION_DECISION", "不支持此异常核实决定", 422)
	}
	return nil
}

// reviewPaymentException 追加独立核实决定，解除冲突也只能引用已有合法财务链。
// 参数：tx 为本地事务，cc 为独立财务负责人，payload 为证据与决定。
// 返回值：新的核实游标回执；主单、付款和项目净额不变。
func (s *Service) reviewPaymentException(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input PaymentExceptionReviewInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	fact, _, e := loadException(tx, cc.actor.TenantID, cc.resourceID)
	if e != nil {
		return nil, "", e
	}
	c, _, e := loadClaim(tx, cc.actor.TenantID, fact.ClaimID, true)
	if e != nil {
		return nil, "", e
	}
	if e = requireFinancial(tx, cc.actor, c, "expense.payment.exception.review"); e != nil {
		return nil, "", e
	}
	if e = validateExceptionReviewer(c, fact, cc.actor, input); e != nil {
		return nil, "", e
	}
	var cursor store.PaymentExceptionCursor
	if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", c.TenantID, fact.ID).First(&cursor).Error; e != nil {
		return nil, "", e
	}
	if e = verifyVersion(cursor.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if cursor.Status == "RESOLVED" || cursor.Status == "DISMISSED" {
		return nil, "", domain.Fail("EXCEPTION_ALREADY_FINAL", "此异常已经终结，只能查阅不可变历史", 409)
	}
	status := "VERIFIED_CONFLICT"
	if input.Decision == "DISMISS" {
		status = "DISMISSED"
	}
	if input.Decision == "RESOLVE_WITH_PAYMENT" || input.Decision == "RESOLVE_WITH_REVERSAL" {
		if e = validateExceptionResolution(tx, c, fact, cc.actor, input); e != nil {
			return nil, "", e
		}
		status = "RESOLVED"
	}
	id, e := newID()
	if e != nil {
		return nil, "", e
	}
	review := domain.PaymentExceptionReview{ID: id, ExceptionID: fact.ID, Actor: cc.actor.SubjectID, Decision: input.Decision, Reason: input.Reason, EvidenceFileIDs: input.EvidenceFileIDs, LinkedPaymentID: input.LinkedPaymentID, LinkedReversalID: input.LinkedReversalID, PreviousStatus: cursor.Status, Status: status, PreviousVersion: cursor.Version, Version: cursor.Version + 1, ClaimVersion: c.Version, ClaimStatus: c.Status, CreatedAt: time.Now().UTC()}
	raw, e := encode(review)
	if e != nil {
		return nil, "", e
	}
	hash, e := digest(review)
	if e != nil {
		return nil, "", e
	}
	if e = tx.Create(&store.PaymentExceptionReview{ID: id, TenantID: c.TenantID, ExceptionID: fact.ID, ClaimID: c.ID, Actor: review.Actor, Version: review.Version, Decision: review.Decision, ContentHash: hash, ContentJSON: raw, CreatedAt: review.CreatedAt}).Error; e != nil {
		return nil, "", e
	}
	r := tx.Model(&store.PaymentExceptionCursor{}).Where("tenant_id = ? AND id = ? AND version = ?", c.TenantID, fact.ID, cursor.Version).Updates(map[string]any{"status": status, "version": review.Version, "last_review_id": id})
	if r.Error != nil {
		return nil, "", r.Error
	}
	if r.RowsAffected != 1 {
		return nil, "", domain.Fail("VERSION_CONFLICT", "异常核实已被其他人处理", 409)
	}
	if e = appendFinancialHistoryArchive(tx, c); e != nil {
		return nil, "", e
	}
	return map[string]any{"id": fact.ID, "claim_id": c.ID, "claim_version": c.Version, "status": status, "version": review.Version, "review": review}, c.ID, nil
}
