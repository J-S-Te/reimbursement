package invoiceparser

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

// TestIsolatedPreflightAndCancellation 验证子进程边界不忽略收紧限制或已有取消。
// 参数：t为测试上下文。
// 返回值：无；错误限制被接受或取消未生效时失败。
func TestIsolatedPreflightAndCancellation(t *testing.T) {
	t.Setenv("REIMBURSEMENT_PARSER_MAX_BYTES", "32")
	if _, err := Isolated(context.Background(), make([]byte, 33), "application/pdf"); err == nil || err.Error() != "DOCUMENT_SIZE_LIMIT" {
		t.Fatal("configured input limit", err)
	}
	t.Setenv("REIMBURSEMENT_PARSER_MAX_BYTES", "20971520")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Isolated(ctx, []byte("%PDF-1.7"), "application/pdf"); err == nil || err.Error() != "PARSER_TIMEOUT" {
		t.Fatal("cancellation", err)
	}
}

// TestRunCommandBoundedFailureEnvelope 验证内部入口保持稳定错误封装和输出上限。
// 参数：t为测试上下文。
// 返回值：无；命令误识别、格式错误丢失或输出超限未拒绝时失败。
func TestRunCommandBoundedFailureEnvelope(t *testing.T) {
	previous := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(previous) })
	var output bytes.Buffer
	handled, err := RunCommand([]string{"--parse-document", "application/pdf"}, strings.NewReader("not a pdf"), &output)
	if !handled || err != nil {
		t.Fatal(handled, err)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || envelope.Code != "UNSUPPORTED_OR_MISMATCHED_FORMAT" {
		t.Fatal(output.String(), err)
	}
	bounded := &boundedOutput{maximum: 2}
	if _, err := bounded.Write([]byte("long")); err == nil {
		t.Fatal("output cap ignored")
	}
	if handled, err := RunCommand([]string{"worker"}, strings.NewReader(""), &output); handled || err != nil {
		t.Fatal("unrelated command intercepted")
	}
}
