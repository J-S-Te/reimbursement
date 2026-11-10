package main

import (
	"github.com/j-s-te/reimbursement/internal/bootstrap"
	"log/slog"
	"os"
)

// main 启动报销API，失败仅输出稳定错误摘要。
// 参数：无。
// 返回值：无；启动失败以非零退出。
func main() {
	if err := bootstrap.RunAPI(); err != nil {
		slog.Error("reimbursement API stopped", "reason", err.Error())
		os.Exit(1)
	}
}
