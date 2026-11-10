package bootstrap

import (
	"errors"
	"github.com/j-s-te/reimbursement/internal/jobs"
	"strconv"
	"time"
)

// approvalReminderOptions 用途：从worker环境读取有界催办周期，不忽略非法显式配置。
// 参数：get为运行环境读取函数，只读取催办专用键。
// 返回值：经校验的调度配置；错误仅包含键名。
func approvalReminderOptions(get func(string) string) (jobs.ApprovalReminderOptions, error) {
	options := jobs.ApprovalReminderOptions{OverdueAfter: 48 * time.Hour, RepeatEvery: 24 * time.Hour, PollEvery: time.Minute, BatchSize: 50}
	for _, field := range []struct {
		key   string
		value *time.Duration
	}{
		{"REIMBURSEMENT_APPROVAL_REMINDER_AFTER", &options.OverdueAfter},
		{"REIMBURSEMENT_APPROVAL_REMINDER_INTERVAL", &options.RepeatEvery},
		{"REIMBURSEMENT_APPROVAL_REMINDER_POLL", &options.PollEvery},
	} {
		if raw := get(field.key); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil || value < time.Second || value > 365*24*time.Hour {
				return options, errors.New("invalid " + field.key)
			}
			*field.value = value
		}
	}
	if raw := get("REIMBURSEMENT_APPROVAL_REMINDER_BATCH"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return options, errors.New("invalid REIMBURSEMENT_APPROVAL_REMINDER_BATCH")
		}
		options.BatchSize = value
	}
	return options, nil
}
