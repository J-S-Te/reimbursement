package invoiceparser

import (
	"context"
	"testing"
)

// TestImageInvoiceOriginalIsNotParsed 锁定拍照发票边界：
// 图片原件允许上传并由人工确认票面，但不得进入票面解析，避免产生"上传成功却永远解析失败"的假象。
// 参数：t为测试上下文。
// 返回值：无。
func TestImageInvoiceOriginalIsNotParsed(t *testing.T) {
	p := New(Limits{})
	for _, media := range []string{"image/png", "image/jpeg"} {
		_, err := p.Parse(context.Background(), []byte("\x89PNG\r\n\x1a\n"), media)
		if err == nil || err.Error() != "UNSUPPORTED_OR_MISMATCHED_FORMAT" {
			t.Fatalf("%s 应被解析器以 UNSUPPORTED_OR_MISMATCHED_FORMAT 拒绝，实际 err=%v", media, err)
		}
	}
}
