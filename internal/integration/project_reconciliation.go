package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/application"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type ProjectReconciliation struct{ Client *Client }

// RequireProjectTenant 绑定已由平台令牌端点取得的机器令牌租户，避免跨租户索引污染。
// 参数：ctx 为令牌边界，client 为专用支出客户端，tenant 为本地队列归属。
// 返回值：租户一致返回空；此处不是接收端验签，项目服务仍执行完整签名验证。
func RequireProjectTenant(ctx context.Context, client *Client, tenant string) error {
	if client == nil || tenant == "" {
		return errors.New("PROJECT_CLIENT_UNAVAILABLE")
	}
	token, e := client.Token(ctx)
	if e != nil {
		return e
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("PROJECT_TENANT_UNVERIFIABLE")
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return errors.New("PROJECT_TENANT_UNVERIFIABLE")
	}
	var claims struct {
		TenantID string `json:"tenant_id"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.TenantID != tenant {
		return errors.New("PROJECT_TENANT_MISMATCH")
	}
	return nil
}

// Reconcile 使用既有项目机器分页协议，租户始终与机器身份绑定。
// 参数：ctx 为网络边界，tenant 为源租户，cursor/size 为已持久分页。
// 返回值：校验后的最小项目对账页；服务失败不转换为空成功。
func (p *ProjectReconciliation) Reconcile(ctx context.Context, tenant, cursor string, size int) (application.ReconciliationPage, error) {
	var out application.ReconciliationPage
	if e := RequireProjectTenant(ctx, p.Client, tenant); e != nil {
		return out, e
	}
	if size < 1 || size > 100 || len(cursor) > 512 {
		return out, errors.New("RECONCILIATION_PAGE_INVALID")
	}
	query := url.Values{"page_size": {strconv.Itoa(size)}, "cursor": {cursor}}
	raw, e := p.Client.Call(ctx, http.MethodGet, "/internal/v1/reimbursement/spend-snapshots/reconciliation?"+query.Encode(), nil)
	if e != nil {
		return out, e
	}
	if json.Unmarshal(raw, &out) != nil || out.Items == nil {
		return out, errors.New("RECONCILIATION_RESPONSE_INVALID")
	}
	return out, nil
}
