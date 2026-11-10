package application

import (
	"errors"
	"regexp"

	"gorm.io/gorm"
)

var workerReasonCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,99}$`)

// AppendWorkerAudit 将后台业务结果与脱敏系统审计写入同一调用方事务。
// 参数：tx为结果事务；tenant/resource为归属；action为动作；taskID为关联任务；result/reason为结果及原因码。
// 返回值：非法事件或可靠投递记录落库错误，调用方必须整体回滚。
func AppendWorkerAudit(tx *gorm.DB, tenant, resource, action, taskID, result, reason string) error {
	switch action {
	case "invoice.parse.completed", "invoice.parse.failed", "archive.sealed", "archive.package.completed", "archive.package.failed", "export.completed":
	default:
		return errors.New("WORKER_AUDIT_ACTION_INVALID")
	}
	if tenant == "" || resource == "" || taskID == "" ||
		(result != "SUCCESS" && result != "FAILURE") {
		return errors.New("WORKER_AUDIT_CONTEXT_INVALID")
	}
	if reason != "" && !workerReasonCode.MatchString(reason) {
		reason = "PROCESSING_FAILED"
	}
	return appendOutbox(tx, tenant, "audit", resource, map[string]any{
		"actor_subject_id": "reimbursement-worker", "actor_type": "SYSTEM",
		"action": action, "resource_id": resource, "command_id": taskID,
		"request_id": taskID, "result": result, "reason_code": reason,
	})
}
