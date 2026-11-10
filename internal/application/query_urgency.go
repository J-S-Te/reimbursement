package application

import (
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// enrichTaskUrgency 按真实任务最近激活或变更时间补充48小时展示提示，不授予办理权限。
// 参数：item为受控任务视图，row为持久任务，c为当前单据，now为服务端计算时刻。
// 返回值：无，非当前待办或时间未知时不显示超期。
func enrichTaskUrgency(item map[string]any, row store.Task, c domain.Claim, now time.Time) {
	item["pending_since"], item["urgency"] = nil, "NORMAL"
	item["urgency_basis"] = "TASK_LAST_UPDATED"
	if row.Status != "PENDING" || row.Round != c.CurrentRound || c.Status != "IN_APPROVAL" {
		return
	}
	since := row.CreatedAt
	if row.UpdatedAt.After(since) {
		since = row.UpdatedAt
	}
	if since.IsZero() {
		return
	}
	// 与催办扫描一致，转交、加签等任务变更会重新起算保守展示窗口。
	item["pending_since"] = since
	if !since.After(now) && now.Sub(since) >= 48*time.Hour {
		item["urgency"] = "OVERDUE"
	}
}
