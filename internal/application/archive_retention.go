package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	"gorm.io/gorm"
	"time"
)

// freezeArchiveRetention 固定提交日有效的组织保留政策，未配置明确标记而不猜法定年限。
// 参数：tx 为提交事务，c 为当前组织单据，now 为真实提交时刻。
// 返回值：唯一政策快照或配置冲突错误。
func freezeArchiveRetention(tx *gorm.DB, c *domain.Claim, now time.Time) error {
	day := now.In(time.FixedZone("Asia/Shanghai", 8*3600))
	rows, err := configs(tx, c.TenantID, "archive_retention_policies", true)
	if err != nil {
		return err
	}
	snapshot := &domain.ArchiveRetentionSnapshot{Status: "UNCONFIGURED"}
	for _, row := range rows {
		var policy domain.ArchiveRetentionPolicy
		if err = json.Unmarshal([]byte(row.ContentJSON), &policy); err != nil {
			return err
		}
		date := day.Format("2006-01-02")
		if policy.OrgID != c.OrgID || date < policy.ValidFrom || policy.ValidUntil != "" && date > policy.ValidUntil {
			continue
		}
		if snapshot.PolicyID != "" {
			return domain.Fail("ARCHIVE_RETENTION_CONFLICT", "组织保留政策重叠，请先受控修复", 422)
		}
		if err = validateAdditionalPolicy("archive_retention_policies", row.ContentJSON); err != nil {
			return err
		}
		version := policy.Version
		if version < 1 {
			version = row.Version
		}
		until := retentionDate(day, policy.RetentionMonths)
		status := "ACTIVE"
		if policy.Hold {
			status = "HOLD"
		}
		snapshot = &domain.ArchiveRetentionSnapshot{Status: status, PolicyID: row.ID, PolicyVersion: version,
			RetentionMonths: policy.RetentionMonths, StartBasis: policy.StartBasis, StartDate: date,
			RetainUntil: until.Format("2006-01-02"), Hold: policy.Hold, Basis: policy.Basis}
	}
	c.ArchiveRetention = snapshot
	return nil
}

// retentionDate 按政策日历月计算期限，月末夹至目标月份最后一天。
// 参数：start 为北京时间起算日，months 为明确批准的保留月数。
// 返回值：同一时区内的到期日，不执行删除。
func retentionDate(start time.Time, months int) time.Time {
	first := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, start.Location()).AddDate(0, months, 0)
	last := first.AddDate(0, 1, -1).Day()
	day := start.Day()
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, start.Location())
}

// ArchiveRetentionStatus 解释冻结政策在当前业务日的保留状态。
// 参数：snapshot 为历史政策快照，now 为当前时刻。
// 返回值：未配置、保留、暂停处置或到期待复核；从不自动删除。
func ArchiveRetentionStatus(snapshot *domain.ArchiveRetentionSnapshot, now time.Time) string {
	if snapshot == nil || snapshot.PolicyID == "" {
		return "UNCONFIGURED"
	}
	if snapshot.Hold {
		return "HOLD"
	}
	if now.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02") > snapshot.RetainUntil {
		return "EXPIRED_REVIEW_REQUIRED"
	}
	return "ACTIVE"
}
