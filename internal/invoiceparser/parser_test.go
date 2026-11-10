package invoiceparser

import (
	"archive/zip"
	"bytes"
	"context"
	"testing"
)

// TestMoneyExactMinor 防止票面金额经浮点计算误差或多余精度进入账本。
// 参数：t 为测试上下文。
// 返回值：无。
func TestMoneyExactMinor(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  int64
		valid bool
	}{{"1,234.56", 123456, true}, {"0.01", 1, true}, {"0.005", 0, false}, {"-1.23", -123, true}, {"92233720368547758.08", 0, false}} {
		got, err := Minor(tc.raw)
		if (err == nil) != tc.valid || tc.valid && got != tc.want {
			t.Fatalf("%q got %d err %v", tc.raw, got, err)
		}
	}
}

// TestRejectsTraversalAndUnboundedInput 防止OFD路径逃逸与超限输入交给第三方解析库。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRejectsTraversalAndUnboundedInput(t *testing.T) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, _ := z.Create("../invoice.xml")
	f.Write([]byte("<x/>"))
	z.Close()
	p := New(Limits{MaxBytes: 1024, MaxExpandedBytes: 2048, MaxPages: 10})
	if _, err := p.Parse(context.Background(), b.Bytes(), "application/ofd"); err == nil {
		t.Fatal("unsafe zip accepted")
	}
	if _, err := p.Parse(context.Background(), make([]byte, 1025), "application/pdf"); err == nil {
		t.Fatal("oversized input accepted")
	}
}
