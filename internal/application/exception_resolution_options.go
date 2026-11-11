package application

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type exceptionPaymentOption struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	PaidDate          string       `json:"paid_date"`
	Amount            domain.Money `json:"amount_minor"`
	Currency          string       `json:"currency"`
	Method            string       `json:"method"`
	TransactionNo     string       `json:"transaction_no"`
	CanResolvePayment bool         `json:"can_resolve_payment"`
}

type exceptionReversalOption struct {
	ID            string       `json:"id"`
	PaymentID     string       `json:"payment_id"`
	Name          string       `json:"name"`
	FactKind      string       `json:"fact_kind"`
	Amount        domain.Money `json:"amount_minor"`
	Currency      string       `json:"currency"`
	TransactionNo string       `json:"transaction_no"`
}

// ExceptionResolutionOptions 按原异常交易筛选当前可合法解除冲突的真实财务账本。
// 参数：ctx/a 为可信核实人调用；exceptionID 为原始付款异常坐标。
// 返回值：匹配付款与真实冲正选项及版本；越界、自审、终结或账本损坏时拒绝。
func (s *Service) ExceptionResolutionOptions(ctx context.Context, a domain.Actor, exceptionID string) (json.RawMessage, error) {
	db := s.db.WithContext(ctx)
	c, fact, cursor, err := s.resolutionOptionsContext(db, a, exceptionID)
	if err != nil {
		return nil, err
	}
	var rows []store.Payment
	err = db.Where("tenant_id = ? AND claim_id = ? AND method = ? AND source_account_ref = ? AND transaction_no = ?",
		c.TenantID, c.ID, fact.Method, fact.SourceAccountRef, fact.TransactionNo).
		Order("created_at DESC,id DESC").Limit(101).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) > 100 {
		return nil, domain.Fail("OPTIONS_LIMIT", "同交易财务记录超过受控上限，请核实账本", 422)
	}
	payments := []exceptionPaymentOption{}
	reversals := []exceptionReversalOption{}
	for _, row := range rows {
		payment, linked, err := resolutionPaymentOptions(db, a, c, fact, row)
		if err != nil {
			return nil, err
		}
		if payment != nil {
			payments = append(payments, *payment)
			reversals = append(reversals, linked...)
		}
	}
	return json.Marshal(map[string]any{"payments": payments, "reversals": reversals,
		"claim_id": c.ID, "claim_version": c.Version, "exception_version": cursor.Version})
}

// resolutionOptionsContext 核对原事实、整单读取、独立核实能力和当前可操作游标。
// 参数：db 为数据库；a 为可信身份；exceptionID 为当前租户异常坐标。
// 返回值：已授权单据、原始事实和游标；不满足操作边界时拒绝。
func (s *Service) resolutionOptionsContext(db *gorm.DB, a domain.Actor, exceptionID string) (domain.Claim, domain.PaymentExceptionFact, store.PaymentExceptionCursor, error) {
	var c domain.Claim
	var cursor store.PaymentExceptionCursor
	if a.TenantID == "" || a.SubjectID == "" {
		return c, domain.PaymentExceptionFact{}, cursor, domain.Fail("SESSION_EXPIRED", "请重新登录", 401)
	}
	fact, row, err := loadException(db, a.TenantID, exceptionID)
	if err != nil {
		return c, fact, cursor, err
	}
	if fact.ID != row.ID || fact.TenantID != a.TenantID || fact.ClaimID != row.ClaimID {
		return c, fact, cursor, domain.Fail("EXCEPTION_FACT_HASH_MISMATCH", "异常事实归属不一致", 409)
	}
	c, _, err = loadClaim(db, a.TenantID, fact.ClaimID, false)
	if err == nil {
		err = requireFinancial(db, a, c, "expense.payment.exception.review")
	}
	if err != nil {
		return c, fact, cursor, err
	}
	full, err := s.fullRead(db, a, c)
	if err != nil || !full {
		if err == nil {
			err = domain.Fail("RESOURCE_NOT_FOUND", "没有完整财务账本访问权限", 404)
		}
		return c, fact, cursor, err
	}
	if a.SubjectID == fact.Recorder || a.SubjectID == c.Applicant {
		return c, fact, cursor, domain.Fail("EXCEPTION_REVIEWER_CONFLICT", "核实人必须独立于记录人和申请人", 403)
	}
	err = db.Where("tenant_id = ? AND id = ? AND claim_id = ?", a.TenantID, fact.ID, c.ID).First(&cursor).Error
	if err == nil && (cursor.Status == "RESOLVED" || cursor.Status == "DISMISSED") {
		err = domain.Fail("EXCEPTION_ALREADY_FINAL", "此异常已经终结", 409)
	}
	if err == nil && cursor.Status != "PENDING" && cursor.Status != "VERIFIED_CONFLICT" {
		err = domain.Fail("STATE_CONFLICT", "付款异常当前状态不支持解除核实", 409)
	}
	return c, fact, cursor, err
}

// resolutionPaymentOptions 复用正式解除校验逐一核对付款和关联真实冲正的证据链。
// 参数：db/a/c/fact 为授权边界；row 为同单同交易付款候选。
// 返回值：合法付款显示及冲正选项；不匹配时为空，证据损坏或读取失败时返回错误。
func resolutionPaymentOptions(db *gorm.DB, a domain.Actor, c domain.Claim, fact domain.PaymentExceptionFact, row store.Payment) (*exceptionPaymentOption, []exceptionReversalOption, error) {
	var payment PaymentResult
	if err := json.Unmarshal([]byte(row.ContentJSON), &payment); err != nil {
		return nil, nil, err
	}
	if payment.ID != row.ID || payment.ClaimID != row.ClaimID {
		return nil, nil, domain.Fail("PAYMENT_FACT_MISMATCH", "付款事实与账本归属不一致", 409)
	}
	allowed, err := resolutionCandidateAllowed(validateExceptionResolution(db, c, fact, a,
		PaymentExceptionReviewInput{Decision: "RESOLVE_WITH_PAYMENT", LinkedPaymentID: row.ID}))
	if err != nil {
		return nil, nil, err
	}
	reversals, err := resolutionReversalOptions(db, a, c, fact, payment)
	if err != nil || !allowed && len(reversals) == 0 {
		return nil, nil, err
	}
	return &exceptionPaymentOption{ID: row.ID, Name: "已登记付款", PaidDate: payment.PaidDate,
		Amount: payment.Amount, Currency: payment.Currency, Method: payment.Method,
		TransactionNo: payment.TransactionNo, CanResolvePayment: allowed}, reversals, nil
}

// resolutionReversalOptions 仅从不可变冲正事实表读取并验证已批准的同付款全额冲正。
// 参数：db/a/c/fact 为当前核实边界；payment 为已读取的原付款事实。
// 返回值：使用真实 Reversal.ID 的合法冲正集合，不读取申请编号冒充财务事实。
func resolutionReversalOptions(db *gorm.DB, a domain.Actor, c domain.Claim, fact domain.PaymentExceptionFact, payment PaymentResult) ([]exceptionReversalOption, error) {
	var rows []store.Reversal
	err := db.Where("tenant_id = ? AND claim_id = ? AND payment_id = ?",
		c.TenantID, c.ID, payment.ID).Order("created_at DESC,id DESC").Limit(101).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) > 100 {
		return nil, domain.Fail("OPTIONS_LIMIT", "同付款冲正记录超过受控上限，请核实账本", 422)
	}
	items := []exceptionReversalOption{}
	for _, row := range rows {
		allowed, err := resolutionCandidateAllowed(validateExceptionResolution(db, c, fact, a,
			PaymentExceptionReviewInput{Decision: "RESOLVE_WITH_REVERSAL", LinkedPaymentID: payment.ID, LinkedReversalID: row.ID}))
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		var proof struct {
			Request ReversalData `json:"request"`
		}
		if err = json.Unmarshal([]byte(row.ContentJSON), &proof); err != nil {
			return nil, err
		}
		items = append(items, exceptionReversalOption{ID: row.ID, PaymentID: payment.ID,
			Name: "已批准全额冲正", FactKind: proof.Request.FactKind, Amount: payment.Amount,
			Currency: payment.Currency, TransactionNo: payment.TransactionNo})
	}
	return items, nil
}

// resolutionCandidateAllowed 只将明确业务不匹配作为不可选，保留损坏及存储错误。
// 参数：err 为正式财务解除校验返回的结果。
// 返回值：可选标记或需要失败关闭的原始错误。
func resolutionCandidateAllowed(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	var problem *domain.Problem
	if errors.As(err, &problem) && (problem.Code == "EXCEPTION_LINK_MISMATCH" || problem.Code == "EXCEPTION_REVIEWER_CONFLICT") {
		return false, nil
	}
	return false, err
}
