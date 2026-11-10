package invoiceparser

import (
	"context"
	"golang.org/x/text/encoding/simplifiedchinese"
	"slices"
	"strings"
	"testing"
)

// structuredOFDFixture 构造真实库可读取的最小合成结构，不能代替授权票样。
// 参数：t为测试上下文，attachment为结构化字段，multi控制多个DocBody。
// 返回值：内存OFD容器。
func structuredOFDFixture(t *testing.T, attachment string, multi bool) []byte {
	root := `<OFD><DocBody><DocRoot>Doc_0/Document.xml</DocRoot></DocBody>`
	names := []string{"OFD.xml", "Doc_0/Document.xml", "Doc_0/Pages/Page_0/Content.xml", "Doc_0/Attachs/original_invoice.xml"}
	document := `<Document><CommonData><MaxUnitID>10</MaxUnitID><PageArea><PhysicalBox>0 0 210 297</PhysicalBox></PageArea></CommonData><Pages><Page ID="1" BaseLoc="Pages/Page_0/Content.xml"/></Pages></Document>`
	contents := []string{"", document, `<Page><Content/></Page>`, attachment}
	if multi {
		root += `<DocBody><DocRoot>Doc_1/Document.xml</DocRoot></DocBody>`
		names = append(names, "Doc_1/Document.xml")
		contents = append(contents, document)
	}
	contents[0] = root + `</OFD>`
	return ofdTestContainer(t, names, contents, false)
}

// structuredInvoiceXML 提供精确金额及双方相似税号的合成结构化字段。
// 参数：无。
// 返回值：单张结构化发票XML。
func structuredInvoiceXML() string {
	return `<Invoice><InvoiceCode>123456789012</InvoiceCode><InvoiceNo>12345678</InvoiceNo><IssueDate>2026-11-10</IssueDate><Buyer><BuyerName>甲方公司</BuyerName><BuyerTaxID>91310100AAAAAA0001</BuyerTaxID></Buyer><Seller><SellerName>乙方公司</SellerName><SellerTaxID>91310100AAAAAA0002</SellerTaxID></Seller><TaxExclusiveTotalAmount>100.00</TaxExclusiveTotalAmount><TaxTotalAmount>13.00</TaxTotalAmount><TaxInclusiveTotalAmount>113.00</TaxInclusiveTotalAmount><GoodsInfos><G1><Item>服务</Item><Quantity>1.000</Quantity><Price>100.000</Price><Amount>100.00</Amount><TaxScheme>0.13</TaxScheme><TaxAmount>13.00</TaxAmount></G1></GoodsInfos></Invoice>`
}

// TestOFDStructuredAmountsSourcesAndMultipleDocuments 验证单Extract不能吞掉多文档及结构化金额来源。
// 参数：t为测试上下文。
// 返回值：无；金额来源、人工状态或多文档边界丢失时失败。
func TestOFDStructuredAmountsSourcesAndMultipleDocuments(t *testing.T) {
	p := New(Limits{})
	result, err := p.Parse(context.Background(), structuredOFDFixture(t, structuredInvoiceXML(), false), "application/ofd")
	if err != nil {
		t.Fatal(err)
	}
	c := result.Documents[0]
	if c.AmountMinor != "10000" || c.TaxMinor != "1300" || c.GrossMinor != "11300" || len(c.DetailLines) != 1 || c.BuyerTaxID != "91310100AAAAAA0001" || c.SellerTaxID != "91310100AAAAAA0002" {
		t.Fatalf("bad fields %+v", c)
	}
	if c.ReviewStatus != "PENDING" || c.VerificationStatus != "NOT_CHECKED" || !strings.Contains(c.FieldCandidates["gross_minor"][0].Sources[0].XMLPath, "/TaxInclusiveTotalAmount[1]") {
		t.Fatal("missing sources/manual status", c)
	}
	result, err = p.Parse(context.Background(), structuredOFDFixture(t, structuredInvoiceXML(), true), "application/ofd")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Documents) != 1 || result.Documents[0].GroupKind != "OFD_MULTIPLE_DOCUMENTS" || result.Documents[0].GrossMinor != "" {
		t.Fatalf("multi %+v", result)
	}
}

// TestOFDConflictingFieldsAreRetained 验证重复XML金额与库选择结果不能覆盖候选冲突。
// 参数：t为测试上下文。
// 返回值：无；不同值未完整保留时失败。
func TestOFDConflictingFieldsAreRetained(t *testing.T) {
	raw := strings.Replace(structuredInvoiceXML(), "</Invoice>", "<TaxInclusiveTotalAmount>114.00</TaxInclusiveTotalAmount></Invoice>", 1)
	result, err := New(Limits{}).Parse(context.Background(), structuredOFDFixture(t, raw, false), "application/ofd")
	if err != nil {
		t.Fatal(err)
	}
	c := result.Documents[0]
	if c.GrossMinor != "" || len(c.FieldCandidates["gross_minor"]) != 2 {
		t.Fatalf("overwritten %+v", c)
	}
}

// TestOFDGBKPreflightMatchesLockedLibrary 验证安全预检兼容锁定库已支持的GBK结构化附件。
// 参数：t为测试上下文。
// 返回值：无；合法受限编码被预检误拒时失败。
func TestOFDGBKPreflightMatchesLockedLibrary(t *testing.T) {
	raw := `<?xml version="1.0" encoding="GBK"?>` + structuredInvoiceXML()
	encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	result, err := New(Limits{}).Parse(context.Background(), structuredOFDFixture(t, string(encoded), false), "application/ofd")
	if err != nil {
		t.Fatal(err)
	}
	if result.Documents[0].BuyerName != "甲方公司" {
		t.Fatal("GBK name decode mismatch")
	}
}

// TestOFDMultipleAttachmentsAndAmountRelations 验证一个文档的多个结构化票源仍需人工拆分。
// 参数：t为测试上下文。
// 返回值：无；单Extract掩盖第二票或金额关系未标记时失败。
func TestOFDMultipleAttachmentsAndAmountRelations(t *testing.T) {
	names := []string{"OFD.xml", "Doc_0/Document.xml", "Doc_0/Attachs/original_invoice.xml", "Doc_0/Attachs/second.xml"}
	contents := []string{`<OFD><DocBody><DocRoot>Doc_0/Document.xml</DocRoot></DocBody></OFD>`, `<Document><Pages><Page ID="1" BaseLoc="Pages/Page_0/Content.xml"/></Pages></Document>`, structuredInvoiceXML(), strings.Replace(structuredInvoiceXML(), "12345678</InvoiceNo>", "87654321</InvoiceNo>", 1)}
	result, err := New(Limits{}).Parse(context.Background(), ofdTestContainer(t, names, contents, false), "application/ofd")
	if err != nil {
		t.Fatal(err)
	}
	if result.Documents[0].GroupKind != "OFD_MULTIPLE_DOCUMENTS" || len(result.Documents[0].FieldCandidates["invoice_number"]) != 2 {
		t.Fatal("second attachment lost")
	}
	raw := strings.Replace(structuredInvoiceXML(), ">113.00</TaxInclusiveTotalAmount>", ">114.00</TaxInclusiveTotalAmount>", 1)
	result, err = New(Limits{}).Parse(context.Background(), structuredOFDFixture(t, raw, false), "application/ofd")
	if err != nil {
		t.Fatal(err)
	}
	c := result.Documents[0]
	if !slices.Contains(c.Issues, "AMOUNT_CONFLICT") || c.DetailLines[0].Quantity != "1.000" || c.DetailLines[0].UnitPrice != "100.000" {
		t.Fatal("amount relationship/precision", c.Issues, c.DetailLines)
	}
}

// TestOFDPartialStructuredFactsSurviveLibraryAbsence 用途：验证库不认识的不完整结构化字段仍可供人工复核。
// 参数：t 为两类合成OFD测试上下文，分别模拟附件不完整及结构字段位于非附件目录。
// 返回值：无；已定位的冲突字段被错误丢弃或库失败未披露时失败。
func TestOFDPartialStructuredFactsSurviveLibraryAbsence(t *testing.T) {
	for _, tc := range []struct{ path, issue string }{
		{"Doc_0/Attachs/original_invoice.xml", "OFD_LIBRARY_NOT_INVOICE"},
		{"Doc_0/Metadata/partial.xml", "OFD_LIBRARY_NO_ATTACHMENT"},
	} {
		t.Run(tc.issue, func(t *testing.T) {
			data := ofdTestContainer(t, []string{"OFD.xml", "Doc_0/Document.xml", tc.path},
				[]string{`<OFD><DocBody><DocRoot>Doc_0/Document.xml</DocRoot></DocBody></OFD>`,
					`<Document><Pages><Page ID="1"/></Pages></Document>`,
					`<Invoice><InvoiceNo>12345678</InvoiceNo><BuyerName>甲公司</BuyerName><BuyerName>乙公司</BuyerName></Invoice>`}, false)
			result, err := New(Limits{}).Parse(context.Background(), data, "application/ofd")
			if err != nil {
				t.Fatal("partial XML facts must remain available", err)
			}
			c := result.Documents[0]
			if result.Status != "NEEDS_REVIEW" || c.Number != "12345678" || c.BuyerName != "" ||
				len(c.FieldCandidates["buyer_name"]) != 2 ||
				!slices.Contains(c.Issues, tc.issue) ||
				!slices.Contains(c.Issues, "FIELD_MISSING:gross_minor") ||
				c.ReviewStatus != "PENDING" || c.VerificationStatus != "NOT_CHECKED" {
				t.Fatal("partial conflict/reason/review invariants missing", c.Issues)
			}
		})
	}
}

// TestOFDCustomTagCandidateKeepsLinkableSources 用途：验证纯ObjectRef标引结果仍有可关联的原件来源。
// 参数：t 为最小合成自定义标引测试上下文，不代表真实票面准确率。
// 返回值：无；库已提取字段但候选总来源为空或冒称精确叶节点时失败。
func TestOFDCustomTagCandidateKeepsLinkableSources(t *testing.T) {
	names := []string{"OFD.xml", "Doc_0/Document.xml", "Doc_0/Tags/list.xml",
		"Doc_0/Tags/invoice.xml", "Doc_0/Pages/Page_0/Content.xml"}
	contents := []string{
		`<OFD><DocBody><DocRoot>Doc_0/Document.xml</DocRoot></DocBody></OFD>`,
		`<Document><CommonData><MaxUnitID>10</MaxUnitID><PageArea><PhysicalBox>0 0 210 297</PhysicalBox></PageArea></CommonData><Pages><Page ID="1" BaseLoc="Pages/Page_0/Content.xml"/></Pages><CustomTags>Tags/list.xml</CustomTags></Document>`,
		`<CustomTags><CustomTag TypeID="invoice"><FileLoc>invoice.xml</FileLoc></CustomTag></CustomTags>`,
		`<root><InvoiceNo><ObjectRef PageRef="1">2</ObjectRef></InvoiceNo><TaxInclusiveTotalAmount><ObjectRef PageRef="1">3</ObjectRef></TaxInclusiveTotalAmount></root>`,
		`<Page><Content><Layer ID="1"><TextObject ID="2" Boundary="0 0 50 5" Font="1" Size="3"><TextCode>12345678</TextCode></TextObject><TextObject ID="3" Boundary="0 10 50 5" Font="1" Size="3"><TextCode>113.00</TextCode></TextObject></Layer></Content></Page>`,
	}
	result, err := New(Limits{}).Parse(context.Background(), ofdTestContainer(t, names, contents, false), "application/ofd")
	if err != nil {
		t.Fatal(err)
	}
	c := result.Documents[0]
	if c.Number != "12345678" || c.GrossMinor != "11300" || len(c.Sources) == 0 {
		t.Fatal("custom-tag fields must remain linkable", c.Number, c.GrossMinor, len(c.Sources))
	}
	for _, source := range c.Sources {
		if source.XMLPath != "Doc_0/Tags/invoice.xml" || source.Method != "OFD_LIBRARY_ATTACHMENT_OR_CUSTOM_TAG" {
			t.Fatal("custom-tag source precision overstated", source)
		}
	}
}
