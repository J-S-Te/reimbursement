package httpapi

import "github.com/gin-gonic/gin"

// approvalRounds 展示当前授权范围内的各轮冻结责任链。
// 参数：c 为单据和分页所在HTTP上下文。
// 返回值：无；输出经过服务端范围裁剪的历史。
func (h handler) approvalRounds(c *gin.Context) {
	result, err := h.service.ApprovalRounds(c.Request.Context(), actor(c), c.Param("id"), params(c))
	h.respond(c, result, err)
}
