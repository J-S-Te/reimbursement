package migrations

import "embed"

// Files 保存按版本顺序执行的报销数据库迁移。
//
//go:embed *.sql
var Files embed.FS
