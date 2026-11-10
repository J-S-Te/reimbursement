package httpapi

import "github.com/gin-gonic/gin"

// invoiceControls 返回当前票据授权范围内的更正与风险记录。
// 参数：c 为票据标识所在HTTP上下文。
// 返回值：无；输出范围受控的案件及来源。
func (h handler) invoiceControls(c *gin.Context) {
	result, err := h.service.QueryInvoiceControls(c.Request.Context(), actor(c), c.Param("id"))
	h.respond(c, result, err)
}
