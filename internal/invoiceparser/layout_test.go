package invoiceparser

import (
	"context"
	"slices"
	"testing"
)

// layoutFixture 构造合成字形坐标，仅证明布局规则而不代表真实票样覆盖。
// 参数：无。
// 返回值：购买方与销售方并列、含金额和单行明细的测试页。
func layoutFixture() layoutPage {
	page := layoutPage{Number: 1, Width: 600, Height: 800}
	for _, r := range []struct {
		x, y float64
		s    string
	}{
		{30, 770, "电子发票（普通发票）"}, {330, 740, "发票号码：12345678901234567890"}, {330, 720, "开票日期：2026年11月10日"},
		{30, 680, "购买方"}, {330, 680, "销售方"}, {30, 660, "名称：甲方公司"}, {330, 660, "名称：乙方公司"},
		{30, 640, "纳税人识别号：91310100AAAAAA0001"}, {330, 640, "纳税人识别号：91310100AAAAAA0002"},
		{30, 590, "项目名称"}, {220, 590, "数量"}, {300, 590, "单价"}, {390, 590, "金额"}, {460, 590, "税率"}, {530, 590, "税额"},
		{30, 570, "*技术服务*测评"}, {220, 570, "1"}, {300, 570, "100.00"}, {390, 570, "100.00"}, {460, 570, "13%"}, {530, 570, "13.00"},
		{30, 520, "不含税金额：100.00"}, {300, 520, "税额合计：13.00"}, {30, 500, "价税合计（小写）：￥113.00"},
	} {
		page.Glyphs = append(page.Glyphs, Source{Page: 1, X: r.x, Y: r.y, Width: float64(len([]rune(r.s))) * 5, Height: 10, Text: r.s})
	}
	return page
}

// TestPDFRegionAndMoneyAdapter 验证先分购销区域、精确金额守恒和明细来源。
// 参数：t为测试上下文。
// 返回值：无；跨区域取值、漏掉明细或自动确认时失败。
func TestPDFRegionAndMoneyAdapter(t *testing.T) {
	result, err := adaptPDFPages(context.Background(), []layoutPage{layoutFixture()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Documents) != 1 {
		t.Fatal(result)
	}
	c := result.Documents[0]
	if c.Number != "12345678901234567890" || c.Date != "2026-11-10" || c.BuyerName != "甲方公司" || c.SellerName != "乙方公司" || c.BuyerTaxID != "91310100AAAAAA0001" || c.SellerTaxID != "91310100AAAAAA0002" {
		t.Fatalf("field mismatch: %+v", c)
	}
	if c.AmountMinor != "10000" || c.TaxMinor != "1300" || c.GrossMinor != "11300" || len(c.DetailLines) != 1 {
		t.Fatalf("amount/details: %+v", c)
	}
	if c.ReviewStatus != "PENDING" || c.VerificationStatus != "NOT_CHECKED" || len(c.FieldCandidates["buyer_tax_id"]) != 1 || c.FieldCandidates["buyer_tax_id"][0].Sources[0].Page != 1 {
		t.Fatal("missing review/source invariants")
	}
}

// TestPDFConflictsAndMergeDoNotOverwrite 验证不同号码与金额全部保留并明确人工拆分。
// 参数：t为测试上下文。
// 返回值：无；冲突被最后覆盖或按页当票时失败。
func TestPDFConflictsAndMergeDoNotOverwrite(t *testing.T) {
	first := layoutFixture()
	second := layoutFixture()
	second.Number = 2
	for i := range second.Glyphs {
		second.Glyphs[i].Page = 2
		if second.Glyphs[i].Text == "发票号码：12345678901234567890" {
			second.Glyphs[i].Text = "发票号码：12345678901234567891"
		}
	}
	result, err := adaptPDFPages(context.Background(), []layoutPage{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "NEEDS_REVIEW" || len(result.Documents) != 1 || result.Documents[0].GroupKind != "AMBIGUOUS_MERGE_OR_CONTINUATION" || result.Documents[0].Number != "" || len(result.Documents[0].FieldCandidates["invoice_number"]) != 2 {
		t.Fatalf("merge=%+v", result)
	}
	first.Glyphs = append(first.Glyphs, Source{Page: 1, X: 30, Y: 480, Width: 200, Height: 10, Text: "价税合计(小写):114.00"})
	result, err = adaptPDFPages(context.Background(), []layoutPage{first})
	if err != nil {
		t.Fatal(err)
	}
	if result.Documents[0].GrossMinor != "" || len(result.Documents[0].FieldCandidates["gross_minor"]) != 2 {
		t.Fatal("conflicting total overwritten")
	}
}

// TestPDFUnscopedPartyAndNoTextFailSafely 验证无区域税号不被猜作购销方且无文字层明确失败。
// 参数：t为测试上下文。
// 返回值：无；无证据字段被猜测或空白被当已识别时失败。
func TestPDFUnscopedPartyAndNoTextFailSafely(t *testing.T) {
	p := layoutPage{Number: 1, Width: 600, Height: 800, Glyphs: []Source{{Page: 1, X: 30, Y: 700, Text: "纳税人识别号:91310100AAAAAA0001", Height: 10}, {Page: 1, X: 30, Y: 650, Text: "纳税人识别号:91310100AAAAAA0002", Height: 10}}}
	result, err := adaptPDFPages(context.Background(), []layoutPage{p})
	if err != nil {
		t.Fatal(err)
	}
	if result.Documents[0].BuyerTaxID != "" || result.Documents[0].SellerTaxID != "" {
		t.Fatal("party guessed without region")
	}
	if _, err = adaptPDFPages(context.Background(), []layoutPage{{Number: 1, Width: 600, Height: 800}}); err == nil {
		t.Fatal("blank accepted")
	}
}

// TestPDFRejectsPartialNumberAndMoneyTokens 验证超长票号和科学计数金额不被截断成有效字段。
// 参数：t为测试上下文。
// 返回值：无；格式非法值被部分识别时失败。
func TestPDFRejectsPartialNumberAndMoneyTokens(t *testing.T) {
	page := layoutFixture()
	for i := range page.Glyphs {
		switch page.Glyphs[i].Text {
		case "发票号码：12345678901234567890":
			page.Glyphs[i].Text = "发票号码：123456789012345678901"
		case "价税合计（小写）：￥113.00":
			page.Glyphs[i].Text = "价税合计（小写）：1e3"
		}
	}
	result, err := adaptPDFPages(context.Background(), []layoutPage{page})
	if err != nil {
		t.Fatal(err)
	}
	c := result.Documents[0]
	if c.Number != "" || len(c.FieldCandidates["gross_minor"]) != 0 {
		t.Fatalf("partial token promoted %+v", c)
	}
}

// TestPDFVerticalPartyAnchors 验证传统票面竖排购销方标记先形成区域再识别税号。
// 参数：t为测试上下文。
// 返回值：无；竖排锚点失效或税号串区时失败。
func TestPDFVerticalPartyAnchors(t *testing.T) {
	page := layoutPage{Number: 1, Width: 600, Height: 800}
	for _, item := range []struct {
		x, y float64
		text string
	}{{10, 700, "购"}, {10, 680, "买"}, {10, 660, "方"}, {30, 700, "名称：甲方公司"}, {30, 660, "纳税人识别号：91310100AAAAAA0001"}, {10, 580, "销"}, {10, 560, "售"}, {10, 540, "方"}, {30, 580, "名称：乙方公司"}, {30, 540, "纳税人识别号：91310100AAAAAA0002"}} {
		page.Glyphs = append(page.Glyphs, Source{Page: 1, X: item.x, Y: item.y, Width: 10, Height: 10, Text: item.text})
	}
	result, err := adaptPDFPages(context.Background(), []layoutPage{page})
	if err != nil {
		t.Fatal(err)
	}
	c := result.Documents[0]
	if c.BuyerTaxID != "91310100AAAAAA0001" || c.SellerTaxID != "91310100AAAAAA0002" {
		t.Fatalf("vertical region buyer=%q seller=%q issues=%v", c.BuyerTaxID, c.SellerTaxID, c.Issues)
	}
}

// TestPDFMixedRatesAndContinuationRemainReviewable 验证多税率行独立保留，续页不凭页数创建发票。
// 参数：t为测试上下文。
// 返回值：无；固定税率假设、漏掉已知明细或续页误分组时失败。
func TestPDFMixedRatesAndContinuationRemainReviewable(t *testing.T) {
	page := layoutFixture()
	for i := range page.Glyphs {
		switch page.Glyphs[i].Text {
		case "不含税金额：100.00":
			page.Glyphs[i].Text = "不含税金额：300.00"
		case "税额合计：13.00":
			page.Glyphs[i].Text = "税额合计：25.00"
		case "价税合计（小写）：￥113.00":
			page.Glyphs[i].Text = "价税合计（小写）：￥325.00"
		}
	}
	for _, cell := range []struct {
		x    float64
		text string
	}{{30, "*技术服务*第二项"}, {220, "1"}, {300, "200.00"}, {390, "200.00"}, {460, "6%"}, {530, "12.00"}} {
		page.Glyphs = append(page.Glyphs, Source{Page: 1, X: cell.x, Y: 550, Width: float64(len([]rune(cell.text))) * 5, Height: 10, Text: cell.text})
	}
	result, err := adaptPDFPages(context.Background(), []layoutPage{page})
	if err != nil {
		t.Fatal(err)
	}
	c := result.Documents[0]
	if len(c.DetailLines) != 2 || c.DetailLines[1].TaxRate != "6%" || slices.Contains(c.Issues, "AMOUNT_CONFLICT") || slices.Contains(c.Issues, "DETAIL_TAX_TOTAL_CONFLICT") {
		t.Fatal(c.Issues, len(c.DetailLines))
	}
	continuation := layoutPage{Number: 2, Width: 600, Height: 800, Glyphs: []Source{{Page: 2, X: 30, Y: 700, Width: 120, Height: 10, Text: "这是未标明原票号码的续页文字"}}}
	result, err = adaptPDFPages(context.Background(), []layoutPage{page, continuation})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Documents) != 1 || result.Documents[0].GroupKind != "AMBIGUOUS_MERGE_OR_CONTINUATION" {
		t.Fatal("continuation assumed invoice", result.Documents[0].GroupKind)
	}
}
