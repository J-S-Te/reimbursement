package jobs

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// archiveHistoryRows 精确装载请求冻结ID，显式空数组不查时间窗口或后续记录。
// 参数：db为归档查询，tenant/claim为归属，ids为精确集合，legacy允许旧窗口，cutoff为请求时刻，kind为稳定错误前缀，budget为内容预算。
// 返回值：完整有界集合；缺失、错归属、超限或未来ID均拒绝。
func archiveHistoryRows[T any](db *gorm.DB, tenant, claim string, ids []string, legacy bool, cutoff time.Time, kind string, budget *int64) ([]T, error) {
	result := []T{}
	if !legacy && len(ids) == 0 {
		return result, nil
	}
	query := db.Model(new(T)).Where("tenant_id=? AND claim_id=? AND created_at<=?", tenant, claim, cutoff)
	if !legacy {
		query = query.Where("id IN ?", ids)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 2000 {
		return nil, errors.New("ARCHIVE_" + kind + "_LIMIT")
	}
	if !legacy && int(count) != len(ids) {
		return nil, errors.New("ARCHIVE_" + kind + "_MISSING")
	}
	if _, isAction := any(new(T)).(*store.Action); !isAction {
		var size int64
		if err := query.Select("COALESCE(SUM(LENGTH(content_json)),0)").Scan(&size).Error; err != nil {
			return nil, err
		}
		*budget += size
		if *budget > 12<<20 {
			return nil, errors.New("ARCHIVE_MANIFEST_LIMIT")
		}
	}
	if err := query.Select("*").Order("created_at,id").Find(&result).Error; err != nil {
		return nil, err
	}
	return result, nil
}

// archivePinnedHistory 仅封存精确不可变动作与异常事实/决定，不读取异常可变游标。
// 参数：db为请求上下文查询，tenant/claim为归属，request为冻结载荷，cutoff为资料截止，history/files/budget为累积结果及预算。
// 返回值：缺失ID、内容摘要、关联或证据错误。
func archivePinnedHistory(db *gorm.DB, tenant, claim string, request archiveFrozenRequest, cutoff time.Time, history *archiveHistory, files map[string]bool, budget *int64) error {
	var err error
	history.Actions, err = archiveHistoryRows[store.Action](db, tenant, claim, request.ClaimActionIDs, request.Legacy, cutoff, "ACTION", budget)
	if err != nil {
		return err
	}
	history.ActionDetails, err = archiveHistoryRows[store.ActionDetail](db, tenant, claim, request.ApprovalActionDetailIDs, request.Legacy, cutoff, "ACTION_DETAIL", budget)
	if err != nil {
		return err
	}
	history.ExceptionFacts, err = archiveHistoryRows[store.PaymentExceptionFact](db, tenant, claim, request.ExceptionFactIDs, request.Legacy, cutoff, "EXCEPTION_FACT", budget)
	if err != nil {
		return err
	}
	history.ExceptionReviews, err = archiveHistoryRows[store.PaymentExceptionReview](db, tenant, claim, request.ExceptionReviewIDs, request.Legacy, cutoff, "EXCEPTION_REVIEW", budget)
	if err != nil {
		return err
	}
	actions := map[string]bool{}
	for _, action := range history.Actions {
		if action.Round < 0 || uint64(action.Round) > request.CurrentApprovalRound {
			return errors.New("ARCHIVE_ACTION_ROUND_INVALID")
		}
		actions[action.ID] = true
	}
	for _, detail := range history.ActionDetails {
		if !request.Legacy && !actions[detail.ActionID] {
			return errors.New("ARCHIVE_ACTION_DETAIL_PARENT_MISSING")
		}
		if err = collectEvidenceIDs(detail.ContentJSON, files); err != nil {
			return err
		}
	}
	return verifyArchiveExceptions(history, tenant, claim, files)
}

// verifyArchiveExceptions 验证不可变事实及核实决定的归属、摘要、父链和必需证据。
// 参数：history为精确装载的归档历史，tenant/claim为可信归属，files为原件核验集合。
// 返回值：任何事实损坏、孤立决定或缺少凭证时返回稳定错误。
func verifyArchiveExceptions(history *archiveHistory, tenant, claim string, files map[string]bool) error {
	facts := map[string]bool{}
	for _, row := range history.ExceptionFacts {
		var fact domain.PaymentExceptionFact
		if json.Unmarshal([]byte(row.ContentJSON), &fact) != nil || !verifyArchiveContentHash(fact, row.ContentHash) {
			return errors.New("ARCHIVE_EXCEPTION_FACT_HASH_MISMATCH")
		}
		if fact.ID != row.ID || fact.TenantID != tenant || fact.ClaimID != claim || row.TenantID != tenant || row.ClaimID != claim {
			return errors.New("ARCHIVE_EXCEPTION_FACT_SCOPE_MISMATCH")
		}
		if len(fact.VoucherFileIDs) == 0 {
			return errors.New("ARCHIVE_EXCEPTION_FACT_EVIDENCE_MISSING")
		}
		if err := collectEvidenceIDs(row.ContentJSON, files); err != nil {
			return err
		}
		facts[row.ID] = true
	}
	for _, row := range history.ExceptionReviews {
		var review domain.PaymentExceptionReview
		if json.Unmarshal([]byte(row.ContentJSON), &review) != nil || !verifyArchiveContentHash(review, row.ContentHash) {
			return errors.New("ARCHIVE_EXCEPTION_REVIEW_HASH_MISMATCH")
		}
		if review.ID != row.ID || review.ExceptionID != row.ExceptionID || review.Version != row.Version || row.TenantID != tenant || row.ClaimID != claim || !facts[review.ExceptionID] {
			return errors.New("ARCHIVE_EXCEPTION_REVIEW_SCOPE_MISMATCH")
		}
		if len(review.EvidenceFileIDs) == 0 {
			return errors.New("ARCHIVE_EXCEPTION_REVIEW_EVIDENCE_MISSING")
		}
		if err := collectEvidenceIDs(row.ContentJSON, files); err != nil {
			return err
		}
	}
	return nil
}

// verifyArchiveExceptionLinks 将不可变核实决定关联到同包真实财务效果，不借实时游标补链。
// 参数：history为已验证事实与决定，spend为冻结完整财务效果。
// 返回值：关联付款或冲正不属于本单据财务历史时拒绝。
func verifyArchiveExceptionLinks(history archiveHistory, spend domain.SpendSnapshot) error {
	payments, reversals := map[string]bool{}, map[string]bool{}
	for _, effect := range spend.Effects {
		payments[effect.PaymentRecordID] = true
		if effect.Kind == "PAYMENT_FULLY_REVERSED" {
			reversals[effect.EffectID] = true
		}
	}
	for _, row := range history.ExceptionReviews {
		var review domain.PaymentExceptionReview
		if json.Unmarshal([]byte(row.ContentJSON), &review) != nil {
			return errors.New("ARCHIVE_EXCEPTION_REVIEW_INVALID")
		}
		if review.LinkedPaymentID != "" && !payments[review.LinkedPaymentID] || review.LinkedReversalID != "" && !reversals[review.LinkedReversalID] {
			return errors.New("ARCHIVE_EXCEPTION_FINANCIAL_LINK_MISSING")
		}
	}
	return nil
}
