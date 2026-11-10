package main

import (
	"github.com/j-s-te/reimbursement/internal/bootstrap"
	"github.com/j-s-te/reimbursement/internal/invoiceparser"
	"log/slog"
	"os"
)

// main 启动独立可靠后台消费者。
// 参数：无。
// 返回值：无；装配或运行失败非零退出。
func main() {
	if handled, err := invoiceparser.RunCommand(os.Args[1:], os.Stdin, os.Stdout); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}

	if err := bootstrap.RunWorker(); err != nil {
		slog.Error("reimbursement worker stopped", "reason", err.Error())
		os.Exit(1)
	}
}
