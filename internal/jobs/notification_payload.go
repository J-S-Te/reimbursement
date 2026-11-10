package jobs

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var notificationCode = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_:-]{0,127}$`)

// notificationPayload 用途：按冻结业务种类生成平台支持的事件及需重新鉴权的单据深链。
// 参数：row 为稳定事件标识与业务坐标，data 为事务内冻结的通知载荷。
// 返回值：平台通知DTO；收件人、种类或单据绑定不合法时返回明确错误。
func notificationPayload(row Record, data map[string]any) (map[string]any, error) {
	if row.EventID == "" {
		return nil, errors.New("INVALID_NOTIFICATION_EVENT_ID")
	}
	eventID := row.EventID
	if !notificationCode.MatchString(eventID) {
		eventID = "REIMBURSEMENT_" + eventID
	}
	if !notificationCode.MatchString(eventID) {
		return nil, errors.New("INVALID_NOTIFICATION_EVENT_ID")
	}
	recipient, ok := data["assignee_subject_id"].(string)
	if !ok || strings.TrimSpace(recipient) == "" {
		return nil, errors.New("NOTIFICATION_RECIPIENT_MISSING")
	}
	kind, ok := data["kind"].(string)
	if data["kind"] != nil && !ok {
		return nil, errors.New("NOTIFICATION_KIND_UNSUPPORTED")
	}
	if kind == "FINANCE_COMPLIANCE_RETURNED" {
		applicant, valid := data["applicant_subject_id"].(string)
		if !valid || strings.TrimSpace(applicant) == "" || recipient != applicant {
			return nil, errors.New("NOTIFICATION_RECIPIENT_INVALID")
		}
	}
	if row.ResourceID == "" || (data["claim_id"] != nil && data["claim_id"] != row.ResourceID) {
		return nil, errors.New("INVALID_NOTIFICATION_CLAIM")
	}
	eventType, title, content, err := notificationKind(kind)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"event_id": eventID, "event_type": eventType, "title": title, "content": content,
		"notification_scope": "CROSS_SYSTEM", "priority": "NORMAL",
		"target_url":     "/reimbursement/claims?" + url.Values{"claim_id": {row.ResourceID}}.Encode(),
		"reference_type": "reimbursement_claim", "reference_id": row.ResourceID,
		"recipient_user_ids": []string{recipient}, "idempotency_key": row.EventID,
		"occurred_at": row.CreatedAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

// notificationKind 用途：有限映射业务通知，兼容历史无kind的审批待办。
// 参数：kind 为业务源冻结的通知种类，空值仅表示历史审批消息。
// 返回值：平台允许的事件代码、标题、说明及错误；未知种类失败关闭。
func notificationKind(kind string) (string, string, string, error) {
	switch kind {
	case "":
		return "REIMBURSEMENT_APPROVAL_ASSIGNED", "报销审批待办", "您有一项报销审批任务，请进入审批中心核验。", nil
	case "APPROVAL_REMINDER":
		return "REIMBURSEMENT_APPROVAL_REMINDER", "报销审批超期提醒", "您有一项超期审批任务，请进入审批中心核验当前待办。", nil
	case "FINANCE_COMPLIANCE_RETURNED":
		return "REIMBURSEMENT_FINANCE_COMPLIANCE_RETURNED", "报销财务复检退回", "会计节点复检未通过，单据已退回；请查看具体原因并处理后重新提交。", nil
	case "INVOICE_MISMATCH":
		return "REIMBURSEMENT_INVOICE_MISMATCH", "发票查验不一致", "报销关联发票查验不一致，请查看单据与查验依据。", nil
	case "INVOICE_CORRECTION_REQUESTED":
		return "REIMBURSEMENT_INVOICE_CORRECTION_REQUESTED", "发票更正待处理", "报销关联发票已申请更正，请查看更正记录。", nil
	case "INVOICE_RECONFIRM_REQUIRED":
		return "REIMBURSEMENT_INVOICE_RECONFIRM_REQUIRED", "发票更正待重新确认", "发票更正已生效，请确认新票面后重新提交审批。", nil
	case "INVOICE_FINANCE_REVIEW_REQUIRED":
		return "REIMBURSEMENT_INVOICE_FINANCE_REVIEW_REQUIRED", "发票风险待财务复核", "报销关联发票存在待复核风险，请查看风险记录并联系财务处理。", nil
	default:
		return "", "", "", errors.New("NOTIFICATION_KIND_UNSUPPORTED")
	}
}
