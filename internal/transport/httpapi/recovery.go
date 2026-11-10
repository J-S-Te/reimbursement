package httpapi

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
)

// registerRecovery 注册受现有认证与请求边界保护的异常恢复入口。
// 参数：group 为已认证业务路由组。
// 返回值：无。
func (h handler) registerRecovery(group *gin.RouterGroup) {
	group.GET("/operations/:kind", h.operationsQuery)
	group.POST("/operations/:kind/:id/retry", h.operationsRetry)
	group.GET("/claims/:id/sync-status", h.syncStatus)
	group.POST("/claims/:id/spend-replays", h.spendReplay)
	group.GET("/claims/:id/spend-capacity", h.spendCapacity)
	group.GET("/approval-recovery-tasks", h.approvalRecoveryQuery)
	group.GET("/approval-recovery-tasks/:id", h.approvalRecoveryQuery)
}

// operationsQuery 查询当前主体组织范围内的失败队列和对账差异。
// 参数：c 为包含可信身份及查询种类的HTTP上下文。
// 返回值：无；输出分页结果或授权错误。
func (h handler) operationsQuery(c *gin.Context) {
	result, err := h.service.OperationsQuery(c.Request.Context(), actor(c), c.Param("kind"), "", params(c))
	h.respond(c, result, err)
}

// recoveryInput 读取恢复命令并禁止客户端自行替换原始队列载荷。
// 参数：c 为当前HTTP上下文。
// 返回值：原因与版本命令，或公开参数错误。
func recoveryInput(c *gin.Context) (application.RecoveryInput, error) {
	var input application.RecoveryInput
	raw, err := body(c)
	if err != nil {
		return input, err
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		return input, domain.Fail("INVALID_REQUEST", "恢复参数不正确", 422)
	}
	return input, nil
}

// operationsRetry 只按固定种类重投已冻结事件或受控重建差异。
// 参数：c 为恢复对象与命令所在上下文。
// 返回值：无；输出不可变幂等回执或拒绝。
func (h handler) operationsRetry(c *gin.Context) {
	input, err := recoveryInput(c)
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	result, err := h.service.RecoverIntegration(c.Request.Context(), actor(c), c.Param("kind"), c.Param("id"), input)
	h.respond(c, result, err)
}

// syncStatus 查询报销当前财务版本与项目投递状态。
// 参数：c 为单据及当前主体上下文。
// 返回值：无；仅显示当前可访问单据的同步状态。
func (h handler) syncStatus(c *gin.Context) {
	result, err := h.service.OperationsQuery(c.Request.Context(), actor(c), "sync-status", c.Param("id"), params(c))
	h.respond(c, result, err)
}

// spendReplay 从数据库权威事件恢复完整快照投递。
// 参数：c 为单据及含原因、当前版本的恢复命令。
// 返回值：无；不新增付款效果或修改财务版本。
func (h handler) spendReplay(c *gin.Context) {
	input, err := recoveryInput(c)
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	result, err := h.service.RecoverIntegration(c.Request.Context(), actor(c), "snapshot", c.Param("id"), input)
	h.respond(c, result, err)
}

// spendCapacity 返回付款前预留全额冲正空间的容量诊断。
// 参数：c 为当前可读单据上下文。
// 返回值：无；输出额度诊断或对象不可见错误。
func (h handler) spendCapacity(c *gin.Context) {
	result, err := h.service.SpendCapacity(c.Request.Context(), actor(c), c.Param("id"))
	h.respond(c, result, err)
}

// approvalRecoveryQuery 查询范围内可由异常接管命令处理的当前待办。
// 参数：c 为可信主体、可选任务标识和分页参数。
// 返回值：无；不绕过普通审批任务的本人受理限制。
func (h handler) approvalRecoveryQuery(c *gin.Context) {
	result, err := h.service.QueryApprovalRecovery(c.Request.Context(), actor(c), c.Param("id"), params(c))
	h.respond(c, result, err)
}
