package application

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// validateTravelDeclaration 仅接受明确城市编码和真实闭区间日期。
// 参数：value 为可选申请人声明，空值保留旧单据语义。
// 返回值：城市或日期缺失、非法、倒置时返回422。
func validateTravelDeclaration(value *domain.TravelDeclaration) error {
	if value == nil {
		return nil
	}
	if !structuredCityCode(value.OriginCityCode) || !structuredCityCode(value.DestinationCityCode) ||
		!validTravelPeriod(value.DepartureDate, value.ReturnDate) {
		return domain.Fail("INVALID_TRAVEL_DECLARATION", "差旅行程须填写六位城市代码和真实且不倒置的起止日期", 422)
	}
	return nil
}

// validateInvoiceTravelFacts 验证人工票面日期和城市，不把票面事实当自动识别或批准。
// 参数：value 为可选AIR、RAIL或HOTEL票面字段。
// 返回值：非法类型、城市、日期及零晚住宿返回422。
func validateInvoiceTravelFacts(value *domain.InvoiceTravelFacts) error {
	if value == nil {
		return nil
	}
	validKind := value.Kind == "AIR" || value.Kind == "RAIL" || value.Kind == "HOTEL"
	validOrigin := structuredCityCode(value.OriginCityCode) || value.Kind == "HOTEL" && value.OriginCityCode == ""
	if !validKind || !validOrigin || !structuredCityCode(value.DestinationCityCode) ||
		!validTravelPeriod(value.StartDate, value.EndDate) || value.Kind == "HOTEL" && value.StartDate == value.EndDate {
		return domain.Fail("INVALID_INVOICE_TRAVEL_FACTS", "请按原件填写票种、六位城市和真实起止日期，住宿退房须晚于入住", 422)
	}
	return nil
}

// validTravelPeriod 检查真实日期且允许交通同日起止。
// 参数：start/end为明确业务日期。
// 返回值：两日均真实存在且不倒置时为真。
func validTravelPeriod(start, end string) bool {
	a, e1 := time.Parse("2006-01-02", start)
	b, e2 := time.Parse("2006-01-02", end)
	return e1 == nil && e2 == nil && a.Year() > 0 && b.Year() > 0 && !b.Before(a)
}

// sameInvoiceTravelFacts 比较完整人工票面事实并区分未填写与实际事实。
// 参数：a/b为之前确认和拟输入的票面字段。
// 返回值：两者同为空或所有字段相同时为真。
func sameInvoiceTravelFacts(a, b *domain.InvoiceTravelFacts) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// loadInvoiceTravelChecks 为启用交叉核验的费用读取本人确认票据及原件来源。
// 参数：tx为事务，claim为租户申请人边界，item为当前费用。
// 返回值：可信票面核验输入、是否启用新规则及读取错误；无新字段时兼容旧流程。
func loadInvoiceTravelChecks(tx *gorm.DB, claim domain.Claim, item domain.Item) ([]domain.TravelInvoiceCheck, bool, error) {
	enabled := item.TravelDeclaration != nil
	ids := []string{}
	seen := map[string]bool{}
	for _, usage := range item.InvoiceUsages {
		if !seen[usage.InvoiceID] {
			ids = append(ids, usage.InvoiceID)
			seen[usage.InvoiceID] = true
		}
	}
	if len(ids) == 0 {
		return nil, enabled, nil
	}
	var rows []store.Invoice
	if err := tx.Where("tenant_id = ? AND id IN ?", claim.TenantID, ids).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	values := map[string]domain.Invoice{}
	for _, row := range rows {
		var inv domain.Invoice
		if err := json.Unmarshal([]byte(row.ContentJSON), &inv); err != nil {
			return nil, false, err
		}
		inv.ID, inv.TenantID, inv.Owner = row.ID, row.TenantID, row.Owner
		values[row.ID] = inv
		enabled = enabled || inv.TravelFacts != nil
	}
	if !enabled {
		return nil, false, nil
	}
	if len(rows) != len(ids) {
		return nil, true, domain.Fail("INVOICE_NOT_AVAILABLE", "差旅用票不存在或无权使用", 422)
	}
	checks := make([]domain.TravelInvoiceCheck, 0, len(ids))
	for _, id := range ids {
		inv := values[id]
		if inv.Owner != claim.Applicant || inv.ReviewStatus != "CONFIRMED" {
			return nil, true, domain.Fail("INVOICE_NOT_AVAILABLE", "交叉核验只接受本人已人工确认的票据", 422)
		}
		check, err := confirmedInvoiceTravelCheck(tx, inv)
		if err != nil {
			return nil, true, err
		}
		checks = append(checks, check)
	}
	return checks, true, nil
}

// confirmedInvoiceTravelCheck 固定人工票面事实版本及原件关联摘要。
// 参数：tx为业务事务，inv为已校验归属和确认状态的票据。
// 返回值：票据事实输入；新事实无有效原件来源时拒绝。
func confirmedInvoiceTravelCheck(tx *gorm.DB, inv domain.Invoice) (domain.TravelInvoiceCheck, error) {
	out := domain.TravelInvoiceCheck{InvoiceID: inv.ID, InvoiceVersion: inv.Version, SourceKind: "NOT_PROVIDED", Facts: inv.TravelFacts, SourceIDs: []string{}}
	if inv.TravelFacts == nil {
		return out, nil
	}
	out.SourceKind = "MANUAL_FACE_CONFIRMATION"
	if err := validateInvoiceTravelFacts(inv.TravelFacts); err != nil {
		return out, err
	}
	var sources []store.InvoiceSource
	if err := tx.Where("tenant_id = ? AND invoice_id = ?", inv.TenantID, inv.ID).Order("id").Limit(1001).Find(&sources).Error; err != nil {
		return out, err
	}
	if len(sources) == 0 || len(sources) > 1000 {
		return out, domain.Fail("INVOICE_TRAVEL_SOURCE_REQUIRED", "人工票面行程必须保留有效原件确认来源", 422)
	}
	docIDs := []string{}
	for _, source := range sources {
		if source.SourcesHash != "" && source.SourcesHash != parseSourceHash(source.SourcesJSON) {
			return out, domain.Fail("INVOICE_SOURCE_HASH_MISMATCH", "票面行程的原件来源摘要不一致", 409)
		}
		docIDs = append(docIDs, source.DocumentID)
	}
	var docs []store.Document
	if err := tx.Where("tenant_id = ? AND owner = ? AND id IN ? AND file_id IN ?", inv.TenantID, inv.Owner, docIDs, inv.FileIDs).Find(&docs).Error; err != nil {
		return out, err
	}
	validDocs := map[string]bool{}
	for _, doc := range docs {
		validDocs[doc.ID] = true
	}
	for _, source := range sources {
		if !validDocs[source.DocumentID] {
			return out, domain.Fail("INVOICE_TRAVEL_SOURCE_REQUIRED", "人工票面行程关联的本人原件缺失", 422)
		}
		out.SourceIDs = append(out.SourceIDs, source.ID)
	}
	sort.Strings(out.SourceIDs)
	var err error
	out.FactDigest, err = digest(inv.TravelFacts)
	return out, err
}
