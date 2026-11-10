package main

import (
	"context"
	"github.com/j-s-te/reimbursement/internal/bootstrap"
	"github.com/j-s-te/reimbursement/migrations"
	"log/slog"
	"os"
	"time"
)

// main 仅在显式确认备份门禁后执行报销专库迁移。
// 参数：MYSQL_DSN指定目标库，REIMBURSEMENT_MIGRATION_BACKUP_VERIFIED为受控操作确认。
// 返回值：无；门禁或迁移失败非零退出。
func main() {
	if os.Getenv("REIMBURSEMENT_MIGRATION_BACKUP_VERIFIED") != "true" {
		slog.Error("verify backup and restore plan before controlled migration")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := bootstrap.OpenDatabase(ctx, os.Getenv("MYSQL_DSN"))
	if err == nil {
		defer db.Close()
		err = bootstrap.Migrate(ctx, db, migrations.Files)
	}
	if err != nil {
		slog.Error("reimbursement migration failed; inspect schema before retry")
		os.Exit(1)
	}
}
