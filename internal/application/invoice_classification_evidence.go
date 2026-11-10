package application

import (
	"encoding/json"
	"errors"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// rebuildDraftClassificationEvidence 根据实际用票重建只读依据，不占票、不追溯改变已批准单据。
// 参数：tx 为草稿保存事务，claim 为当前本人可编辑单据。
// 返回值：票据边界、有效规则或数据库异常；旧规则保持无依据字段。
func rebuildDraftClassificationEvidence(tx *gorm.DB, claim *domain.Claim) error {
	ids := invoiceIDs(*claim)
	if len(ids) == 0 {
		return nil
	}
	var rows []store.Invoice
	if err := tx.Where("tenant_id = ? AND owner = ? AND id IN ?", claim.TenantID, claim.Applicant, ids).Order("id").Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) != len(ids) {
		return domain.Fail("INVOICE_NOT_AVAILABLE", "费用引用了不存在或非本人的票据", 422)
	}
	evidence := map[string]domain.InvoiceClassificationEvidence{}
	policies := map[string]domain.InvoicePolicy{}
	for _, row := range rows {
		fact, ok, err := draftInvoiceClassificationEvidence(tx, *claim, row, policies)
		if err != nil {
			return err
		}
		if ok {
			evidence[row.ID] = fact
		}
	}
	return attachDraftClassificationEvidence(claim, evidence)
}

// attachDraftClassificationEvidence 按实际用票写回人工选值，去重并限制整单证据放大。
// 参数：claim 为清除客户端结论的草稿，evidence 为服务器按票据生成的可信依据。
// 返回值：证据行或命中总数超限时返回422，其余返回空。
func attachDraftClassificationEvidence(claim *domain.Claim, evidence map[string]domain.InvoiceClassificationEvidence) error {
	rows, matches := 0, 0
	for index := range claim.Items {
		item := &claim.Items[index]
		seen := map[string]bool{}
		for _, usage := range item.InvoiceUsages {
			fact, ok := evidence[usage.InvoiceID]
			if !ok || seen[usage.InvoiceID] {
				continue
			}
			seen[usage.InvoiceID] = true
			rows++
			matches += len(fact.Matches)
			if rows > 1000 || matches > 10000 {
				return domain.Fail("CLASSIFICATION_EVIDENCE_LIMIT", "整单归类依据超过1000行或10000项命中，请拆分费用草稿", 422)
			}
			fact.SelectedFeeType = item.FeeType
			fact.ManualOverride = item.FeeType != "" && (fact.Status == "NEEDS_REVIEW" || item.FeeType != fact.SuggestedFeeType)
			item.ClassificationEvidence = append(item.ClassificationEvidence, fact)
		}
	}
	return nil
}

// draftInvoiceClassificationEvidence 生成单张确认票据的可信依据，旧票种政策保持兼容。
// 参数：tx 为草稿事务，claim 为本人单据，row 为当前租户本人的票据，policies 为票种缓存。
// 返回值：服务器依据、是否需保存及读取或规则错误。
func draftInvoiceClassificationEvidence(tx *gorm.DB, claim domain.Claim, row store.Invoice, policies map[string]domain.InvoicePolicy) (domain.InvoiceClassificationEvidence, bool, error) {
	var inv domain.Invoice
	if err := json.Unmarshal([]byte(row.ContentJSON), &inv); err != nil {
		return domain.InvoiceClassificationEvidence{}, false, err
	}
	rule, found := policies[inv.InvoiceType]
	if !found {
		loaded, err := invoiceRule(tx, claim.TenantID, inv.InvoiceType)
		var problem *domain.Problem
		if errors.As(err, &problem) && problem.Code == "INVOICE_RULE_MISSING" {
			return domain.InvoiceClassificationEvidence{}, false, nil
		}
		if err != nil {
			return domain.InvoiceClassificationEvidence{}, false, err
		}
		rule = loaded
		policies[inv.InvoiceType] = rule
	}
	if len(rule.ContentMappings) == 0 {
		return domain.InvoiceClassificationEvidence{}, false, nil
	}
	if inv.ReviewStatus != "CONFIRMED" || row.IdentityKey == nil || inv.VerificationStatus == "MISMATCH" ||
		(row.ReservationState == "CONSUMED" && row.ReservationClaimID != claim.ID) ||
		(row.ReservationClaimID != "" && row.ReservationClaimID != claim.ID) || (row.ReservationState == "RESERVED" && row.ReservationClaimID != claim.ID) {
		return domain.InvoiceClassificationEvidence{}, false, domain.Fail("INVOICE_NOT_AVAILABLE", "内容归类仅适用于本人已确认且未被其他单据占用的票据", 422)
	}
	if err := validateInvoiceContent(inv.Content); err != nil {
		return domain.InvoiceClassificationEvidence{}, false, err
	}
	suggestion := domain.InvoiceExpenseSuggestion{FeeType: rule.FeeType, DefaultClaimType: rule.DefaultClaimType}
	applyInvoiceContentClassification(&suggestion, inv.Content, rule)
	if suggestion.FeeType == "" && suggestion.ClassificationStatus != "NEEDS_REVIEW" {
		suggestion.ClassificationStatus = "NEEDS_REVIEW"
		appendInvoiceClassificationReason(&suggestion, "NO_MAPPING")
	}
	return domain.InvoiceClassificationEvidence{
		InvoiceID: row.ID, InvoiceVersion: row.Version, RuleID: rule.ID, RuleVersion: rule.Version,
		Status: suggestion.ClassificationStatus, Source: suggestion.ClassificationSource,
		Reasons: suggestion.ClassificationReasons, Matches: suggestion.ClassificationMatches, SuggestedFeeType: suggestion.FeeType,
	}, true, nil
}
