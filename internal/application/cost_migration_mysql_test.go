package application_test

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/reimbursement/internal/bootstrap"
	"github.com/j-s-te/reimbursement/migrations"
)

// TestCostMigrationsMySQL 在授权私有隔离库验证真实迁移及幂等重放，不使用业务库。
// 参数：t为测试上下文，DSN只从明确的0600文件读取。
// 返回值：无，路径、库边界或迁移错误即失败。
func TestCostMigrationsMySQL(t *testing.T) {
	path := os.Getenv("REIMBURSEMENT_COST_TEST_DSN_FILE")
	if path == "" {
		t.Skip("explicit private isolated DSN file required")
	}
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private DSN file required")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal("read isolated DSN failed")
	}
	cfg, e := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if e != nil {
		t.Fatal("invalid isolated DSN")
	}
	host, _, e := net.SplitHostPort(cfg.Addr)
	if e != nil || cfg.Net != "tcp" || host != "127.0.0.1" || !strings.HasPrefix(cfg.DBName, "reimbursement_test_") {
		t.Fatal("isolated reimbursement_test_ loopback schema required")
	}
	db, e := bootstrap.OpenDatabase(context.Background(), cfg.FormatDSN())
	if e != nil {
		t.Fatal("open isolated DB failed")
	}
	defer db.Close()
	for range 2 {
		if e = bootstrap.Migrate(context.Background(), db, migrations.Files); e != nil {
			t.Fatal("isolated migration failed", e)
		}
	}
}
