package invoiceparser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain 用途：让测试可执行文件承接真实解析子进程协议和环境探针。
// 参数：m 为本包测试入口；探针仅在测试二进制的专有媒体类型下启用。
// 返回值：无；解析协议和测试结果通过进程退出码返回。
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--parse-document" {
		switch os.Args[2] {
		case "application/x-test-parser-environment":
			emitParserEnvironmentProbe()
			os.Exit(0)
		case "application/x-test-parser-overflow":
			_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 5<<20))
			os.Exit(0)
		default:
			handled, err := RunCommand(os.Args[1:], os.Stdin, os.Stdout)
			if handled && err == nil {
				os.Exit(0)
			}
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// emitParserEnvironmentProbe 用途：报告子进程是否获得非解析用途环境及实际资源限制。
// 参数：无；仅在测试子进程中读取环境，不输出任何变量值或凭据。
// 返回值：无；通过原协议返回非白名单键名及已解析的资源限制。
func emitParserEnvironmentProbe() {
	allowed := map[string]bool{
		"GOMEMLIMIT": true, "GOMAXPROCS": true,
		"REIMBURSEMENT_PARSER_MAX_BYTES":          true,
		"REIMBURSEMENT_PARSER_MAX_EXPANDED_BYTES": true,
		"REIMBURSEMENT_PARSER_MAX_PAGES":          true,
	}
	unexpected := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !allowed[key] {
			unexpected = append(unexpected, key)
		}
	}
	p := New(Limits{})
	result := Result{Status: fmt.Sprintf("%d/%d/%d", p.limits.MaxBytes,
		p.limits.MaxExpandedBytes, p.limits.MaxPages), Warnings: unexpected}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"result": result, "code": ""})
}

// TestIsolatedChildReceivesOnlyParserEnvironment 用途：验证真实子进程不继承父进程凭据。
// 参数：t 为本地进程隔离测试上下文，全部注入值均为测试哨兵。
// 返回值：无；凭据泄入子进程或收紧限制未透传时失败。
func TestIsolatedChildReceivesOnlyParserEnvironment(t *testing.T) {
	for _, key := range []string{"MYSQL_DSN", "PLATFORM_FILE_SECRET", "OIDC_CLIENT_SECRET",
		"COMMERCIAL_LICENSE_CLIENT_SECRET", "HTTP_PROXY", "LD_PRELOAD"} {
		t.Setenv(key, "parser-test-sentinel")
	}
	t.Setenv("REIMBURSEMENT_PARSER_MAX_BYTES", "32")
	t.Setenv("REIMBURSEMENT_PARSER_MAX_EXPANDED_BYTES", "128")
	t.Setenv("REIMBURSEMENT_PARSER_MAX_PAGES", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := Isolated(ctx, []byte("probe"), "application/x-test-parser-environment")
	if err != nil {
		t.Fatal("environment probe failed", err)
	}
	if len(result.Warnings) != 0 {
		t.Fatal("unexpected inherited environment keys", result.Warnings)
	}
	if result.Status != "32/128/1" {
		t.Fatal("tightened parser limits did not reach child", result.Status)
	}
}

// TestIsolatedOverflowStopsChildWithStableCode 用途：验证输出超限立即结束子进程并保留限额诊断。
// 参数：t 为真实管道输出测试上下文，不执行第三方文档解析。
// 返回值：无；超限被误报为超时或通用进程失败时失败。
func TestIsolatedOverflowStopsChildWithStableCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Isolated(ctx, []byte("probe"), "application/x-test-parser-overflow")
	if err == nil || err.Error() != "PARSER_OUTPUT_LIMIT" {
		t.Fatalf("output overflow should stop with PARSER_OUTPUT_LIMIT: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("output overflow only stopped at caller deadline")
	}
}

// TestIsolatedRestrictedEnvironmentParsesOFD 用途：验证最小环境仍可经真实子进程解析结构化原件。
// 参数：t 为合成OFD进程回归上下文，不替代授权票样验收。
// 返回值：无；关键金额、来源或人工复核状态丢失时失败。
func TestIsolatedRestrictedEnvironmentParsesOFD(t *testing.T) {
	for _, key := range []string{"MAX_BYTES", "MAX_EXPANDED_BYTES", "MAX_PAGES"} {
		t.Setenv("REIMBURSEMENT_PARSER_"+key, "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data := structuredOFDFixture(t, structuredInvoiceXML(), false)
	result, err := Isolated(ctx, data, "application/ofd")
	if err != nil {
		t.Fatal("real OFD subprocess failed", err)
	}
	if len(result.Documents) != 1 {
		t.Fatal("expected one structured candidate")
	}
	c := result.Documents[0]
	if c.GrossMinor != "11300" || c.BuyerTaxID != "91310100AAAAAA0001" ||
		c.ReviewStatus != "PENDING" || c.VerificationStatus != "NOT_CHECKED" ||
		len(c.FieldCandidates["gross_minor"]) != 1 ||
		len(c.FieldCandidates["gross_minor"][0].Sources) == 0 {
		t.Fatal("subprocess changed invoice facts or source/manual-review state")
	}
}
