package main

import (
	"context"
	"github.com/j-s-te/reimbursement/internal/identity"
	"log/slog"
	"os"
	"time"
)

// main 独立发布权限目录，不依赖业务数据库和API就绪。
// 参数：publish子命令及PLATFORM_AUTHORIZATION_CATALOG相关运行凭据。
// 返回值：无；目录发布失败非零退出。
func main() {
	if len(os.Args) != 2 || os.Args[1] != "publish" {
		slog.Error("usage: authz-catalog publish")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := identity.SyncAuthorizationCatalog(ctx, identity.CatalogSyncOptions{Enabled: true, BaseURL: os.Getenv("PLATFORM_BASE_URL"), ApplicationID: os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID"), ClientID: os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID"), ClientSecret: os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET")})
	if err != nil {
		slog.Error("authorization catalog publication failed")
		os.Exit(1)
	}
}
