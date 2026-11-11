package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
)

type handler struct{ service *application.Service }

// New 装配报销业务 API，所有业务入口均经过可信 RP 会话中间件。
// 参数：service 为应用服务，authenticate 为必须设置 expense_actor 的认证中间件，costOptions为独立机器用途。
// 返回值：具有请求边界和统一错误响应的 Gin 引擎。
func New(service *application.Service, authenticate gin.HandlerFunc, costOptions ...*CostMachineOptions) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(func(c *gin.Context) {
		id := make([]byte, 16)
		if _, e := rand.Read(id); e != nil {
			c.AbortWithStatusJSON(503, gin.H{"code": "REQUEST_ID_UNAVAILABLE", "message": "请求追踪服务暂不可用"})
			return
		}
		value := hex.EncodeToString(id)
		c.Set("request_id", value)
		c.Header("X-Request-ID", value)
		c.Next()
	})
	h := handler{service: service}
	var cost *CostMachineOptions
	if len(costOptions) > 0 {
		cost = costOptions[0]
	}
	h.registerCostCandidates(r, cost)
	group := r.Group("/api/v1")
	if authenticate == nil {
		group.Use(func(c *gin.Context) {
			h.respond(c, nil, domain.Fail("AUTHORIZATION_UNAVAILABLE", "认证服务未配置", 503))
			c.Abort()
		})
	} else {
		group.Use(authenticate)
	}
	group.Use(func(c *gin.Context) {
		if _, ok := c.Get("expense_actor"); !ok {
			h.respond(c, nil, domain.Fail("SESSION_EXPIRED", "请重新登录", 401))
			c.Abort()
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
		c.Next()
	})
	for _, route := range []struct{ path, kind string }{
		{"/personnel-options", "personnel"}, {"/invoice-document-options", "documents"},
		{"/invoice-options", "invoices"}, {"/organization-options", "organizations"},
		{"/configuration-options", "configurations"}, {"/claims/:id/payment-options", "payment"},
		{"/payment-exceptions/:id/resolution-options", "resolution"},
	} {
		group.GET(route.path, h.businessOptions(route.kind))
	}
	for _, route := range []struct{ path, kind string }{{"/claims", "claims"}, {"/claims/:id", "claim"}, {"/approval-tasks", "approval-tasks"}, {"/approval-tasks/:id", "approval-task"}, {"/invoices", "invoices"}, {"/invoices/:id", "invoice"}, {"/payments/:id", "payment"}, {"/claims/:id/payments", "payments"}, {"/claims/:id/reversal-requests", "reversal-requests"}, {"/claims/:id/archives", "archives"}, {"/commands/:id", "commands"}, {"/dashboard", "dashboard"}, {"/claim-options", "claim-options"}, {"/invoice-documents/:id", "documents"}, {"/parse-runs/:id", "jobs"}, {"/exports/:id", "exports"}} {
		group.GET(route.path, h.query(route.kind))
	}
	for _, route := range []struct{ method, path, operation string }{{"POST", "/claims", "claim.create"}, {"PATCH", "/claims/:id", "claim.update"}, {"POST", "/claims/:id/submit", "claim.submit"}, {"POST", "/claims/:id/withdraw", "claim.withdraw"}, {"POST", "/claims/:id/void", "claim.void"}, {"POST", "/invoices", "invoice.create"}, {"POST", "/invoices/:id/confirm", "invoice.confirm"}, {"POST", "/invoices/:id/verify", "invoice.verify"}, {"POST", "/approval-tasks/:id/actions", "approval.action"}, {"POST", "/claims/:id/payments", "payment.record"}, {"POST", "/payments/:id/reversal-requests", "reversal.request"}, {"POST", "/reversal-requests/:id/decisions", "reversal.decide"}, {"POST", "/claims/:id/archive-retries", "archive.retry"}} {
		group.Handle(route.method, route.path, h.command(route.operation))
	}
	group.POST("/approval-tasks/:id/recover", h.command("approval.recover"))
	group.POST("/claims/:id/reopen", h.command("claim.reopen"))
	group.POST("/claims/:id/approved-void", h.command("claim.approved_void"))
	group.POST("/claims/:id/payment-exceptions", h.command("payment.exception.record"))
	group.GET("/claims/:id/payment-exceptions", h.query("payment-exceptions"))
	group.GET("/payment-exceptions/:id", h.query("payment-exception"))
	group.POST("/payment-exceptions/:id/reviews", h.command("payment.exception.review"))
	group.GET("/configurations/:kind", h.configQuery)
	group.POST("/configurations/:kind", h.configSave)
	group.POST("/configurations/:kind/:id/publish", h.configPublish)
	group.POST("/configurations/:kind/:id/retire", h.configLifecycle("config.retire"))
	group.POST("/configurations/:kind/:id/replace", h.configLifecycle("config.replace"))
	group.GET("/claims/:id/approval-rounds", h.approvalRounds)
	group.GET("/approval-tasks/:id/history", h.query("approval-history"))
	group.POST("/claims/:id/evidence/:file_id/download-ticket", h.downloadEvidence)
	group.GET("/project-options", h.projects)
	group.POST("/project-references/validate", h.validateProjects)
	group.POST("/invoice-documents/upload-sessions", h.upload)
	group.POST("/invoice-documents/:id/complete", h.complete)
	group.POST("/invoice-documents/:id/parse", h.parse)
	group.GET("/invoices/:id/controls", h.invoiceControls)
	group.GET("/invoices/:id/sources", h.sources)
	group.POST("/invoices/:id/corrections", h.command("invoice.correction.request"))
	group.POST("/invoice-corrections/:id/decisions", h.command("invoice.correction.decide"))
	group.POST("/claims/:id/invoice-reconfirmations", h.command("claim.invoice.reconfirm"))
	group.POST("/invoices/:id/sources", h.command("invoice.source.append"))
	group.POST("/invoices/:id/risks", h.command("invoice.risk.record"))
	group.POST("/invoice-risks/:id/resolutions", h.command("invoice.risk.close"))
	group.POST("/invoices/:id/files/:file_id/download-ticket", h.download)
	group.POST("/exports", h.export)
	group.POST("/archives/:id/packages", h.archivePackage)
	group.GET("/archives/:id/packages", h.archivePackages)
	group.GET("/archive-packages/:id", h.archivePackageStatus)
	group.POST("/archive-packages/:id/download", h.archiveDownload)
	h.registerRecovery(group)
	h.registerPhaseOne(group)
	return r
}

// actor 仅提取认证中间件建立的身份，忽略客户端自报租户和主体。
// 参数：c 为当前 HTTP 上下文。
// 返回值：可信身份，缺失时为空身份并被应用层拒绝。
func actor(c *gin.Context) domain.Actor {
	v, _ := c.Get("expense_actor")
	a, _ := v.(domain.Actor)
	a.RequestID = c.GetString("request_id")
	return a
}

// params 提取有限查询参数供应用层按白名单解释。
// 参数：c 为 HTTP 上下文。
// 返回值：各参数首值。
func params(c *gin.Context) map[string]string {
	out := map[string]string{}
	for key, values := range c.Request.URL.Query() {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

// body 限制请求体并拒绝缺失或畸形 JSON。
// 参数：c 为 HTTP 上下文。
// 返回值：原始请求或公开参数错误。
func body(c *gin.Context) (json.RawMessage, error) {
	raw, e := io.ReadAll(c.Request.Body)
	if e != nil {
		return nil, domain.Fail("REQUEST_TOO_LARGE", "请求超过大小限制", 413)
	}
	if !json.Valid(raw) {
		return nil, domain.Fail("INVALID_REQUEST", "请求必须是有效JSON", 422)
	}
	return raw, nil
}

// respond 统一输出业务 data 与脱敏错误，不泄露 SQL 和堆栈。
// 参数：c 为 HTTP 上下文，value 为结果，err 为应用错误。
// 返回值：无。
func (h handler) respond(c *gin.Context, value any, err error) {
	if err != nil {
		var p *domain.Problem
		var typeError *json.UnmarshalTypeError
		var syntaxError *json.SyntaxError
		var numberError *strconv.NumError
		if errors.As(err, &typeError) || errors.As(err, &syntaxError) || errors.As(err, &numberError) {
			err = domain.Fail("INVALID_REQUEST", "参数类型、金额或JSON格式不正确", 422)
		}
		if errors.As(err, &p) {
			c.JSON(p.Status, gin.H{"code": p.Code, "message": p.Message, "field_errors": p.FieldErrors, "request_id": c.GetString("request_id")})
		} else {
			c.JSON(500, gin.H{"code": "INTERNAL_ERROR", "message": "业务处理失败，请保留命令标识稍后查询", "request_id": c.GetString("request_id")})
		}
		return
	}
	status := c.GetInt("response_status")
	if status == 0 {
		status = 200
	}
	c.JSON(status, gin.H{"data": value})
}

// query 生成当前身份范围内的只读处理器。
// 参数：kind 为应用查询类型。
// 返回值：HTTP处理函数。
func (h handler) query(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		r, e := h.service.Query(c.Request.Context(), actor(c), kind, c.Param("id"), params(c))
		h.respond(c, r, e)
	}
}

// command 将一次请求作为单个原子业务命令交付应用层。
// 参数：operation 为固定服务器动作。
// 返回值：HTTP处理函数。
func (h handler) command(operation string) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, e := body(c)
		if e != nil {
			h.respond(c, nil, e)
			return
		}
		r, e := h.service.Execute(c.Request.Context(), actor(c), operation, c.Param("id"), raw)
		h.respond(c, r, e)
	}
}

// configQuery 查询指定类型配置页。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) configQuery(c *gin.Context) {
	r, e := h.service.Query(c.Request.Context(), actor(c), "configurations", c.Param("kind"), params(c))
	h.respond(c, r, e)
}

// configPayload 将配置种类绑定到受控路由而非浏览器字段。
// 参数：c 为 HTTP 上下文。
// 返回值：绑定种类后的配置请求或参数错误。
func configPayload(c *gin.Context) (json.RawMessage, error) {
	raw, e := body(c)
	if e != nil {
		return nil, e
	}
	var fields map[string]any
	if e = json.Unmarshal(raw, &fields); e != nil || fields == nil {
		return nil, domain.Fail("INVALID_REQUEST", "配置必须是JSON对象", 422)
	}
	fields["kind"] = c.Param("kind")
	return json.Marshal(fields)
}

// configSave 持久化配置草稿。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) configSave(c *gin.Context) {
	raw, e := configPayload(c)
	if e != nil {
		h.respond(c, nil, e)
		return
	}
	r, e := h.service.Execute(c.Request.Context(), actor(c), "config.save", c.Param("kind"), raw)
	h.respond(c, r, e)
}

// configPublish 发布只读政策版本。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) configPublish(c *gin.Context) {
	raw, e := configPayload(c)
	if e != nil {
		h.respond(c, nil, e)
		return
	}
	r, e := h.service.Execute(c.Request.Context(), actor(c), "config.publish", c.Param("id"), raw)
	h.respond(c, r, e)
}

// projects 搜索当前主体可关联的项目与服务项。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) projects(c *gin.Context) {
	size, _ := strconv.Atoi(c.Query("page_size"))
	r, e := h.service.ProjectOptions(c.Request.Context(), actor(c), c.Query("keyword"), c.Query("cursor"), size)
	h.respond(c, r, e)
}

// validateProjects 复核当前本人草稿而非客户端自报项目结论。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) validateProjects(c *gin.Context) {
	var input struct {
		ClaimID         string `json:"claim_id"`
		DraftVersion    int64  `json:"draft_version"`
		ExpectedVersion int64  `json:"expected_version"`
	}
	if e := c.ShouldBindJSON(&input); e != nil {
		h.respond(c, nil, domain.Fail("INVALID_REQUEST", "请提供单据及草稿版本", 422))
		return
	}
	if input.DraftVersion == 0 {
		input.DraftVersion = input.ExpectedVersion
	}
	r, e := h.service.ValidateProjects(c.Request.Context(), actor(c), input.ClaimID, input.DraftVersion)
	h.respond(c, r, e)
}

// upload 创建平台真实文件上传会话。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) upload(c *gin.Context) {
	raw, e := body(c)
	if e != nil {
		h.respond(c, nil, e)
		return
	}
	r, e := h.service.CreateUpload(c.Request.Context(), actor(c), raw)
	h.respond(c, r, e)
}

// complete 确认网关已就绪文件并排队解析。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) complete(c *gin.Context) {
	c.Set("response_status", 202)
	r, e := h.service.CompleteUpload(c.Request.Context(), actor(c), c.Param("id"))
	h.respond(c, r, e)
}

// sources 返回当前本人可见票据的来源文件引用。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) sources(c *gin.Context) {
	r, e := h.service.InvoiceSources(c.Request.Context(), actor(c), c.Param("id"))
	h.respond(c, r, e)
}

// download 当前鉴权后申请一次性原件票据。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) download(c *gin.Context) {
	r, e := h.service.DownloadInvoice(c.Request.Context(), actor(c), c.Param("id"), c.Param("file_id"))
	h.respond(c, r, e)
}

// export 创建小批量受控异步导出任务。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) export(c *gin.Context) {
	c.Set("response_status", 202)
	raw, e := body(c)
	if e != nil {
		h.respond(c, nil, e)
		return
	}
	r, e := h.service.StartExport(c.Request.Context(), actor(c), raw)
	h.respond(c, r, e)
}

// parse 查询当前解析运行或重试失败任务。
// 参数：c 为 HTTP 上下文。
// 返回值：无。
func (h handler) parse(c *gin.Context) {
	c.Set("response_status", 202)
	raw, e := body(c)
	if e != nil {
		h.respond(c, nil, e)
		return
	}
	r, e := h.service.ParseDocument(c.Request.Context(), actor(c), c.Param("id"), raw)
	h.respond(c, r, e)
}

// configLifecycle 将退休或替换种类绑定到可信路由，并交付单个原子命令。
// 参数：operation 为固定生命周期动作。
// 返回值：HTTP处理函数。
func (h handler) configLifecycle(operation string) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, e := configPayload(c)
		if e != nil {
			h.respond(c, nil, e)
			return
		}
		r, e := h.service.Execute(c.Request.Context(), actor(c), operation, c.Param("id"), raw)
		h.respond(c, r, e)
	}
}

// downloadEvidence 根据费用附件关系和当前范围签发短效证据票据。
// 参数：c 为当前HTTP上下文。
// 返回值：无。
func (h handler) downloadEvidence(c *gin.Context) {
	r, e := h.service.DownloadClaimEvidence(c.Request.Context(), actor(c), c.Param("id"), c.Param("file_id"))
	h.respond(c, r, e)
}

// archivePackage 为已封存档案版本创建按需下载包任务。
// 参数：c 为包含档案标识及当前版本的HTTP上下文。
// 返回值：无。
func (h handler) archivePackage(c *gin.Context) {
	raw, e := body(c)
	if e != nil {
		h.respond(c, nil, e)
		return
	}
	r, e := h.service.Execute(c.Request.Context(), actor(c), "archive.package.create", c.Param("id"), raw)
	h.respond(c, r, e)
}

// archivePackages 列出该封存档案版本下当前主体可恢复的下载包元数据，不返回文件内容。
// 参数：c 为包含档案标识与分页参数的HTTP上下文。
// 返回值：无。
func (h handler) archivePackages(c *gin.Context) {
	r, e := h.service.ArchivePackages(c.Request.Context(), actor(c), c.Param("id"), params(c))
	h.respond(c, r, e)
}

// archivePackageStatus 返回档案包真实异步状态、失败原因与完成后摘要。
// 参数：c 为包含原建包任务标识的HTTP上下文。
// 返回值：无。
func (h handler) archivePackageStatus(c *gin.Context) {
	r, e := h.service.ArchivePackageStatus(c.Request.Context(), actor(c), c.Param("id"))
	h.respond(c, r, e)
}

// archiveDownload 每次下载重新校验当前整单授权和包哈希后回传ZIP，不把ID或票据当授权。
// 参数：c 为HTTP上下文；:id 为已完成建包任务。
// 返回值：无。
func (h handler) archiveDownload(c *gin.Context) {
	name, content, e := h.service.ReadArchivePackage(c.Request.Context(), actor(c), c.Param("id"))
	if e != nil {
		h.respond(c, nil, e)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Data(http.StatusOK, "application/zip", content)
}
