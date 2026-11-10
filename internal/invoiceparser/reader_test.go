package invoiceparser

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// minimalPDF 生成仅用于锁定库读取契约的合成PDF，不代表发票真实票样。
// 参数：content为页面绘制流，rotation为页面旋转角度。
// 返回值：带真实xref和MediaBox的单页PDF字节。
func minimalPDF(content string, rotation int) []byte {
	objects := []string{`<< /Type /Catalog /Pages 2 0 R >>`, `<< /Type /Pages /Kids [3 0 R] /Count 1 /MediaBox [0 0 612 792] >>`, fmt.Sprintf(`<< /Type /Page /Parent 2 0 R /Rotate %d /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>`, rotation), `<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>`, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, object := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	start := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), start)
	return b.Bytes()
}

// TestPDFReaderGeometryAndEmptyText 验证真实PDF读取保留继承尺寸旋转，并拒绝空白和损坏文件。
// 参数：t为测试上下文。
// 返回值：无；库读取契约或安全状态不符合预期时失败。
func TestPDFReaderGeometryAndEmptyText(t *testing.T) {
	p := New(Limits{})
	raw := minimalPDF("BT /F1 12 Tf 30 700 Td (Synthetic invoice text only) Tj ET", 90)
	result, err := p.Parse(context.Background(), raw, "application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	c := result.Documents[0]
	if len(c.Pages) != 1 || c.Pages[0].Width != 612 || c.Pages[0].Height != 792 || c.Pages[0].Rotation != 90 || c.ReviewStatus != "PENDING" {
		t.Fatal(c)
	}
	if _, err = p.Parse(context.Background(), minimalPDF("", 0), "application/pdf"); err == nil || err.Error() != "NO_USABLE_TEXT_LAYER" {
		t.Fatal("empty PDF", err)
	}
	if _, err = p.Parse(context.Background(), []byte("%PDF-1.7\nbroken"), "application/pdf"); err == nil {
		t.Fatal("broken PDF accepted")
	}
}

// TestEnvironmentLimitsFailClosed 验证运维配置只能收紧资源边界且格式错误失败关闭。
// 参数：t为测试上下文。
// 返回值：无；环境配置扩大硬上限或被静默忽略时失败。
func TestEnvironmentLimitsFailClosed(t *testing.T) {
	raw := minimalPDF("BT /F1 12 Tf 30 700 Td (Synthetic invoice text only) Tj ET", 0)
	t.Setenv("REIMBURSEMENT_PARSER_MAX_BYTES", "64")
	if _, err := New(Limits{}).Parse(context.Background(), raw, "application/pdf"); err == nil || err.Error() != "DOCUMENT_SIZE_LIMIT" {
		t.Fatal(err)
	}
	t.Setenv("REIMBURSEMENT_PARSER_MAX_BYTES", "0")
	if _, err := New(Limits{}).Parse(context.Background(), raw, "application/pdf"); err == nil || err.Error() != "PARSER_LIMIT_CONFIGURATION_INVALID" {
		t.Fatal(err)
	}
	t.Setenv("REIMBURSEMENT_PARSER_MAX_BYTES", "20971520")
	t.Setenv("REIMBURSEMENT_PARSER_MAX_PAGES", "1")
	if err := New(Limits{}).checkOFD(context.Background(), structuredOFDFixture(t, structuredInvoiceXML(), true)); err == nil || err.Error() != "OFD_PAGE_LIMIT" {
		t.Fatal(err)
	}
	t.Setenv("REIMBURSEMENT_PARSER_MAX_PAGES", "100")
	t.Setenv("REIMBURSEMENT_PARSER_MAX_EXPANDED_BYTES", "32")
	if err := New(Limits{}).checkOFD(context.Background(), structuredOFDFixture(t, structuredInvoiceXML(), false)); err == nil {
		t.Fatal("expanded cap ignored")
	}
}

// TestOFDXMLDirectivesAndMalformedBoundaries 验证DTD、处理指令及非单根XML不能进入第三方库。
// 参数：t为测试上下文。
// 返回值：无；不安全XML被接受时失败。
func TestOFDXMLDirectivesAndMalformedBoundaries(t *testing.T) {
	for _, raw := range []string{`<!DOCTYPE x [<!ENTITY z "unsafe">]><OFD>&z;</OFD>`, `<?external href="http://localhost/"?><OFD/>`, `<OFD/><OFD/>`, strings.Repeat("<x>", 65) + strings.Repeat("</x>", 65)} {
		if err := New(Limits{}).checkOFD(context.Background(), ofdTestContainer(t, []string{"OFD.xml"}, []string{raw}, false)); err == nil {
			t.Fatal("unsafe XML", raw)
		}
	}
}
