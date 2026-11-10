package invoiceparser

import (
	"encoding/json"
	"errors"
	"strings"

	ofd "github.com/zc310/ofd/pkg/invoice"
)

// addOFDFact 归一结构化XML字段并保留原始路径，非法字段只记录问题。
// 参数：c为票据候选，field为统一字段名，raw/source为原值及来源。
// 返回值：无；缺失及冲突不通过库结果覆盖。
func addOFDFact(c *Candidate, field, raw string, source Source) {
	value := normalizeText(raw)
	if strings.HasPrefix(field, "detail.") {
		return
	}
	if strings.HasSuffix(field, "_minor") {
		addMoneyField(c, field, raw, source)
		return
	}
	if field == "invoice_date" {
		value = canonicalDate(raw)
		if value == "" && raw != "" {
			addIssue(c, "INVOICE_DATE_INVALID")
		}
	}
	if strings.HasSuffix(field, "_tax_id") {
		value = strings.ToUpper(compactText(value))
		if value != "" && !taxIDPattern.MatchString(value) {
			addIssue(c, "PARTY_TAX_ID_FORMAT_REQUIRES_REVIEW")
		}
	}
	addField(c, field, value, raw, source)
}

// parseOFD 枚举多文档后调用锁定单票Extract，并统一来源、金额及人工状态。
// 参数：data为安全预检后的原件，inventory为完整容器的有界索引。
// 返回值：单票候选或多文档人工处置组；提取失败转成稳定业务原因。
func (p *Parser) parseOFD(data []byte, inventory ofdInventory) (Result, error) {
	c := newCandidate()
	for _, fact := range inventory.Facts {
		field := ofdFieldName(fact.Name, fact.Source.XMLPath)
		addOFDFact(&c, field, fact.Value, fact.Source)
		c.Sources = append(c.Sources, fact.Source)
	}
	result := Result{Status: "NEEDS_REVIEW", ParserVersion: "ofd-b4255b7d5424", RulesVersion: rulesVersion, NormalizationVersion: normalizationVersion, Warnings: []string{"OFD_EXTRACTION_IS_NOT_SIGNATURE_OR_TAX_VERIFICATION"}}
	if inventory.DocBodies > 1 || len(inventory.DocumentRoots) > 1 || len(inventory.InvoiceFiles) > 1 || len(c.FieldCandidates["invoice_number"]) > 1 {
		c.GroupKind = "OFD_MULTIPLE_DOCUMENTS"
		addIssue(&c, "MULTIPLE_OFD_DOCUMENTS_REQUIRE_MANUAL_SPLIT")
		c.Raw, _ = json.Marshal(inventory)
		result.Documents = []Candidate{c}
		return result, nil
	}
	invoice, err := ofd.Extract(data)
	if err != nil {
		if errors.Is(err, ofd.ErrNoAttachment) || errors.Is(err, ofd.ErrNotInvoice) {
			return partialOFDResult(c, inventory, result, err)
		}
		return Result{}, errors.New("INVALID_OFD_INVOICE")
	}
	addOFDLibraryFields(&c, invoice)
	c.DetailLines = ofdStructuredDetails(&c, inventory)
	if len(c.DetailLines) == 0 {
		for _, detail := range invoice.Details {
			c.DetailLines = append(c.DetailLines, ofdLibraryDetail(&c, detail, invoice.Attachment))
		}
	}
	for _, warning := range invoice.Warnings {
		addIssue(&c, "OFD_LIBRARY_WARNING:"+warning)
	}
	resolveFields(&c)
	c.Raw, _ = json.Marshal(struct {
		Invoice   *ofd.Invoice `json:"library_invoice"`
		Inventory ofdInventory `json:"inventory"`
	}{invoice, inventory})
	result.Documents = []Candidate{c}
	return result, nil
}

// partialOFDResult 用途：保留安全预检已定位的部分结构化字段，同时披露库无法识别的原因。
// 参数：c 为XML候选，inventory 为有界来源索引，result 为人工复核信封，cause 为库失败原因。
// 返回值：存在字段时返回明确待复核候选；没有字段时仍以NO_STRUCTURED_INVOICE失败。
func partialOFDResult(c Candidate, inventory ofdInventory, result Result, cause error) (Result, error) {
	if len(c.FieldCandidates) == 0 {
		return Result{}, errors.New("NO_STRUCTURED_INVOICE")
	}
	code := "OFD_LIBRARY_NOT_INVOICE"
	if errors.Is(cause, ofd.ErrNoAttachment) {
		code = "OFD_LIBRARY_NO_ATTACHMENT"
	}
	addIssue(&c, code)
	c.DetailLines = ofdStructuredDetails(&c, inventory)
	resolveFields(&c)
	c.Raw, _ = json.Marshal(struct {
		Inventory    ofdInventory `json:"inventory"`
		LibraryError string       `json:"library_error"`
	}{inventory, code})
	result.Warnings = append(result.Warnings, "PARTIAL_STRUCTURED_FIELDS_REQUIRE_REVIEW")
	result.Documents = []Candidate{c}
	return result, nil
}

// addOFDLibraryFields 补充库支持的自定义标引字段，不声称库推断值拥有精确叶路径。
// 参数：c为已收集XML候选，invoice为锁定库的单票结果。
// 返回值：无；不同值仍作为并存候选。
func addOFDLibraryFields(c *Candidate, invoice *ofd.Invoice) {
	source := Source{XMLPath: invoice.Attachment, Method: "OFD_LIBRARY_ATTACHMENT_OR_CUSTOM_TAG"}
	for _, item := range []struct{ field, value string }{{"invoice_type", invoice.Type}, {"invoice_code", invoice.Code}, {"invoice_number", invoice.Number}, {"invoice_date", invoice.Date}, {"buyer_name", invoice.Buyer.Name}, {"buyer_tax_id", invoice.Buyer.Code}, {"seller_name", invoice.Seller.Name}, {"seller_tax_id", invoice.Seller.Code}, {"amount_minor", decimalText(invoice.Amount)}, {"tax_minor", decimalText(invoice.TaxAmount)}, {"gross_minor", decimalText(invoice.TotalAmount)}} {
		if item.value == "" {
			continue
		}
		source.Text = item.value
		addOFDFact(c, item.field, item.value, source)
		source.Field = item.field
		c.Sources = append(c.Sources, source)
	}
}

// decimalText 保留第三方十进制值，不经过浮点或强制金额舍入。
// 参数：value为可空精确十进制。
// 返回值：库规范十进制字符串；缺失值返回空。
func decimalText(value *ofd.Decimal) string {
	if value == nil {
		return ""
	}
	return value.String()
}

// ofdStructuredDetails 按实际XML父节点重建明细，重复叶值全部保留。
// 参数：c为文件诊断容器，inventory为XML叶来源索引。
// 返回值：保留数量、单价和税率原始精度的明细集合。
func ofdStructuredDetails(c *Candidate, inventory ofdInventory) []DetailLine {
	rows := map[string]*Candidate{}
	order := []string{}
	for _, fact := range inventory.Facts {
		field := ofdFieldName(fact.Name, fact.Source.XMLPath)
		if !strings.HasPrefix(field, "detail.") {
			continue
		}
		row := fact.Source.XMLPath[:strings.LastIndex(fact.Source.XMLPath, "/")]
		if rows[row] == nil {
			candidate := newCandidate()
			rows[row] = &candidate
			order = append(order, row)
		}
		addField(rows[row], strings.TrimPrefix(field, "detail."), normalizeText(fact.Value), fact.Value, fact.Source)
	}
	result := []DetailLine{}
	for _, row := range order {
		candidate := rows[row]
		values := map[string]string{}
		sources := []Source{}
		issues := []string{}
		for field, candidates := range candidate.FieldCandidates {
			if len(candidates) == 1 {
				values[field] = candidates[0].Value
			} else {
				issues = append(issues, "DETAIL_FIELD_CONFLICT:"+field)
				addIssue(c, "DETAIL_FIELD_CONFLICT")
			}
			for _, value := range candidates {
				sources = append(sources, value.Sources...)
			}
		}
		detail := detailFromCells(c, values, sources)
		detail.FieldCandidates = candidate.FieldCandidates
		detail.Issues = append(detail.Issues, issues...)
		result = append(result, detail)
	}
	return result
}

// ofdLibraryDetail 适配仅有自定义标引的库明细，并准确标记只能定位到来源文件。
// 参数：c为文件诊断，detail为库明细，attachment为实际标引文件。
// 返回值：金额精确归一且来源能力明确的明细。
func ofdLibraryDetail(c *Candidate, detail ofd.Detail, attachment string) DetailLine {
	values := map[string]string{"name": detail.Name, "model": detail.Model, "unit": detail.Unit, "quantity": decimalText(detail.Count), "unit_price": decimalText(detail.Price), "amount": decimalText(detail.Amount), "tax_rate": decimalText(detail.TaxRate), "tax": decimalText(detail.TaxAmount)}
	return detailFromCells(c, values, []Source{{XMLPath: attachment, Method: "OFD_LIBRARY_CUSTOM_TAG_DETAIL"}})
}
