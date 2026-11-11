package httpapi

import (
	"context"
	core "github.com/J-S-Te/license-core"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/reimbursement/internal/costmachine"
	"strings"
	"time"
)

type CostMachineOptions struct {
	Verifier     *costmachine.Verifier
	CheckLicense func(context.Context, core.Operation) error
}

// registerCostCandidates 注册隔离于浏览器会话的最小财务机器读取入口。
// 参数：router为业务路由，options为专用验签及许可门禁。
// 返回值：无；未完整装配时入口明确503。
func (h handler) registerCostCandidates(router *gin.Engine, options *CostMachineOptions) {
	router.GET("/internal/v1/settlement/cost-candidates", func(c *gin.Context) {
		if options == nil || options.Verifier == nil || options.CheckLicense == nil {
			c.JSON(503, gin.H{"code": "COST_MACHINE_DISABLED", "message": "专属成本读取用途未配置"})
			return
		}
		bearer := c.GetHeader("Authorization")
		if !strings.HasPrefix(bearer, "Bearer ") {
			c.JSON(401, gin.H{"code": "COST_MACHINE_UNAUTHORIZED"})
			return
		}
		tenant, err := options.Verifier.Verify(strings.TrimPrefix(bearer, "Bearer "), time.Now().UTC())
		if err != nil {
			c.JSON(401, gin.H{"code": "COST_MACHINE_UNAUTHORIZED"})
			return
		}
		if options.CheckLicense(c.Request.Context(), core.READ_HISTORY) != nil {
			c.JSON(403, gin.H{"code": "COMMERCIAL_LICENSE_DENIED"})
			return
		}
		query := c.Request.URL.Query()
		claim := query.Get("claim_id")
		if len(query) > 1 || (len(query) == 1 && len(query["claim_id"]) != 1) || len(claim) > 64 || strings.TrimSpace(claim) != claim {
			c.JSON(422, gin.H{"code": "INVALID_REQUEST"})
			return
		}
		snapshot, err := h.service.CostCandidates(c.Request.Context(), tenant, claim)
		if err != nil {
			h.respond(c, nil, err)
			return
		}
		c.JSON(200, gin.H{"code": "OK", "data": snapshot})
	})
}
