package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// requireExceptionRead 将异常财务事实限定在当前能力及显式财务范围内。
// 参数：db 为数据库，a 为身份，c 为关联单据。
// 返回值：无权限或范围时拒绝。
func requireExceptionRead(db *gorm.DB, a domain.Actor, c domain.Claim) error {
	if !a.Has("expense.claim.read") || (!a.Has("expense.payment.exception.record") && !a.Has("expense.payment.exception.review") && !a.Has("expense.audit.read")) {
		return domain.Fail("FORBIDDEN", "没有外部付款异常台账的当前读取权限", 403)
	}
	allowed, e := financeScope(db, a, c.OrgID)
	if e != nil {
		return e
	}
	if !allowed {
		return domain.Fail("RESOURCE_NOT_FOUND", "异常台账不存在或不在财务范围", 404)
	}
	return nil
}

// exceptionView 将不可变原始事实与独立工作游标组合，不覆盖观察时状态。
// 参数：fact 为原记录，c 为当前单据，cursor 为核实游标，reviews 为可选完整历史。
// 返回值：响应对象或编码错误。
func exceptionView(fact domain.PaymentExceptionFact, c domain.Claim, cursor store.PaymentExceptionCursor, reviews []domain.PaymentExceptionReview) (any, error) {
	raw, e := encode(fact)
	if e != nil {
		return nil, e
	}
	var view map[string]any
	if e = json.Unmarshal([]byte(raw), &view); e != nil {
		return nil, e
	}
	view["status"] = cursor.Status
	view["version"] = cursor.Version
	view["claim_version"] = c.Version
	view["current_claim_status"] = c.Status
	view["review_count"] = cursor.Version - 1
	view["allowed_actions"] = []string{}
	if reviews != nil {
		view["reviews"] = reviews
	}
	return view, nil
}

// paymentExceptionQuery 分页读取授权异常事实或带完整独立决定的详情。
// 参数：db 为数据库，a 为当前身份，kind/id 为列表单据或异常，params 为分页参数。
// 返回值：台账页或详情，范围外对象不可见。
func (s *Service) paymentExceptionQuery(db *gorm.DB, a domain.Actor, kind, id string, params map[string]string) (any, error) {
	claimID := id
	var fact domain.PaymentExceptionFact
	var e error
	if kind == "payment-exception" {
		fact, _, e = loadException(db, a.TenantID, id)
		if e != nil {
			return nil, e
		}
		claimID = fact.ClaimID
	}
	c, _, e := loadClaim(db, a.TenantID, claimID, false)
	if e != nil {
		return nil, e
	}
	if e = requireExceptionRead(db, a, c); e != nil {
		return nil, e
	}
	if kind == "payment-exception" {
		var cursor store.PaymentExceptionCursor
		if e = db.Where("tenant_id = ? AND id = ? AND claim_id = ?", a.TenantID, fact.ID, c.ID).First(&cursor).Error; e != nil {
			return nil, e
		}
		var rows []store.PaymentExceptionReview
		if e = db.Where("tenant_id = ? AND exception_id = ?", a.TenantID, fact.ID).Order("version").Limit(1001).Find(&rows).Error; e != nil {
			return nil, e
		}
		if len(rows) > 1000 {
			return nil, domain.Fail("EXCEPTION_HISTORY_LIMIT", "核实历史超过单次读取上限", 422)
		}
		reviews := []domain.PaymentExceptionReview{}
		for _, row := range rows {
			var review domain.PaymentExceptionReview
			if e = json.Unmarshal([]byte(row.ContentJSON), &review); e != nil {
				return nil, e
			}
			hash, err := digest(review)
			if err != nil {
				return nil, err
			}
			if hash != row.ContentHash {
				return nil, domain.Fail("EXCEPTION_HISTORY_MISMATCH", "异常核实历史摘要不一致", 409)
			}
			reviews = append(reviews, review)
		}
		view, err := exceptionView(fact, c, cursor, reviews)
		if err != nil {
			return nil, err
		}
		allowExceptionReview(view, a, c, fact, cursor)
		return view, nil
	}
	page, size, offset := pageArgs(params)
	q := db.Model(&store.PaymentExceptionFact{}).Where("tenant_id = ? AND claim_id = ?", a.TenantID, c.ID)
	var total int64
	if e = q.Count(&total).Error; e != nil {
		return nil, e
	}
	var rows []store.PaymentExceptionFact
	if e = q.Order("created_at DESC,id DESC").Limit(size).Offset(offset).Find(&rows).Error; e != nil {
		return nil, e
	}
	items := []any{}
	for _, row := range rows {
		var original domain.PaymentExceptionFact
		if e = json.Unmarshal([]byte(row.ContentJSON), &original); e != nil {
			return nil, e
		}
		hash, err := digest(original)
		if err != nil {
			return nil, err
		}
		if hash != row.ContentHash {
			return nil, domain.Fail("EXCEPTION_FACT_HASH_MISMATCH", "异常事实摘要不一致", 409)
		}
		var cursor store.PaymentExceptionCursor
		if e = db.Where("tenant_id = ? AND id = ?", a.TenantID, row.ID).First(&cursor).Error; e != nil {
			return nil, e
		}
		view, err := exceptionView(original, c, cursor, nil)
		if err != nil {
			return nil, err
		}
		allowExceptionReview(view, a, c, original, cursor)
		items = append(items, view)
	}
	return map[string]any{"items": items, "total": total, "page": page, "page_size": size}, nil
}

// allowExceptionReview 根据当前独立权限与游标状态提供前端可用动作。
// 参数：view 为已裁剪响应，a 为身份，c/fact/cursor 为当前授权事实。
// 返回值：无。
func allowExceptionReview(view any, a domain.Actor, c domain.Claim, fact domain.PaymentExceptionFact, cursor store.PaymentExceptionCursor) {
	data, ok := view.(map[string]any)
	if !ok {
		return
	}
	if a.Has("expense.payment.exception.review") && a.SubjectID != c.Applicant && a.SubjectID != fact.Recorder && (cursor.Status == "PENDING" || cursor.Status == "VERIFIED_CONFLICT") {
		data["allowed_actions"] = []string{"REVIEW_EXCEPTION"}
	}
}
