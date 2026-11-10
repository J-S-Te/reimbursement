package invoiceparser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
)

type boundedOutput struct {
	buffer   bytes.Buffer
	maximum  int
	exceeded bool
	onLimit  func()
}

// Write 限制解析子进程输出，避免畸形票面文字耗尽主worker内存。
// 参数：data为子进程生成结果字节。
// 返回值：实际写入字节数；超限立即失败。
func (b *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > b.maximum-b.buffer.Len() {
		b.exceeded = true
		if b.onLimit != nil {
			b.onLimit()
		}
		return 0, errors.New("PARSER_OUTPUT_LIMIT")
	}
	return b.buffer.Write(data)
}

// Isolated 在可被取消杀死的独立worker子进程执行第三方解析库。
// 参数：ctx为任务硬超时；data为已验证原件；media为允许格式。
// 返回值：有界结果；超时、崩溃或输出超限不会拖死主worker。
func Isolated(ctx context.Context, data []byte, media string) (Result, error) {
	parser := New(Limits{})
	if parser.configurationError != nil {
		return Result{}, parser.configurationError
	}
	if len(data) == 0 || len(data) > parser.limits.MaxBytes {
		return Result{}, errors.New("DOCUMENT_SIZE_LIMIT")
	}
	executable, err := os.Executable()
	if err != nil {
		return Result{}, err
	}
	childContext, stopChild := context.WithCancel(ctx)
	defer stopChild()
	command := exec.CommandContext(childContext, executable, "--parse-document", media)
	command.Stdin = bytes.NewReader(data)
	output := &boundedOutput{maximum: hardMaxOutputBytes, onLimit: stopChild}
	command.Stdout = output
	command.Stderr = io.Discard
	command.Env = parserEnvironment(parser.limits)
	if err = command.Run(); err != nil {
		if ctx.Err() != nil {
			return Result{}, errors.New("PARSER_TIMEOUT")
		}
		if output.exceeded {
			return Result{}, errors.New("PARSER_OUTPUT_LIMIT")
		}
		return Result{}, errors.New("PARSER_PROCESS_FAILED")
	}
	var envelope struct {
		Result Result `json:"result"`
		Code   string `json:"code"`
	}
	if json.Unmarshal(output.buffer.Bytes(), &envelope) != nil {
		return Result{}, errors.New("PARSER_RESULT_INVALID")
	}
	if envelope.Code != "" {
		return Result{}, errors.New(envelope.Code)
	}
	return envelope.Result, nil
}

// parserEnvironment 用途：仅向文档解析子进程传递已校验资源限制。
// 参数：limits 为当前父进程解析器校验后的有效限制。
// 返回值：显式最小环境，不继承业务凭据、代理或动态加载器配置。
func parserEnvironment(limits Limits) []string {
	return []string{
		"GOMEMLIMIT=128MiB", "GOMAXPROCS=1",
		"REIMBURSEMENT_PARSER_MAX_BYTES=" + strconv.Itoa(limits.MaxBytes),
		"REIMBURSEMENT_PARSER_MAX_EXPANDED_BYTES=" + strconv.FormatUint(limits.MaxExpandedBytes, 10),
		"REIMBURSEMENT_PARSER_MAX_PAGES=" + strconv.Itoa(limits.MaxPages),
	}
}

// RunCommand 提供worker内部解析子进程入口，不加载数据库或运行凭据。
// 参数：args为worker命令参数；input/output为有界原件及结果通道。
// 返回值：是否处理解析命令以及输入输出错误。
func RunCommand(args []string, input io.Reader, output io.Writer) (bool, error) {
	if len(args) == 0 || args[0] != "--parse-document" {
		return false, nil
	}
	if len(args) != 2 {
		return true, errors.New("INVALID_PARSER_COMMAND")
	}
	debug.SetMemoryLimit(128 << 20)
	parser := New(Limits{})
	if parser.configurationError != nil {
		return true, json.NewEncoder(output).Encode(map[string]any{
			"result": Result{}, "code": parser.configurationError.Error(),
		})
	}
	raw, err := io.ReadAll(io.LimitReader(input, int64(parser.limits.MaxBytes)+1))
	if err != nil {
		return true, err
	}
	result, parseErr := parser.Parse(context.Background(), raw, args[1])
	code := ""
	if parseErr != nil {
		code = parseErr.Error()
		if len(code) > 100 || strings.ContainsAny(code, "\n\r") {
			code = "PARSER_FAILED"
		}
	}
	return true, json.NewEncoder(output).Encode(map[string]any{"result": result, "code": code})
}
