package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	core "github.com/J-S-Te/license-core"
	"github.com/J-S-Te/license-core/consumer"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/config"
	"github.com/j-s-te/reimbursement/internal/costmachine"
	"github.com/j-s-te/reimbursement/internal/identity"
	"github.com/j-s-te/reimbursement/internal/integration"
	"github.com/j-s-te/reimbursement/internal/invoiceparser"
	"github.com/j-s-te/reimbursement/internal/jobs"
	"github.com/j-s-te/reimbursement/internal/transport/httpapi"
	"github.com/j-s-te/reimbursement/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Runtime struct {
	Config    config.Config
	DB        *gorm.DB
	Service   *application.Service
	Files     *integration.Files
	Processor *jobs.Processor
	License   *consumer.Gate
}

// Open 装配专库及独立用途客户端，不自动迁移或伪造运行许可。
// 参数：ctx为进程生命周期。
// 返回值：可用于API/worker的依赖集合；缺配置或许可时失败关闭。
func Open(ctx context.Context) (*Runtime, error) {
	cfg, err := config.From(os.Getenv)
	if err != nil {
		return nil, err
	}
	if os.Getenv("COMMERCIAL_LICENSE_ENABLED") != "true" || os.Getenv("COMMERCIAL_LICENSE_COMPATIBILITY") != "" {
		return nil, errors.New("commercial runtime must be explicitly enabled without compatibility bypass")
	}
	gate, err := consumer.FromEnvironment("reimbursement")
	if err != nil {
		return nil, errors.New("commercial runtime configuration unavailable")
	}
	rawDB, err := OpenDatabase(ctx, cfg.DSN)
	if err != nil {
		return nil, err
	}
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: rawDB}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), NowFunc: func() time.Time { return time.Now().UTC() }})
	if err != nil {
		rawDB.Close()
		return nil, errors.New("database initialization failed")
	}
	pending, err := Pending(ctx, rawDB, migrations.Files)
	if err != nil || len(pending) > 0 {
		rawDB.Close()
		return nil, errors.New("database migrations pending or checksum mismatch; run controlled migrate command")
	}
	reference := integration.NewClient(cfg.PlatformURL, cfg.ProjectURL, cfg.ReferenceID, cfg.ReferenceSecret, "reimbursement.project_reference.read")
	files := integration.NewFiles(db, integration.NewClient(cfg.PlatformURL, cfg.FileURL, cfg.FileID, cfg.FileSecret, "platform:file:upload platform:file:bind platform:file:download"))
	service := application.New(db, application.Dependencies{Projects: &integration.Projects{Client: reference}, Files: files, Personnel: &integration.Personnel{Client: integration.NewClient(cfg.PlatformURL, cfg.PlatformURL, cfg.OwnerID, cfg.OwnerSecret, "owner_directory.read")}, Environment: cfg.Environment})
	processor := &jobs.Processor{Personnel: &integration.Personnel{Client: integration.NewClient(cfg.PlatformURL, cfg.PlatformURL, cfg.OwnerID, cfg.OwnerSecret, "owner_directory.read")}, License: gate, DB: db, Service: service, Files: files, Audit: integration.NewClient(cfg.PlatformURL, cfg.PlatformURL, cfg.AuditID, cfg.AuditSecret, "audit.ingest"), Notifications: integration.NewClient(cfg.PlatformURL, cfg.PlatformURL, cfg.NotificationID, cfg.NotificationSecret, "notification.ingest"), Project: integration.NewClient(cfg.PlatformURL, cfg.ProjectURL, cfg.SpendID, cfg.SpendSecret, "project.reimbursement_cost.write"), Environment: cfg.Environment, Parser: invoiceparser.New(invoiceparser.Limits{})}
	return &Runtime{Config: cfg, DB: db, Service: service, Files: files, Processor: processor, License: gate}, nil
}

// authorizedMiddleware 从持久RP会话派生主体，逐次校验敏感写权限与同源边界。
// 参数：auth为OIDC认证器；runtime为运行依赖。
// 返回值：Gin认证中间件，不接受请求头伪造的tenant或actor。
func authorizedMiddleware(auth *identity.OIDCAuthenticator, runtime *Runtime) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != "GET" && c.Request.Method != "HEAD" && c.Request.Method != "OPTIONS" && !sameOrigin(c.Request, runtime.Config.PublicURL) {
			c.AbortWithStatusJSON(403, gin.H{"code": "CSRF_FORBIDDEN", "message": "请求来源校验失败"})
			return
		}
		principal, err := auth.Authenticate(c.Request.Context(), c.Request)
		if err != nil {
			status := 503
			code := "AUTHORIZATION_UNAVAILABLE"
			if errors.Is(err, identity.ErrUnauthenticated) {
				status = 401
				code = "SESSION_EXPIRED"
			}
			if errors.Is(err, identity.ErrAuthorizationDenied) {
				status = 403
				code = "FORBIDDEN"
			}
			c.AbortWithStatusJSON(status, gin.H{"code": code, "message": "当前会话或应用授权不可用"})
			return
		}
		operation := core.MUTATE_BUSINESS
		if c.Request.Method == "GET" {
			operation = core.READ_HISTORY
		}
		borrowDownload := strings.HasPrefix(c.Request.URL.Path, "/api/v1/archive-borrows/") && strings.HasSuffix(c.Request.URL.Path, "/download")
		if borrowDownload || strings.HasPrefix(c.Request.URL.Path, "/api/v1/archive-packages/") || strings.HasSuffix(c.Request.URL.Path, "/packages") || strings.HasSuffix(c.Request.URL.Path, "/download-ticket") || c.Request.URL.Path == "/api/v1/exports" || strings.HasPrefix(c.Request.URL.Path, "/api/v1/exports/") {
			operation = core.EXPORT_HISTORY
		}
		if err = runtime.License.Check(c.Request.Context(), operation); err != nil {
			c.AbortWithStatusJSON(403, gin.H{"code": "COMMERCIAL_LICENSE_DENIED", "message": "当前运行许可不允许此操作"})
			return
		}
		c.Set("expense_actor", principal.Actor())
		c.Next()
	}
}

// sameOrigin 精确比较公开来源并要求显式CSRF头，反向代理头不参与信任推断。
// 参数：request为原始请求；public为配置的公开源。
// 返回值：当前请求是否通过同源写入检查。
func sameOrigin(request *http.Request, public string) bool {
	origin, err := url.Parse(request.Header.Get("Origin"))
	expected, e := url.Parse(public)
	return err == nil && e == nil && origin.Scheme == expected.Scheme && origin.Host == expected.Host && origin.Path == "" && origin.RawQuery == "" && request.Header.Get("X-CSRF-Token") == "1"
}

// RunAPI 启动报销接口与认证路由，退出时排空HTTP并关闭专库。
// 参数：无，运行参数来自显式环境配置。
// 返回值：配置、装配或监听失败错误。
func RunAPI() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime, err := Open(ctx)
	if err != nil {
		return err
	}
	rawDB, _ := runtime.DB.DB()
	defer rawDB.Close()
	cfg := runtime.Config
	auth, err := identity.NewOIDCAuthenticator(ctx, oidcOptions(cfg), identity.NewGORMOIDCStore(runtime.DB), cfg.SessionKey)
	if err != nil {
		return errors.New("OIDC initialization failed")
	}
	costVerifier, err := costmachine.New(cfg.CostMachine)
	if err != nil {
		return errors.New("cost machine verifier initialization failed")
	}
	router := httpapi.New(runtime.Service, authorizedMiddleware(auth, runtime), &httpapi.CostMachineOptions{Verifier: costVerifier, CheckLicense: runtime.License.Check})
	if err = router.SetTrustedProxies(nil); err != nil {
		return err
	}
	registerAuth(router, auth, runtime)
	router.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"status": "alive"}) })
	router.GET("/readyz", func(c *gin.Context) {
		value, ready := runtimeReadiness(c.Request.Context(), runtime)
		status := 200
		if !ready {
			status = 503
		}
		c.JSON(status, value)
	})
	syncDone := make(chan error, 1)
	go func() {
		syncDone <- runtime.License.Run(ctx, func(error) { slog.Warn("commercial runtime synchronization temporarily unavailable") })
	}()
	server := &http.Server{Addr: cfg.Address, Handler: router, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ListenAndServe() }()
	select {
	case err = <-serveDone:
		stop()
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err = server.Shutdown(shutdown)
		cancel()
	}
	stop()
	select {
	case <-syncDone:
	case <-time.After(5 * time.Second):
		return errors.New("license synchronization did not stop")
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// registerAuth 绑定独立登录、会话、退出与中央活动路由。
// 参数：router为业务路由；auth为认证器；runtime提供公开来源及配置。
// 返回值：无。
func registerAuth(router *gin.Engine, auth *identity.OIDCAuthenticator, runtime *Runtime) {
	router.GET("/auth/login", gin.WrapF(auth.Login))
	router.GET("/auth/callback", gin.WrapF(auth.Callback))
	router.POST("/auth/backchannel-logout", gin.WrapF(auth.BackchannelLogout))
	router.GET("/auth/logout", gin.WrapF(auth.Logout))
	router.POST("/auth/local-logout", func(c *gin.Context) {
		if !sameOrigin(c.Request, runtime.Config.PublicURL) {
			c.JSON(403, gin.H{"code": "CSRF_FORBIDDEN", "message": "请求来源校验失败"})
			return
		}
		auth.LogoutLocal(c.Writer, c.Request)
	})
	router.GET("/logged-out", func(c *gin.Context) { c.Redirect(302, "/portal") })
	router.GET("/auth/session", func(c *gin.Context) {
		p, err := auth.Authenticate(c.Request.Context(), c.Request)
		if err != nil {
			status := 503
			if errors.Is(err, identity.ErrUnauthenticated) {
				status = 401
			}
			c.JSON(status, gin.H{"code": "SESSION_EXPIRED", "message": "当前会话不可用"})
			return
		}
		actor := p.Actor()
		c.Header("Cache-Control", "no-store")
		c.JSON(200, gin.H{"data": gin.H{"tenant_id": actor.TenantID, "subject_id": actor.SubjectID, "identity_id": actor.SubjectID, "user_id": actor.SubjectID, "display_name": p.DisplayName, "permissions": actor.Permissions, "roles": p.Roles, "authorization_revision": p.AuthorizationRevision, "catalog_version": p.CatalogVersion}})
	})
	router.POST("/auth/activity", gin.WrapF(auth.Activity))
}

// RunWorker 启动独立解析归档与投递消费者，所有任务通过数据库租约恢复。
// 参数：无，使用与API同版本的显式运行配置。
// 返回值：停止或队列故障错误。
func RunWorker() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime, err := Open(ctx)
	if err != nil {
		return err
	}
	rawDB, _ := runtime.DB.DB()
	defer rawDB.Close()
	owner, err := os.Hostname()
	if err != nil {
		return err
	}
	owner += "-" + time.Now().UTC().Format("20060102T150405.000000000")
	q := jobs.Queue{DB: runtime.DB, Owner: owner, Lease: 4 * time.Minute}
	reminderOptions, err := approvalReminderOptions(os.Getenv)
	if err != nil {
		return err
	}
	done := make(chan error, 7)
	health := http.NewServeMux()
	health.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"alive"}`))
	})
	health.HandleFunc("/readyz", workerReadinessHandler(runtime))
	health.HandleFunc("/metrics", metricsHandler(runtime))
	healthAddress := os.Getenv("REIMBURSEMENT_WORKER_HEALTH_ADDRESS")
	if healthAddress == "" {
		healthAddress = ":9097"
	}
	server := &http.Server{Addr: healthAddress, Handler: health, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() { done <- server.ListenAndServe() }()
	go func() { done <- q.Run(ctx, "exp_job", runtime.Processor.Job) }()
	go func() { done <- q.Run(ctx, "exp_outbox", runtime.Processor.Outbox) }()
	go func() { done <- runtime.Processor.RunReconciliation(ctx) }()
	go func() { done <- runtime.Processor.RunArchiveDownloadRetention(ctx) }()
	go func() { done <- runtime.Processor.RunApprovalReminders(ctx, reminderOptions) }()
	go func() {
		done <- runtime.License.Run(ctx, func(error) { slog.Warn("commercial runtime synchronization temporarily unavailable") })
	}()
	remaining := 7
	select {
	case err = <-done:
		remaining--
		stop()
	case <-ctx.Done():
		err = nil
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if shutdownErr := server.Shutdown(shutdown); shutdownErr != nil {
		return errors.New("worker health shutdown failed")
	}
	for i := 0; i < remaining; i++ {
		select {
		case <-done:
		case <-shutdown.Done():
			return errors.New("worker drain timed out")
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
