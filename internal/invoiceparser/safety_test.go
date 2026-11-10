package invoiceparser

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// ofdTestContainer 构造纯合成容器以验证预检，不代表真实发票样本。
// 参数：names和contents是同序条目，symlink控制首个条目是否为符号链接。
// 返回值：ZIP字节；测试构造失败立即终止。
func ofdTestContainer(t *testing.T, names, contents []string, symlink bool) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for i, name := range names {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if i == 0 && symlink {
			h.SetMode(os.ModeSymlink | 0777)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "/") {
			continue
		}
		if _, err = f.Write([]byte(contents[i])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestParserSecurityAndHardLimits 验证符号链接、同名目录、XML深度和配置不能扩大硬边界。
// 参数：t为测试上下文。
// 返回值：无；危险容器或超额配置被接受时失败。
func TestParserSecurityAndHardLimits(t *testing.T) {
	p := New(Limits{})
	cases := [][]string{{"OFD.xml", "same", "same/"}, {"OFD.xml", "Doc_0/../escape.xml"}, {"OFD.xml", "OFD.xml"}}
	for _, names := range cases {
		contents := make([]string, len(names))
		for i := range contents {
			contents[i] = "<OFD/>"
		}
		if err := p.checkOFD(context.Background(), ofdTestContainer(t, names, contents, false)); err == nil {
			t.Fatal("unsafe path accepted", names)
		}
	}
	if err := p.checkOFD(context.Background(), ofdTestContainer(t, []string{"OFD.xml"}, []string{"target"}, true)); err == nil {
		t.Fatal("symlink accepted")
	}
	deep := strings.Repeat("<x>", 70) + strings.Repeat("</x>", 70)
	if err := p.checkOFD(context.Background(), ofdTestContainer(t, []string{"OFD.xml"}, []string{deep}, false)); err == nil {
		t.Fatal("deep XML accepted")
	}
	oversized := New(Limits{MaxBytes: 21 << 20, MaxExpandedBytes: 101 << 20, MaxPages: 101})
	if _, err := oversized.Parse(context.Background(), []byte("%PDF-1.7"), "application/pdf"); err == nil || err.Error() != "PARSER_LIMIT_CONFIGURATION_INVALID" {
		t.Fatal("hard cap not rejected", err)
	}
}

// TestMoneyNormalizationAndInvalidGrouping 验证全角归一不改变金额精度且拒绝错误千分位。
// 参数：t为测试上下文。
// 返回值：无；金额被静默改写时失败。
func TestMoneyNormalizationAndInvalidGrouping(t *testing.T) {
	if value, err := Minor("￥１，２３４．５６"); err != nil || value != 123456 {
		t.Fatal(value, err)
	}
	for _, raw := range []string{"12,34.56", "1.001", "1,234,56", "1e3"} {
		if _, err := Minor(raw); err == nil {
			t.Fatal("invalid money accepted", raw)
		}
	}
}
