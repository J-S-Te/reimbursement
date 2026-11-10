package bootstrap

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"io/fs"
	"sort"
	"strings"
)

// OpenDatabase 打开独立MySQL连接并限制连接与等待时长。
// 参数：ctx为启动边界；dsn为配置中的报销专库连接串。
// 返回值：已连通数据库；错误不含凭据。
func OpenDatabase(ctx context.Context, dsn string) (*sql.DB, error) {
	cfg, err := driver.ParseDSN(dsn)
	if err != nil || cfg.DBName == "" {
		return nil, errors.New("invalid database configuration")
	}
	cfg.MultiStatements = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, errors.New("database open failed")
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("database unavailable")
	}
	return db, nil
}

// Pending 检查递增SQL与已应用摘要，启动不会自动迁移业务库。
// 参数：ctx为取消边界；db为独立报销库；files为编译内嵌迁移。
// 返回值：未应用文件；已应用SQL被篡改或数据库失败返回错误。
func Pending(ctx context.Context, db *sql.DB, files fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var exists int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='exp_schema_migration'").Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return names, nil
	}
	pending := []string{}
	for _, name := range names {
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(data)
		var stored []byte
		err = db.QueryRowContext(ctx, "SELECT checksum FROM exp_schema_migration WHERE name=?", name).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			pending = append(pending, name)
			continue
		}
		if err != nil {
			return nil, err
		}
		if string(hash[:]) != string(stored) {
			return nil, fmt.Errorf("migration checksum mismatch: %s", name)
		}
	}
	return pending, nil
}

// Migrate 在专用连接持有MySQL命名锁期间执行版本化SQL。
// 参数：ctx为受控迁移上下文；db为专库；files为内嵌迁移。
// 返回值：失败立即停止；MySQL DDL非事务，失败需按运行手册核验再恢复。
func Migrate(ctx context.Context, db *sql.DB, files fs.FS) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var locked int
	if err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(CONCAT(DATABASE(),':reimbursement:migrate'),30)").Scan(&locked); err != nil || locked != 1 {
		return errors.New("migration lock unavailable")
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK(CONCAT(DATABASE(),':reimbursement:migrate'))")
	if _, err = conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS exp_schema_migration(name VARCHAR(255) PRIMARY KEY,checksum BINARY(32) NOT NULL,applied_at DATETIME(3) NOT NULL) ENGINE=InnoDB"); err != nil {
		return err
	}
	pending, err := Pending(ctx, db, files)
	if err != nil {
		return err
	}
	for _, name := range pending {
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(body)
		if _, err = conn.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s failed; inspect schema before retry", name)
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO exp_schema_migration(name,checksum,applied_at) VALUES(?,?,UTC_TIMESTAMP(3))", name, hash[:]); err != nil {
			return err
		}
	}
	return nil
}
