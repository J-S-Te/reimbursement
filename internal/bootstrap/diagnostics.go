package bootstrap

import (
	"context"
	"encoding/json"
	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/reimbursement/internal/observability"
	"net/http"
	"time"
)

// runtimeReadiness 返回数据库、运行许可及配置分项；配置存在不冒充远端连通。
// 参数：ctx 为健康检查边界，runtime 为当前已装配应用。
// 返回值：分项诊断与整体可受理标记，不包含凭据或地址。
func runtimeReadiness(ctx context.Context, runtime *Runtime) (map[string]any, bool) {
	db, err := runtime.DB.DB()
	database := err == nil
	if database {
		database = db.PingContext(ctx) == nil
	}
	license := runtime.License.Check(ctx, core.MUTATE_BUSINESS) == nil
	cfg := runtime.Config
	configured := map[string]bool{"project_reference": cfg.ReferenceID != "" && cfg.ReferenceSecret != "", "project_spend": cfg.SpendID != "" && cfg.SpendSecret != "", "file_gateway": cfg.FileID != "" && cfg.FileSecret != "", "audit": cfg.AuditID != "" && cfg.AuditSecret != "", "notification": cfg.NotificationID != "" && cfg.NotificationSecret != ""}
	ready := database && license
	for _, value := range configured {
		ready = ready && value
	}
	status := "ready"
	if !ready {
		status = "not_ready"
	}
	return map[string]any{"status": status, "database": database, "commercial_license": license, "integration_configured": configured, "remote_connectivity": "CHECKED_ON_BUSINESS_REQUEST"}, ready
}

// metricsHandler 只暴露内部worker技术指标，查询失败返回不可用而非伪造零。
// 参数：runtime 为当前进程依赖。
// 返回值：私网监控HTTP处理函数。
func metricsHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if runtime.License.Check(ctx, core.ESSENTIAL_SERVICE) != nil {
			http.Error(w, "metrics unavailable", 503)
			return
		}
		body, err := observability.Metrics(ctx, runtime.DB, time.Now().UTC())
		if err != nil {
			http.Error(w, "metrics unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(body))
	}
}

// workerReadinessHandler 输出内部就绪分项而不触发业务写入或外部探测。
// 参数：runtime 为当前进程依赖。
// 返回值：就绪HTTP处理函数。
func workerReadinessHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		value, ready := runtimeReadiness(ctx, runtime)
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(503)
		}
		_ = json.NewEncoder(w).Encode(value)
	}
}
