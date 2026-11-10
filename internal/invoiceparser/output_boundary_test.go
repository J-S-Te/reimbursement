package invoiceparser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDensePDFOutputRetainsReviewEvidence 用途：验证合法高密度文字不会仅因重复证据越过进程输出限制。
// 参数：t 为合成PDF真实读取与子进程测试上下文，不代表真实票样验收。
// 返回值：无；结果超限、压缩未披露或页级来源丢失时失败。
func TestDensePDFOutputRetainsReviewEvidence(t *testing.T) {
	raw := minimalPDF("BT /F1 12 Tf 30 700 Td ("+strings.Repeat("A", 50000)+") Tj ET", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := Isolated(ctx, raw, "application/pdf")
	if err != nil {
		t.Fatal("bounded legal PDF must remain reviewable", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) >= 4<<20 {
		t.Fatal("result exceeds output budget", len(encoded), err)
	}
	c := result.Documents[0]
	if !slices.Contains(result.Warnings, "EVIDENCE_COMPACTED_REFER_TO_ORIGINAL") ||
		len(c.Sources) == 0 || c.Sources[0].Page != 1 || c.ReviewStatus != "PENDING" {
		t.Fatal("compaction disclosure or source/review state missing")
	}
	var metadata struct {
		OriginalSourceCount int `json:"original_source_count"`
		OriginalRawBytes    int `json:"original_raw_bytes"`
	}
	if err := json.Unmarshal(c.Raw, &metadata); err != nil ||
		metadata.OriginalSourceCount != 50000 || metadata.OriginalRawBytes == 0 {
		t.Fatal("missing original evidence counts", metadata, err)
	}
}

// TestCompactionKeepsEveryConflictingFieldAndDetail 用途：验证缩小证据不会消除冲突、明细或其来源。
// 参数：t 为结果边界回归上下文，输入使用完整真实布局适配器结果。
// 返回值：无；来源压缩改变任何候选字段或明细及人工状态时失败。
func TestCompactionKeepsEveryConflictingFieldAndDetail(t *testing.T) {
	page := layoutFixture()
	page.Glyphs = append(page.Glyphs, Source{Page: 1, X: 30, Y: 480,
		Width: 200, Height: 10, Text: "价税合计(小写):114.00"})
	result, err := adaptPDFPages(context.Background(), []layoutPage{page})
	if err != nil {
		t.Fatal(err)
	}
	c := result.Documents[0]
	fields, _ := json.Marshal(c.FieldCandidates)
	details, _ := json.Marshal(c.DetailLines)
	c.Raw, _ = json.Marshal(strings.Repeat("x", 4<<20))
	result.Documents = []Candidate{c}
	bounded, err := boundResultEvidence(result)
	if err != nil {
		t.Fatal(err)
	}
	got := bounded.Documents[0]
	gotFields, _ := json.Marshal(got.FieldCandidates)
	gotDetails, _ := json.Marshal(got.DetailLines)
	if !bytes.Equal(fields, gotFields) || !bytes.Equal(details, gotDetails) ||
		got.GrossMinor != "" || len(got.FieldCandidates["gross_minor"]) != 2 ||
		!slices.Contains(got.Issues, "FIELD_CONFLICT:gross_minor") ||
		!reflect.DeepEqual(c.Pages, got.Pages) || got.ReviewStatus != "PENDING" {
		t.Fatal("compaction changed facts/conflicts or review state")
	}
}

// TestIrreducibleOFDOutputFailsWithoutDroppingCandidates 用途：验证不可约冲突候选超限时明确拒绝而非截断成成功。
// 参数：t 为有界合成结构化OFD测试上下文，不使用业务原件。
// 返回值：无；超限静默丢候选、输出超过限制或失败码丢失时失败。
func TestIrreducibleOFDOutputFailsWithoutDroppingCandidates(t *testing.T) {
	var names strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&names, "<BuyerName>%04d%s</BuyerName>", i, strings.Repeat("A", 1500))
	}
	xml := strings.Replace(structuredInvoiceXML(), "</Invoice>", names.String()+"</Invoice>", 1)
	data := structuredOFDFixture(t, xml, false)
	var output bytes.Buffer
	handled, err := RunCommand([]string{"--parse-document", "application/ofd"},
		bytes.NewReader(data), &output)
	if !handled || err != nil || output.Len() >= 4<<20 {
		t.Fatal("command envelope exceeds bound", handled, err, output.Len())
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil ||
		envelope.Code != "PARSER_OUTPUT_LIMIT" {
		t.Fatal("must explicitly fail instead of truncating candidates", envelope.Code, err)
	}
}
