package invoiceparser

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	ofd "github.com/zc310/ofd/pkg/invoice"
)

// TestPDFTableReferenceFields 验证脱敏数电票版式的字段、分列合计和跨行名称。
// 参数：t 为测试上下文，夹具只保留授权样票几何并替换身份信息。
// 返回值：无；任一票面字段缺失、串列或续行变成伪明细时失败。
func TestPDFTableReferenceFields(t *testing.T) {
	data, err := os.ReadFile("testdata/invoice-table-layout.xhtml")
	if err != nil {
		t.Fatal(err)
	}
	pages, err := parsePopplerLayout(data)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adaptPDFPages(context.Background(), pages)
	if err != nil || len(result.Documents) != 1 {
		t.Fatalf("候选解析失败：%v", err)
	}
	c := result.Documents[0]
	encoded, _ := json.Marshal(c)
	var fields map[string]any
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{
		"invoice_number": "12345678901234567890", "invoice_date": "2026-11-10",
		"seller_name": "销售方测试企业", "seller_tax_id": "91330100BBBBBB0002",
		"buyer_name": "购买方测试企业", "buyer_tax_id": "91330100AAAAAA0001",
		"amount_minor": "8850", "tax_minor": "1150", "gross_minor": "10000",
		"issuer": "测试开票人",
	} {
		if fields[field] != want {
			t.Errorf("%s = %v，期望 %q", field, fields[field], want)
		}
	}
	if len(c.DetailLines) != 1 {
		t.Fatalf("明细行数 = %d，期望 1；问题 %v", len(c.DetailLines), c.Issues)
	}
	line := c.DetailLines[0]
	if line.Name != "*汽油*95号车用汽油(VIB)" || line.Unit != "升" ||
		line.Quantity != "10.97" || line.UnitPrice != "8.0674567" ||
		line.AmountMinor != "8850" || line.TaxRate != "13%" || line.TaxMinor != "1150" {
		t.Errorf("明细字段错误：%+v", line)
	}
	if c.ReviewStatus != "PENDING" || c.VerificationStatus != "NOT_CHECKED" ||
		!slices.Equal(c.Issues, []string{"MANUAL_CONFIRMATION_REQUIRED"}) {
		t.Errorf("复核状态或金额关系错误：%v", c.Issues)
	}
}

// TestPDFZeroWidthGlyphsNeedFallback 验证大量无宽度字形不被当作可靠中文布局。
// 参数：t 为测试上下文。
// 返回值：无；字形码可读但几何不可靠时未触发后备则失败。
func TestPDFZeroWidthGlyphsNeedFallback(t *testing.T) {
	page := layoutFixture()
	if !pdfPagesHaveUsableText([]layoutPage{page}) {
		t.Fatal("正常文字和几何被错误拒绝")
	}
	for i := range page.Glyphs {
		page.Glyphs[i].Width = 0
	}
	if pdfPagesHaveUsableText([]layoutPage{page}) {
		t.Fatal("全部字形宽度缺失仍被视为可靠布局")
	}
}

// TestPDFIssuerConflictRemainsUnresolved 验证开票人冲突保留双来源且不能自动选值。
// 参数：t 为测试上下文。
// 返回值：无；冲突被覆盖或遗漏问题码时失败。
func TestPDFIssuerConflictRemainsUnresolved(t *testing.T) {
	page := layoutFixture()
	for i, name := range []string{"开票人：甲", "开票人：乙"} {
		page.Glyphs = append(page.Glyphs, Source{Page: 1, X: 30,
			Y: 450 - float64(i)*20, Width: 100, Height: 10, Text: name})
	}
	result, err := adaptPDFPages(context.Background(), []layoutPage{page})
	if err != nil {
		t.Fatal(err)
	}
	c := result.Documents[0]
	if len(c.FieldCandidates["issuer"]) != 2 ||
		!slices.Contains(c.Issues, "FIELD_CONFLICT:issuer") {
		t.Fatal("开票人冲突未保留")
	}
}

// TestPDFIncompleteRowsAreNotSilentlyMerged 验证无续写证据或跨段的缺金额行保持独立。
// 参数：t 为测试上下文。
// 返回值：无；独立项目被拼接到上一行并掩盖缺金额时失败。
func TestPDFIncompleteRowsAreNotSilentlyMerged(t *testing.T) {
	for _, tc := range []struct {
		name, original, next string
		y                    float64
	}{
		{"独立名称", "*技术服务*测评", "需单独核对)", 550},
		{"跨段续行", "*技术服务*测评(", "需单独核对)", 530},
		{"新项目标记", "*技术服务*测评(", "*技术服务*新项目", 550},
		{"未闭合独立名称", "*技术服务*测评(", "设备租赁", 550},
		{"自带成对括号独立名称", "*技术服务*测评(", "设备租赁(一天)", 550},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := layoutFixture()
			for i := range page.Glyphs {
				if page.Glyphs[i].Text == "*技术服务*测评" {
					page.Glyphs[i].Text = tc.original
				}
			}
			page.Glyphs = append(page.Glyphs, Source{Page: 1, X: 30,
				Y: tc.y, Width: 50, Height: 10, Text: tc.next})
			result, err := adaptPDFPages(context.Background(), []layoutPage{page})
			if err != nil {
				t.Fatal(err)
			}
			c := result.Documents[0]
			if len(c.DetailLines) != 2 || !slices.Contains(c.Issues, "DETAIL_AMOUNTS_INCOMPLETE") {
				t.Fatalf("不完整独立行被隐藏：%v", c.Issues)
			}
		})
	}
}

// TestOFDIssuerKeepsXMLSource 验证结构化票据的开票人同样进入统一候选并保留来源。
// 参数：t 为测试上下文。
// 返回值：无；开票人缺失或来源无法追溯时失败。
func TestOFDIssuerKeepsXMLSource(t *testing.T) {
	xml := structuredInvoiceXML()
	xml = strings.Replace(xml, "</Invoice>", "<Drawer>测试开票人</Drawer></Invoice>", 1)
	result, err := New(Limits{}).Parse(context.Background(),
		structuredOFDFixture(t, xml, false), "application/ofd")
	if err != nil {
		t.Fatal(err)
	}
	values := result.Documents[0].FieldCandidates["issuer"]
	if len(values) != 1 || values[0].Value != "测试开票人" ||
		!strings.Contains(values[0].Sources[0].XMLPath, "/Drawer[1]") {
		t.Fatal("开票人及XML来源未保留")
	}
}

// TestPDFNameStartingWithTotalIsNotSummary 验证含“合计”前缀的业务名称不会被丢成合计行。
// 参数：t 为测试上下文。
// 返回值：无；正常项目行被汇总规则截掉时失败。
func TestPDFNameStartingWithTotalIsNotSummary(t *testing.T) {
	page := layoutFixture()
	for i := range page.Glyphs {
		if page.Glyphs[i].Text == "*技术服务*测评" {
			page.Glyphs[i].Text = "合计咨询服务"
		}
	}
	result, err := adaptPDFPages(context.Background(), []layoutPage{page})
	if err != nil || len(result.Documents[0].DetailLines) != 1 {
		t.Fatalf("正常项目被汇总规则吞掉：%v", err)
	}
}

// TestOFDLibraryIssuerKeepsAttachmentSource 验证库自定义标引中的开票人进入统一候选。
// 参数：t 为测试上下文。
// 返回值：无；开票人未归一或来源丢失时失败。
func TestOFDLibraryIssuerKeepsAttachmentSource(t *testing.T) {
	c := newCandidate()
	addOFDLibraryFields(&c, &ofd.Invoice{Drawer: " 测试开票人 ", Attachment: "Doc/Tags.xml"})
	resolveFields(&c)
	if c.Issuer != "测试开票人" ||
		c.FieldCandidates["issuer"][0].Sources[0].XMLPath != "Doc/Tags.xml" {
		t.Fatal("库开票人及来源未保留")
	}
}
