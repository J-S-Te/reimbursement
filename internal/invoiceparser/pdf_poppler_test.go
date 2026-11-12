package invoiceparser

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestPopplerLayoutParsesPageWords 验证 bbox-layout XHTML 到词框坐标的转换。
func TestPopplerLayoutParsesPageWords(t *testing.T) {
	raw := []byte(`<html><body><doc><page width="600" height="800"><flow><block><line>
<word xMin="10" yMin="20" xMax="110" yMax="40">价税合计（小写）：113.00</word>
</line></block></flow></page></doc></body></html>`)
	pages, err := parsePopplerLayout(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || len(pages[0].Glyphs) != 1 {
		t.Fatalf("pages=%+v", pages)
	}
	glyph := pages[0].Glyphs[0]
	if glyph.Text != "价税合计（小写）：113.00" || glyph.X != 10 || glyph.Y != 760 || glyph.Width != 100 || glyph.Height != 20 || glyph.Method != pdfPopplerMethod {
		t.Fatalf("glyph=%+v", glyph)
	}
}

// TestPDFCIDFontFallsBackToPoppler 验证锁定库无法还原中文 CID 字体时，
// 使用 pdftotext 词框仍能解析票号、日期、双方和金额。
// 该测试依赖本机 poppler-utils；未安装时明确跳过，生产镜像已显式安装。
func TestPDFCIDFontFallsBackToPoppler(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
	data, err := os.ReadFile("testdata/chinese-invoice.pdf")
	if err != nil {
		t.Fatal(err)
	}
	result, err := New(Limits{}).Parse(context.Background(), data, "application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.ParserVersion, "+poppler") || len(result.Documents) != 1 {
		t.Fatalf("parser version/result=%+v", result)
	}
	candidate := result.Documents[0]
	if candidate.Number != "87654321" || candidate.Date != "2026-11-12" ||
		candidate.GrossMinor != "11300" || candidate.AmountMinor != "10000" || candidate.TaxMinor != "1300" ||
		candidate.BuyerName != "甲方有限公司" || candidate.SellerName != "乙方有限公司" ||
		candidate.BuyerTaxID != "91310100BBBBBB0001" || candidate.SellerTaxID != "91310100BBBBBB0002" {
		t.Fatalf("candidate=%+v", candidate)
	}
}
