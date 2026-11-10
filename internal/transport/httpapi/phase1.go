package httpapi

import (
	"bytes"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/reimbursement/internal/domain"
	"io"
	"net/http"
)

// registerPhaseOne 用途：接线一期预览、异常池、统计与借阅，复用可信身份及错误信封。
// 参数：group 为已配置认证和商业许可门禁的业务路由组。
// 返回值：无。
func (h handler) registerPhaseOne(group *gin.RouterGroup) {
	group.GET("/parse-exceptions", h.parseExceptions)
	group.POST("/claims/:id/preview", h.claimPreview)
	group.GET("/statistics", h.statistics)
	group.GET("/archive-projects", h.archiveProjects)
	group.POST("/invoice-expense-suggestions", h.invoiceExpenseSuggestions)
	group.POST("/archives/:id/borrow-requests", h.borrowRequest)
	group.GET("/archive-borrows", h.borrowList)
	group.GET("/archive-borrows/:id", h.borrowDetail)
	group.POST("/archive-borrows/:id/decisions", h.borrowDecision)
	group.POST("/archive-borrows/:id/download", h.borrowDownload)
}

// parseExceptions 用途：分页呈现尚未形成业务发票的本人原件解析异常。
// 参数：c为已认证请求，查询条件不得扩大原件所有权。
// 返回值：无；真实异常分页或公开错误写入统一信封。
func (h handler) parseExceptions(c *gin.Context) {
	value, err := h.service.ParseExceptions(c.Request.Context(), actor(c), params(c))
	h.respond(c, value, err)
}

// claimPreview 用途：按URL绑定本人草稿的只读标准及当前审批路线预览。
// 参数：c为含草稿版本和可编辑字段的请求，载荷目标须与URL一致。
// 返回值：无；不生成命令或持久业务结果。
func (h handler) claimPreview(c *gin.Context) {
	raw, err := body(c)
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	var input struct {
		ClaimID string `json:"claim_id"`
	}
	if json.Unmarshal(raw, &input) != nil || input.ClaimID != c.Param("id") {
		h.respond(c, nil, domain.Fail("INVALID_REQUEST", "预览草稿标识须与请求地址一致", 422))
		return
	}
	value, err := h.service.PreviewClaim(c.Request.Context(), actor(c), raw)
	h.respond(c, value, err)
}

// borrowRequest 用途：为当前已有整单范围的封存档案提交受控借阅申请。
// 参数：c为认证请求，id为不可变档案版本。
// 返回值：无；幂等回执或错误写入HTTP信封。
func (h handler) borrowRequest(c *gin.Context) {
	raw, err := body(c)
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	value, err := h.service.RequestArchiveBorrow(c.Request.Context(), actor(c), c.Param("id"), raw)
	h.respond(c, value, err)
}

// borrowDecision 用途：由独立档案人员批准或拒绝借阅，不扩大申请人原范围。
// 参数：c为带申请版本、决定及原因的认证请求。
// 返回值：无；真实审核回执或错误写入HTTP信封。
func (h handler) borrowDecision(c *gin.Context) {
	raw, err := body(c)
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	value, err := h.service.DecideArchiveBorrow(c.Request.Context(), actor(c), c.Param("id"), raw)
	h.respond(c, value, err)
}

// borrowList 用途：分页读取本人或当前有权审核的借阅记录。
// 参数：c为可信主体及受控分页查询。
// 返回值：无；不把无权限记录计入可见总数。
func (h handler) borrowList(c *gin.Context) {
	value, err := h.service.ArchiveBorrows(c.Request.Context(), actor(c), params(c))
	h.respond(c, value, err)
}

// borrowDetail 用途：读取当前可见借阅及真实生成物状态。
// 参数：c为认证请求，id为借阅坐标。
// 返回值：无；失效、撤权及生成物状态由服务端返回。
func (h handler) borrowDetail(c *gin.Context) {
	value, err := h.service.ArchiveBorrow(c.Request.Context(), actor(c), c.Param("id"))
	h.respond(c, value, err)
}

// borrowDownload 用途：重验借阅许可、当前范围与完整性后提供下载。
// 参数：c为当前借阅人的同源写请求。
// 返回值：无；审计或摘要失败时不输出ZIP字节。
func (h handler) borrowDownload(c *gin.Context) {
	name, content, err := h.service.ReadArchiveBorrow(c.Request.Context(), actor(c), c.Param("id"))
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Data(http.StatusOK, "application/zip", content)
}

// archiveProjects 用途：返回当前整单档案范围内的项目聚合，不提升任务可见范围。
// 参数：c为认证请求，查询日期使用实际封存时间。
// 返回值：无；聚合结果附本次可信查询主体，防止跨系统目录发生会话切换混淆。
func (h handler) archiveProjects(c *gin.Context) {
	value, err := h.service.ArchiveProjects(c.Request.Context(), actor(c), params(c))
	if err == nil {
		page, ok := value.(map[string]any)
		if !ok {
			h.respond(c, nil, domain.Fail("INVALID_RESPONSE", "档案目录暂不可用", 503))
			return
		}
		a := actor(c)
		page["viewer"] = map[string]string{"tenant_id": a.TenantID, "subject_id": a.SubjectID}
	}
	h.respond(c, value, err)
}

// statistics 用途：返回当前授权范围的真实报销统计。
// 参数：c 为带可信主体及白名单筛选条件的请求。
// 返回值：无；业务结果或错误写入HTTP信封。
func (h handler) statistics(c *gin.Context) {
	value, err := h.service.Statistics(c.Request.Context(), actor(c), params(c))
	h.respond(c, value, err)
}

// invoiceExpenseSuggestions 用途：从已确认本人票据生成需人工完善的费用建议。
// 参数：c 为仅携带票据坐标的有界请求。
// 返回值：无；建议不会创建草稿或改动核验状态。
func (h handler) invoiceExpenseSuggestions(c *gin.Context) {
	raw, err := body(c)
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	var input struct {
		InvoiceIDs []string `json:"invoice_ids"`
	}
	if err = decodePhaseOne(raw, &input); err != nil {
		h.respond(c, nil, err)
		return
	}
	value, err := h.service.InvoiceExpenseSuggestions(c.Request.Context(), actor(c), input.InvoiceIDs)
	h.respond(c, value, err)
}

// decodePhaseOne 用途：拒绝未声明参数及尾随JSON，避免业务坐标请求夹带身份声明。
// 参数：raw为有界请求字节，target为固定业务请求DTO。
// 返回值：格式非法返回公开422错误，否则为空。
func decodePhaseOne(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return domain.Fail("INVALID_REQUEST", "请求参数不正确", 422)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return domain.Fail("INVALID_REQUEST", "请求必须只有一个JSON对象", 422)
	}
	return nil
}
