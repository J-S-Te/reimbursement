package application

import (
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
)

// TestSubmissionFreezesExplicitRetentionPolicy 验证新审批轮次固定组织政策版本和明确保留边界。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestSubmissionFreezesExplicitRetentionPolicy(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	seedPolicy(t, db, "archive_retention_policies", domain.ArchiveRetentionPolicy{ID: "retention", Version: 3, OrgID: "org", RetentionMonths: 12, StartBasis: "SUBMISSION_DATE", Basis: "组织书面档案制度", ValidFrom: "2026-01-01", Published: true})
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "retention-submit", "expected_version": 1})
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.ArchiveRetention == nil || c.ArchiveRetention.PolicyID != "retention" || c.ArchiveRetention.PolicyVersion != 3 || c.ArchiveRetention.Status != "ACTIVE" {
		t.Fatalf("未冻结明确保留政策: %+v", c.ArchiveRetention)
	}
	start, err := time.Parse("2006-01-02", c.ArchiveRetention.StartDate)
	if err != nil || c.ArchiveRetention.RetainUntil != start.AddDate(0, 12, 0).Format("2006-01-02") {
		t.Fatal("保留期限未按显式月数计算")
	}
}

// TestSubmissionMarksUnconfiguredRetention 验证无档案政策时明确标记而不默认法定年限。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestSubmissionMarksUnconfiguredRetention(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "unconfigured-retention", "expected_version": 1})
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.ArchiveRetention == nil || c.ArchiveRetention.Status != "UNCONFIGURED" || c.ArchiveRetention.RetainUntil != "" {
		t.Fatalf("未配置政策被隐式赋期限: %+v", c.ArchiveRetention)
	}
}
