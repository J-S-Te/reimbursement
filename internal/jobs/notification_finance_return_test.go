package jobs

import (
	"testing"
	"time"
)

// TestFinanceComplianceReturnNotification 仅向被退回单据的申请人发送事实通知。
// 参数：t为有限事件映射测试上下文。
// 返回值：无；未知映射或收件人不匹配时失败。
func TestFinanceComplianceReturnNotification(t *testing.T) {
	row := Record{EventID: "return-event", ResourceID: "claim", CreatedAt: time.Now().UTC()}
	data := map[string]any{"kind": "FINANCE_COMPLIANCE_RETURNED", "claim_id": "claim", "applicant_subject_id": "applicant", "assignee_subject_id": "applicant"}
	value, err := notificationPayload(row, data)
	if err != nil {
		t.Fatal(err)
	}
	if value["event_type"] != "REIMBURSEMENT_FINANCE_COMPLIANCE_RETURNED" {
		t.Fatalf("wrong event: %v", value)
	}
	recipients := value["recipient_user_ids"].([]string)
	if len(recipients) != 1 || recipients[0] != "applicant" {
		t.Fatalf("wrong recipient: %v", recipients)
	}
	data["assignee_subject_id"] = "accountant"
	if _, err = notificationPayload(row, data); err == nil {
		t.Fatal("return reason must not be sent to another subject")
	}
}
