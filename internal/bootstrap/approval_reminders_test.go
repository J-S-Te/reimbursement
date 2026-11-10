package bootstrap

import (
	"testing"
	"time"
)

// TestApprovalReminderConfiguration 用途：核验默认催办周期及非法配置失败关闭。
// 参数：t 为配置测试上下文。
// 返回值：无；错用单位、忽略非法值或扫描无界时失败。
func TestApprovalReminderConfiguration(t *testing.T) {
	options, err := approvalReminderOptions(func(string) string { return "" })
	if err != nil || options.OverdueAfter != 48*time.Hour || options.RepeatEvery != 24*time.Hour || options.BatchSize != 50 {
		t.Fatalf("defaults=%+v err=%v", options, err)
	}
	for _, env := range []map[string]string{{"REIMBURSEMENT_APPROVAL_REMINDER_AFTER": "bad"}, {"REIMBURSEMENT_APPROVAL_REMINDER_BATCH": "1000"}, {"REIMBURSEMENT_APPROVAL_REMINDER_INTERVAL": "0s"}} {
		if _, err := approvalReminderOptions(func(key string) string { return env[key] }); err == nil {
			t.Fatal("invalid reminder configuration accepted")
		}
	}
}
