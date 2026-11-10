package application

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type suspectedInvoiceDuplicate struct {
	InvoiceID     string       `json:"invoice_id"`
	InvoiceNumber string       `json:"invoice_number"`
	InvoiceDate   string       `json:"invoice_date"`
	Total         domain.Money `json:"total_minor"`
}

// readableInvoiceRows 复用票据正式读取范围，提示候选与原详情适用同一对象边界。
// 参数：db为数据库，a为可信主体，claims为其既有整单读取范围。
// 返回值：同租户本人、关联可读整单或明确全租户权限的票据查询。
func readableInvoiceRows(db *gorm.DB, a domain.Actor, claims *gorm.DB) *gorm.DB {
	q := db.Model(&store.Invoice{}).Where("tenant_id = ?", a.TenantID)
	if !a.Has("expense.scope.tenant_all") {
		q = q.Where(db.Where("owner = ?", a.SubjectID).
			Or("reservation_claim_id IN (?)", claims.Select("id")))
	}
	return q
}

// usableDuplicateSource 校验风险匹配所需的明确业务身份，未知销售方不作推测。
// 参数：row为已授权票据索引，invoice为当前票面。
// 返回值：是否可按相同日期、总额及销售方检查不同业务身份。
func usableDuplicateSource(row store.Invoice, invoice domain.Invoice) bool {
	if row.IdentityKey == nil || *row.IdentityKey == "" || invoice.IdentityKey != *row.IdentityKey ||
		invoice.Total <= 0 || strings.TrimSpace(invoice.SellerTaxID) == "" ||
		invoice.MergedIntoInvoiceID != "" {
		return false
	}
	if _, err := time.Parse("2006-01-02", invoice.InvoiceDate); err != nil {
		return false
	}
	for _, retired := range []string{"MERGED", "VOID", "VOIDED", "CANCELLED"} {
		if row.ReservationState == retired || invoice.ReviewStatus == retired {
			return false
		}
	}
	return true
}

// duplicateCandidateRows 在授权SQL范围中匹配票面事实，最多读取六条以判断更多。
// 参数：db为数据库，a为主体，source/row为已授权源票及索引。
// 返回值：仅有权查看且不同业务身份的候选，不统计隐藏匹配数量。
func duplicateCandidateRows(db *gorm.DB, a domain.Actor, source domain.Invoice,
	row store.Invoice) ([]store.Invoice, error) {
	claims, err := readableClaims(db, a, false)
	if err != nil {
		return nil, err
	}
	q := readableInvoiceRows(db, a, claims).Where("id <> ? AND identity_key IS NOT NULL", row.ID).
		Where("identity_key <> ? AND identity_key <> ?", "", *row.IdentityKey).
		Where("reservation_state NOT IN ?", []string{"MERGED", "VOID", "VOIDED", "CANCELLED"})
	for field, value := range map[string]string{"invoice_date": source.InvoiceDate,
		"total_minor": strconv.FormatInt(int64(source.Total), 10), "seller_tax_id": source.SellerTaxID} {
		q = q.Where(queryJSONText(db, "content_json", field)+" = ?", value)
	}
	q = q.Where("COALESCE("+queryJSONText(db, "content_json", "merged_into_invoice_id")+", '') = ?", "").
		Where("COALESCE("+queryJSONText(db, "content_json", "review_status")+", '') NOT IN ?",
			[]string{"MERGED", "VOID", "VOIDED", "CANCELLED"}).
		Where(queryJSONText(db, "content_json", "identity_key") + " = identity_key")
	var rows []store.Invoice
	err = q.Order("created_at DESC, id DESC").Limit(6).Find(&rows).Error
	return rows, err
}

// addInvoiceDuplicateWarnings 为已通过正式读取授权的单票详情附加只读风险提示。
// 参数：db为数据库，a为主体，id为当前详情票据，view为已授权详情响应。
// 返回值：候选读取错误；提示不改核验、占用状态或提交规则。
func addInvoiceDuplicateWarnings(db *gorm.DB, a domain.Actor, id string, view map[string]any) error {
	items := []suspectedInvoiceDuplicate{}
	view["suspected_duplicates"], view["suspected_duplicates_more"] = items, false
	var row store.Invoice
	if err := db.Where("tenant_id = ? AND id = ?", a.TenantID, id).First(&row).Error; err != nil {
		return err
	}
	var source domain.Invoice
	if err := json.Unmarshal([]byte(row.ContentJSON), &source); err != nil {
		return err
	}
	if !usableDuplicateSource(row, source) {
		return nil
	}
	rows, err := duplicateCandidateRows(db, a, source, row)
	if err != nil {
		return err
	}
	for _, candidate := range rows {
		var invoice domain.Invoice
		if err = json.Unmarshal([]byte(candidate.ContentJSON), &invoice); err != nil {
			return err
		}
		items = append(items, suspectedInvoiceDuplicate{candidate.ID, invoice.InvoiceNumber,
			invoice.InvoiceDate, invoice.Total})
	}
	view["suspected_duplicates_more"] = len(items) > 5
	if len(items) > 5 {
		items = items[:5]
	}
	view["suspected_duplicates"] = items
	return nil
}
