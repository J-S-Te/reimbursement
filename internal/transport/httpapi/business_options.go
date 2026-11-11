package httpapi

import (
	"encoding/json"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/reimbursement/internal/domain"
)

// businessOptions 用途：将固定对象选择入口交给各自受限的业务服务。
// 参数：kind为路由注册时固定的用途类型，不接受浏览器指定任意查询对象。
// 返回值：已通过统一认证的HTTP处理器；业务错误保留原状态与追踪号。
func (h handler) businessOptions(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, size, err := optionPagination(c)
		if err != nil {
			h.respond(c, nil, err)
			return
		}
		value, err := h.readBusinessOptions(c, kind, page, size)
		h.respond(c, value, err)
	}
}

// readBusinessOptions 用途：按固定业务入口传递可信主体与有界查询条件。
// 参数：c为当前请求；kind为固定选择类型；page/size为已校验分页。
// 返回值：业务对象安全分页或同单选项；权限、归属及状态由服务端逐项复核。
func (h handler) readBusinessOptions(c *gin.Context, kind string, page, size int) (json.RawMessage, error) {
	ctx, a, keyword := c.Request.Context(), actor(c), c.Query("keyword")
	switch kind {
	case "personnel":
		return h.service.PersonnelOptions(ctx, a, c.Query("purpose"), c.Query("resource_id"), keyword, page, size)
	case "documents":
		return h.service.DocumentOptions(ctx, a, c.Query("purpose"), keyword, page, size)
	case "invoices":
		return h.service.OwnInvoiceOptions(ctx, a, keyword, page, size)
	case "organizations":
		return h.service.OrganizationOptions(ctx, a, c.Query("subject_id"), keyword, page, size)
	case "configurations":
		return h.service.ConfigurationOptions(ctx, a, c.Query("kind"), keyword, page, size)
	case "payment":
		return h.service.PaymentOptions(ctx, a, c.Param("id"))
	case "resolution":
		return h.service.ExceptionResolutionOptions(ctx, a, c.Param("id"))
	default:
		return nil, domain.Fail("OPTIONS_INVALID", "不支持的业务对象选择", 422)
	}
}

// optionPagination 用途：限制选择器分页，拒绝非法页码而不悄悄扩大结果。
// 参数：c为携带可选page/page_size的请求。
// 返回值：默认第一页及20项；非法或超范围参数返回422。
func optionPagination(c *gin.Context) (int, int, error) {
	page, pErr := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, sErr := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if pErr != nil || sErr != nil || page < 1 || page > 1000 || size < 1 || size > 50 {
		return 0, 0, domain.Fail("OPTIONS_INVALID", "请选择有效页码和每页1至50项", 422)
	}
	return page, size, nil
}
