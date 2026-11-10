package application

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strconv"
	"strings"
	"time"
)

// invoiceRule 读取明确票种政策，禁止解析结果直接代表确认或税务验真。
// 参数：tx 为数据库，tenant 和 invoiceType 为政策边界。
// 返回值：唯一有效规则或配置错误。
func invoiceRule(tx *gorm.DB, tenant, invoiceType string) (domain.InvoicePolicy, error) {
	rows, e := configs(tx, tenant, "invoice_rules", true)
	if e != nil {
		return domain.InvoicePolicy{}, e
	}
	var found *domain.InvoicePolicy
	for _, row := range rows {
		rule, err := decodeInvoicePolicy(row.ContentJSON)
		if err != nil {
			return rule, err
		}
		if rule.Active && rule.InvoiceType == invoiceType {
			if found != nil {
				return rule, domain.Fail("INVOICE_RULE_CONFLICT", "此票种存在多个有效政策", 422)
			}
			copy := rule
			copy.ID = row.ID
			copy.Version = row.Version
			found = &copy
		}
	}
	if found == nil {
		return domain.InvoicePolicy{}, domain.Fail("INVOICE_RULE_MISSING", "此票种尚未配置确认和身份政策", 422)
	}
	return *found, nil
}

// editInvoice 创建待复核票据或追加人工字段修订并确认业务身份。
// 参数：tx 为事务，cc 为本人票据命令，payload 为人工确认内容。
// 返回值：票据、对象标识及错误。
func (s *Service) editInvoice(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input domain.Invoice
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	var extra struct {
		Reason           string                   `json:"reason"`
		SourceSelections []InvoiceSourceSelection `json:"source_selections"`
	}
	if e := json.Unmarshal(payload, &extra); e != nil {
		return nil, "", e
	}
	var inv domain.Invoice
	var row store.Invoice
	var e error
	previous := int64(0)
	if cc.operation == "invoice.create" {
		id, err := newID()
		if err != nil {
			return nil, "", err
		}
		inv = domain.Invoice{ID: id, TenantID: cc.actor.TenantID, Owner: cc.actor.SubjectID, Version: 1, ReviewStatus: "PENDING", VerificationStatus: "NOT_CHECKED", ReservationState: "AVAILABLE"}
	} else {
		e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&row).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "发票不存在", 404)
		}
		if e != nil {
			return nil, "", e
		}
		if row.Owner != cc.actor.SubjectID {
			return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "发票不存在或无权复核", 404)
		}
		if e = verifyVersion(row.Version, cc.input.ExpectedVersion); e != nil {
			return nil, "", e
		}
		if row.ReservationState != "AVAILABLE" {
			return nil, "", domain.Fail("INVOICE_LOCKED", "已占用或消费的票据不可修改确认内容", 409)
		}
		if e = json.Unmarshal([]byte(row.ContentJSON), &inv); e != nil {
			return nil, "", e
		}
		if inv.ReviewStatus == "CONFIRMED" && !sameInvoiceTravelFacts(inv.TravelFacts, input.TravelFacts) {
			return nil, "", domain.Fail("INVOICE_TRAVEL_FACTS_IMMUTABLE", "已确认行程只能通过独立批准的重大更正流程修改", 409)
		}
		previous = row.Version
		inv.Version = row.Version + 1
	}
	inv.InvoiceType = input.InvoiceType
	inv.InvoiceNumber = input.InvoiceNumber
	inv.InvoiceCode = input.InvoiceCode
	inv.InvoiceDate = input.InvoiceDate
	inv.SellerTaxID = input.SellerTaxID
	inv.BuyerTaxID = input.BuyerTaxID
	inv.Total = input.Total
	inv.FileIDs = input.FileIDs
	inv.Content = input.Content
	if e = validateInvoiceContent(inv.Content); e != nil {
		return nil, "", e
	}
	inv.TravelFacts = input.TravelFacts
	if e = validateInvoiceTravelFacts(inv.TravelFacts); e != nil {
		return nil, "", e
	}
	if len(inv.FileIDs) == 0 || len(inv.FileIDs) > 20 {
		return nil, "", domain.Fail("INVOICE_SOURCE_REQUIRED", "票据必须关联本人上传的原件", 422)
	}
	if cc.operation == "invoice.confirm" {
		if extra.Reason == "" {
			return nil, "", domain.Fail("REASON_REQUIRED", "人工确认或校正必须填写理由", 422)
		}
		if e = confirmInvoiceFields(tx, &inv); e != nil {
			return nil, "", e
		}
		if e = linkSelectedInvoiceSources(tx, cc.actor, inv, extra.SourceSelections, extra.Reason); e != nil {
			return nil, "", e
		}
	}
	if cc.operation == "invoice.confirm" {
		var duplicate store.Invoice
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND identity_key = ? AND id <> ?", inv.TenantID, inv.IdentityKey, inv.ID).First(&duplicate).Error
		if err == nil {
			return mergeInvoiceSources(tx, cc, inv, row, duplicate, extra.Reason)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", err
		}
	}
	raw, e := encode(inv)
	if e != nil {
		return nil, "", e
	}
	var identity *string
	if inv.IdentityKey != "" {
		identity = &inv.IdentityKey
	}
	if previous == 0 {
		e = tx.Create(&store.Invoice{ID: inv.ID, TenantID: inv.TenantID, Owner: inv.Owner, IdentityKey: identity, Version: inv.Version, ReservationState: "AVAILABLE", ContentJSON: raw}).Error
	} else {
		revisionID, err := newID()
		if err != nil {
			return nil, "", err
		}
		revision, err := encode(map[string]any{"previous": json.RawMessage(row.ContentJSON), "confirmed": inv, "reason": extra.Reason, "actor_subject_id": cc.actor.SubjectID})
		if err != nil {
			return nil, "", err
		}
		if err = tx.Create(&store.InvoiceRevision{ID: revisionID, TenantID: inv.TenantID, InvoiceID: inv.ID, Version: inv.Version, ContentJSON: revision}).Error; err != nil {
			return nil, "", err
		}
		e = tx.Model(&store.Invoice{}).Where("tenant_id = ? AND id = ? AND version = ?", inv.TenantID, inv.ID, previous).Updates(map[string]any{"version": inv.Version, "identity_key": identity, "content_json": raw}).Error
	}
	if e != nil {
		return nil, "", domain.Fail("INVOICE_IDENTITY_CONFLICT", "票据确认身份已存在，请检查原件及本人台账", 409)
	}
	if cc.operation == "invoice.create" && len(extra.SourceSelections) > 0 {
		if e = linkSelectedInvoiceSources(tx, cc.actor, inv, extra.SourceSelections, extra.Reason); e != nil {
			return nil, "", e
		}
	}
	if cc.operation == "invoice.confirm" {
		if e = LinkManualInvoiceSources(tx, inv, extra.Reason); e != nil {
			return nil, "", e
		}
	}
	return inv, inv.ID, nil
}

// confirmInvoiceFields 共用正式确认和重大更正的票种、身份、金额及购买方规则。
// 参数：tx 为事务，inv 为当前归属已由服务器固定的完整票面。
// 返回值：确认身份摘要；必要字段、日期、金额或政策异常时拒绝。
func confirmInvoiceFields(tx *gorm.DB, inv *domain.Invoice) error {
	if e := validateInvoiceContent(inv.Content); e != nil {
		return e
	}
	if e := validateInvoiceTravelFacts(inv.TravelFacts); e != nil {
		return e
	}
	rule, e := invoiceRule(tx, inv.TenantID, inv.InvoiceType)
	if e != nil {
		return e
	}
	if inv.Total <= 0 {
		return domain.Fail("INVALID_INVOICE_AMOUNT", "票面金额必须大于零", 422)
	}
	if _, e = time.Parse("2006-01-02", inv.InvoiceDate); e != nil {
		return domain.Fail("INVALID_DATE", "发票日期不正确", 422)
	}
	if rule.RequiredBuyerTaxID && strings.TrimSpace(inv.BuyerTaxID) == "" {
		return domain.Fail("INVOICE_BUYER_REQUIRED", "此票种必须确认购买方税号", 422)
	}
	fields := map[string]string{"invoice_type": inv.InvoiceType, "invoice_code": inv.InvoiceCode, "invoice_number": inv.InvoiceNumber, "invoice_date": inv.InvoiceDate, "seller_tax_id": inv.SellerTaxID, "total_minor": strconv.FormatInt(int64(inv.Total), 10)}
	identity := []string{}
	if len(rule.IdentityFields) == 0 {
		return domain.Fail("INVOICE_IDENTITY_INCOMPLETE", "票种未配置业务身份字段", 422)
	}
	for _, field := range rule.IdentityFields {
		if strings.TrimSpace(fields[field]) == "" {
			return domain.Fail("INVOICE_IDENTITY_INCOMPLETE", "票据业务身份字段尚未完整确认", 422)
		}
		identity = append(identity, field, fields[field])
	}
	inv.IdentityKey, e = digest(identity)
	inv.ReviewStatus = "CONFIRMED"
	return e
}

// mergeInvoiceSources 在同归属同票面确认后保留 PDF/OFD 两个来源而不重复建立业务身份。
// 参数：tx 为事务，cc 为可信命令，candidate 为新确认票据，source/target 为候选与已确认行，reason 为人工依据。
// 返回值：归并后的权威票据或冲突。
func mergeInvoiceSources(tx *gorm.DB, cc *commandContext, candidate domain.Invoice, source, target store.Invoice, reason string) (any, string, error) {
	if target.Owner != cc.actor.SubjectID || target.ReservationState != "AVAILABLE" {
		return nil, "", domain.Fail("INVOICE_IDENTITY_CONFLICT", "票据身份已有记录，已占用来源需走受控补充证据流程", 409)
	}
	var existing domain.Invoice
	if e := json.Unmarshal([]byte(target.ContentJSON), &existing); e != nil {
		return nil, "", e
	}
	if existing.Total != candidate.Total || existing.BuyerTaxID != candidate.BuyerTaxID || existing.SellerTaxID != candidate.SellerTaxID || existing.InvoiceDate != candidate.InvoiceDate || existing.InvoiceCode != candidate.InvoiceCode {
		return nil, "", domain.Fail("INVOICE_FIELD_CONFLICT", "同票不同来源的金额、日期或购销方存在冲突，请人工核实", 409)
	}
	if !sameInvoiceTravelFacts(existing.TravelFacts, candidate.TravelFacts) {
		return nil, "", domain.Fail("INVOICE_FIELD_CONFLICT", "同票来源的人工行程事实不同，请走受控更正核实", 409)
	}
	if !sameInvoiceContent(existing.Content, candidate.Content) {
		return nil, "", domain.Fail("INVOICE_FIELD_CONFLICT", "同票来源的人工商品明细不同，请走受控更正核实", 409)
	}
	seen := map[string]bool{}
	for _, id := range existing.FileIDs {
		seen[id] = true
	}
	for _, id := range candidate.FileIDs {
		if !seen[id] {
			existing.FileIDs = append(existing.FileIDs, id)
			seen[id] = true
		}
	}
	if len(existing.FileIDs) > 20 {
		return nil, "", domain.Fail("FILE_REFERENCE_LIMIT", "归并后的原件引用最多20个，请受控整理证据", 422)
	}
	existing.Version = target.Version + 1
	raw, e := encode(existing)
	if e != nil {
		return nil, "", e
	}
	if e = tx.Model(&store.Invoice{}).Where("tenant_id = ? AND id = ? AND version = ?", candidate.TenantID, target.ID, target.Version).Updates(map[string]any{"version": existing.Version, "content_json": raw}).Error; e != nil {
		return nil, "", e
	}
	candidate.ReviewStatus = "DISPUTED"
	candidate.IdentityKey = ""
	candidate.MergedIntoInvoiceID = target.ID
	candidateJSON, e := encode(candidate)
	if e != nil {
		return nil, "", e
	}
	if e = tx.Model(&store.Invoice{}).Where("tenant_id = ? AND id = ? AND version = ?", candidate.TenantID, source.ID, source.Version).Updates(map[string]any{"version": candidate.Version, "content_json": candidateJSON}).Error; e != nil {
		return nil, "", e
	}
	revisionID, e := newID()
	if e != nil {
		return nil, "", e
	}
	revision, e := encode(map[string]any{"previous": json.RawMessage(target.ContentJSON), "merged": existing, "candidate_id": source.ID, "reason": reason, "actor_subject_id": cc.actor.SubjectID})
	if e != nil {
		return nil, "", e
	}
	e = tx.Create(&store.InvoiceRevision{ID: revisionID, TenantID: candidate.TenantID, InvoiceID: target.ID, Version: existing.Version, ContentJSON: revision}).Error
	if e == nil {
		e = LinkManualInvoiceSources(tx, candidate, reason)
	}
	if e == nil {
		e = CopyInvoiceSources(tx, candidate.TenantID, source.ID, target.ID)
	}
	return existing, target.ID, e
}

type VerificationInput struct {
	CommandInput
	Method          string   `json:"verification_method"`
	Result          string   `json:"verification_result"`
	Reason          string   `json:"reason"`
	EvidenceFileIDs []string `json:"evidence_file_ids"`
}

// verifyInvoice 记录带证据人工核验，外部税务查验未接入时明确失败关闭。
// 参数：tx 为事务，cc 为具有当前财务范围的核验人，payload 为核验方法和证据。
// 返回值：真实核验记录与票据状态。
func (s *Service) verifyInvoice(tx *gorm.DB, cc *commandContext, payload json.RawMessage) (any, string, error) {
	var input VerificationInput
	if e := json.Unmarshal(payload, &input); e != nil {
		return nil, "", e
	}
	if input.Method != "MANUAL" {
		return nil, "", domain.Fail("VERIFICATION_PROVIDER_UNAVAILABLE", "尚未接入正式外部查验供应商", 503)
	}
	if input.Reason == "" || len(input.EvidenceFileIDs) == 0 || (input.Result != "PASS" && input.Result != "MISMATCH" && input.Result != "UNKNOWN") {
		return nil, "", domain.Fail("VERIFICATION_EVIDENCE_REQUIRED", "请填写人工核验依据、结论及证据原件", 422)
	}
	var row store.Invoice
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", cc.actor.TenantID, cc.resourceID).First(&row).Error; e != nil {
		return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "发票不存在", 404)
	}
	if e := verifyVersion(row.Version, input.ExpectedVersion); e != nil {
		return nil, "", e
	}
	if row.Owner == cc.actor.SubjectID {
		return nil, "", domain.Fail("SELF_VERIFICATION_FORBIDDEN", "票据所有人不能为本人执行财务核验", 403)
	}
	if row.Owner != cc.actor.SubjectID {
		if row.ReservationClaimID == "" {
			return nil, "", domain.Fail("RESOURCE_NOT_FOUND", "票据尚未进入可核验财务范围", 404)
		}
		c, _, e := loadClaim(tx, cc.actor.TenantID, row.ReservationClaimID, false)
		if e != nil {
			return nil, "", e
		}
		if e = requireFinancial(tx, cc.actor, c, "expense.invoice.verify"); e != nil {
			return nil, "", e
		}
	}
	var invoice domain.Invoice
	if e := json.Unmarshal([]byte(row.ContentJSON), &invoice); e != nil {
		return nil, "", e
	}
	invoice.VerificationStatus = "MANUAL_REVIEWED"
	if input.Result == "MISMATCH" {
		invoice.VerificationStatus = "MISMATCH"
	}
	invoice.Version = row.Version + 1
	raw, e := encode(invoice)
	if e != nil {
		return nil, "", e
	}
	id, e := newID()
	if e != nil {
		return nil, "", e
	}
	evidence, e := encode(input)
	if e != nil {
		return nil, "", e
	}
	if e = tx.Create(&store.Verification{ID: id, TenantID: row.TenantID, InvoiceID: row.ID, Actor: cc.actor.SubjectID, Method: "MANUAL", Result: input.Result, ContentJSON: evidence}).Error; e != nil {
		return nil, "", e
	}
	if e = tx.Model(&store.Invoice{}).Where("id = ? AND version = ?", row.ID, row.Version).Updates(map[string]any{"version": invoice.Version, "content_json": raw}).Error; e != nil {
		return nil, "", e
	}
	if input.Result == "MISMATCH" && row.ReservationClaimID != "" {
		if e = appendOutbox(tx, row.TenantID, "notification", row.ReservationClaimID, map[string]any{"kind": "INVOICE_MISMATCH", "assignee_subject_id": row.Owner, "invoice_id": row.ID, "claim_id": row.ReservationClaimID}); e != nil {
			return nil, "", e
		}
	}
	return map[string]any{"id": id, "invoice_id": invoice.ID, "version": invoice.Version, "verification_status": invoice.VerificationStatus}, invoice.ID, nil
}

// InvoiceSources 返回授权票据原件的有界解析来源摘要，过滤上传票据等敏感元数据。
// 参数：ctx 为上下文，a 为当前身份，invoiceID 为授权票据。
// 返回值：文件引用、解析器版本、页坐标或 XML 路径及诊断。
func (s *Service) InvoiceSources(ctx context.Context, a domain.Actor, invoiceID string) (json.RawMessage, error) {
	raw, e := s.Query(ctx, a, "invoice", invoiceID, nil)
	if e != nil {
		return nil, e
	}
	var invoice domain.Invoice
	if e = json.Unmarshal(raw, &invoice); e != nil {
		return nil, e
	}
	frozen, found, e := s.frozenInvoiceSources(ctx, invoice)
	if e != nil {
		return nil, e
	}
	if found {
		return json.Marshal(map[string]any{"items": frozen, "file_ids": invoice.FileIDs})
	}
	items := []any{}
	for _, fileID := range invoice.FileIDs {
		var doc store.Document
		if e = s.db.WithContext(ctx).Where("tenant_id = ? AND file_id = ?", a.TenantID, fileID).First(&doc).Error; e != nil {
			return nil, domain.Fail("INVOICE_SOURCE_MISSING", "票据原件来源记录缺失", 409)
		}
		var metadata struct {
			Filename  string `json:"filename"`
			MediaType string `json:"media_type"`
			Request   struct {
				Filename     string `json:"filename"`
				OriginalName string `json:"original_name"`
			} `json:"request"`
			ParseResult struct {
				Status        string   `json:"status"`
				ParserVersion string   `json:"parser_version"`
				RulesVersion  string   `json:"rules_version"`
				Warnings      []string `json:"warnings"`
				Documents     []struct {
					Number  string          `json:"invoice_number"`
					Sources json.RawMessage `json:"sources"`
					Issues  []string        `json:"issues"`
				} `json:"documents"`
			} `json:"parse_result"`
		}
		if e = json.Unmarshal([]byte(doc.ContentJSON), &metadata); e != nil {
			return nil, e
		}
		sources := []json.RawMessage{}
		issues := append([]string{}, metadata.ParseResult.Warnings...)
		for _, candidate := range metadata.ParseResult.Documents {
			if candidate.Number == invoice.InvoiceNumber && candidate.Number != "" {
				var selected []json.RawMessage
				if e := json.Unmarshal(candidate.Sources, &selected); e != nil {
					return nil, e
				}
				sources = append(sources, selected...)
				issues = append(issues, candidate.Issues...)
			}
		}
		name := metadata.Filename
		if name == "" {
			name = metadata.Request.Filename
		}
		if name == "" {
			name = metadata.Request.OriginalName
		}
		items = append(items, map[string]any{"file_id": fileID, "document_id": doc.ID, "original_name": name, "media_type": metadata.MediaType, "source_kind": "LEGACY_UNVERIFIED", "parse_status": metadata.ParseResult.Status, "parser_version": metadata.ParseResult.ParserVersion, "rules_version": metadata.ParseResult.RulesVersion, "sources": sources, "issues": issues})
	}
	return json.Marshal(map[string]any{"items": items, "file_ids": invoice.FileIDs})
}
