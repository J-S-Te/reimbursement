package jobs

import core "github.com/J-S-Te/license-core"

// outboxLicenseOperation 用途：区分不可变事件投递与重建投影的业务副作用。
// 参数：topic 为持久化任务的固定服务端种类。
// 返回值：仅四类已审核事件投递为基础服务；重建和未知任务要求业务写许可。
func outboxLicenseOperation(topic string) core.Operation {
	switch topic {
	case "project", "project_spend", "audit", "notification":
		return core.ESSENTIAL_SERVICE
	default:
		return core.MUTATE_BUSINESS
	}
}
