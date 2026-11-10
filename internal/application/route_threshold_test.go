package application

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// TestRouteThresholdsUseStrictPerItemOr 验证真实审批规划按单项超额和严格OR升级。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestRouteThresholdsUseStrictPerItemOr(t *testing.T) {
	cases := []struct {
		name                            string
		requested, limit, basis, amount int64
		count                           int
		travel, want                    bool
	}{
		{"旧规则任意超标", 101, 100, 0, 0, 1, false, true},
		{"未超标不升级", 100, 100, 0, 0, 1, false, false},
		{"金额相等不升级", 120, 100, 0, 20, 1, false, false},
		{"金额少一分不升级", 119, 100, 0, 20, 1, false, false},
		{"金额多一分升级", 121, 100, 0, 20, 1, false, true},
		{"比例相等不升级", 120, 100, 2000, 0, 1, false, false},
		{"比例低于不升级", 119, 100, 2000, 0, 1, false, false},
		{"比例高于升级", 121, 100, 2000, 0, 1, false, true},
		{"仅比例命中OR", 115, 100, 1000, 100, 1, false, true},
		{"仅金额命中OR", 121, 100, 5000, 20, 1, false, true},
		{"两阈值均未命中", 110, 100, 2000, 20, 1, false, false},
		{"两阈值恰好相等", 120, 100, 2000, 20, 1, false, false},
		{"比例小数精确大于", 4, 3, 3333, 0, 1, false, true},
		{"比例小数精确小于", 4, 3, 3334, 0, 1, false, false},
		{"多项累计不能升级", 110, 100, 1500, 15, 2, false, false},
		{"差旅异常无条件升级", 100, 100, 1000000, 1000, 1, true, true},
		{"大数比例不溢出", math.MaxInt64, math.MaxInt64/2 + 1, 10000, 0, 1, false, false},
		{"大数比例确实超限", math.MaxInt64, 1, 1000000, 0, 1, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, claim, refs := routeThresholdFixture(t, tc.requested, tc.limit, tc.count)
			claim.Items[0].TravelException = tc.travel
			setRouteThresholdFixture(t, db, tc.basis, domain.Money(tc.amount))
			plan, err := buildApprovalPlan(db, claim, refs)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, task := range plan.Tasks {
				found = found || task.NodeRole == "TECHNICAL_DIRECTOR"
			}
			if found != tc.want {
				t.Fatalf("阈值升级=%v，期望%v", found, tc.want)
			}
			if claim.Items[0].OverStandard != (tc.requested > tc.limit) {
				t.Fatal("阈值计算篡改了超标事实")
			}
		})
	}
}

// TestRouteThresholdConfigurationRejectsInvalidValues 验证发布校验拒绝负数、非整数及越界阈值。
// 参数：t 为配置测试上下文。
// 返回值：无。
func TestRouteThresholdConfigurationRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		field string
		value any
	}{
		{"extra_threshold_basis_points", -1}, {"extra_threshold_basis_points", 1000001},
		{"extra_threshold_basis_points", 1.5}, {"extra_threshold_basis_points", "2000"},
		{"extra_threshold_basis_points", nil},
		{"extra_threshold_minor", "-1"}, {"extra_threshold_minor", "9223372036854775808"},
		{"extra_threshold_minor", 20}, {"extra_threshold_minor", "01"},
	}
	for _, tc := range cases {
		body := map[string]any{"org_id": "org", "route_type": "TECHNICAL", "valid_from": "2026-01-01",
			"base_role": "PROJECT_EXPENSE_OWNER", "extra_role": "TECHNICAL_DIRECTOR", "accounting_role": "ACCOUNTANT"}
		body[tc.field] = tc.value
		raw, _ := json.Marshal(body)
		err := validateConfig("route_rules", string(raw))
		var problem *domain.Problem
		if !errors.As(err, &problem) || problem.Status != 422 {
			t.Fatalf("非法阈值未返回422: %s，错误%v", raw, err)
		}
	}
}

// TestRouteThresholdSubmitKeepsReasonAndImmutableDecision 验证低于升级门槛仍须超标说明且冻结完整判定。
// 参数：t 为独立数据库中的真实提交测试上下文。
// 返回值：无。
func TestRouteThresholdSubmitKeepsReasonAndImmutableDecision(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	setRouteThresholdFixture(t, db, 0, 20)
	var standard store.Config
	if err := db.First(&standard, "id = ?", "standard").Error; err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal([]byte(standard.ContentJSON), &body)
	body["limit_minor"] = "80"
	raw, _ := json.Marshal(body)
	if err := db.Model(&standard).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	_, err := s.Execute(context.Background(), actor, "claim.submit", id,
		json.RawMessage(`{"command_id":"threshold-no-reason","expected_version":1}`))
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "EXCEPTION_REASON_REQUIRED" {
		t.Fatalf("低于升级阈值仍必须说明: %v", err)
	}
	claim, _, err := loadClaim(db, actor.TenantID, id, false)
	if err != nil {
		t.Fatal(err)
	}
	claim.Items[0].ExceptionReason = "业务产生必要差额"
	raw, _ = json.Marshal(claim)
	if err := db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	runCommand(t, s, actor, "claim.submit", id,
		map[string]any{"command_id": "threshold-submit", "expected_version": 1})
	var row store.Snapshot
	if err := db.Where("claim_id = ?", id).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	var frozen approvalSnapshot
	if err := json.Unmarshal([]byte(row.ContentJSON), &frozen); err != nil {
		t.Fatal(err)
	}
	d := frozen.Approval.RouteDecision
	if !frozen.Claim.Items[0].OverStandard || d == nil || d.Triggered || d.ExtraThresholdMinor != 20 {
		t.Fatalf("超标事实或阈值判定错误: %s", row.ContentJSON)
	}
	if err := VerifyApprovalSnapshot(json.RawMessage(row.ContentJSON), row.ContentHash); err != nil {
		t.Fatal(err)
	}
	d.ExtraThresholdMinor++
	raw, _ = json.Marshal(frozen)
	if err := VerifyApprovalSnapshot(raw, row.ContentHash); err == nil {
		t.Fatal("篡改阈值未被内容摘要发现")
	}
}

// TestRouteThresholdDecisionFreezesPolicyAndReasons 验证新审批计划保存阈值版本和逐项判定原因。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestRouteThresholdDecisionFreezesPolicyAndReasons(t *testing.T) {
	db, claim, refs := routeThresholdFixture(t, 121, 100, 1)
	setRouteThresholdFixture(t, db, 2000, 20)
	plan, err := buildApprovalPlan(db, claim, refs)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plan)
	var frozen struct {
		Decision struct {
			RuleID      string       `json:"rule_id"`
			RuleVersion int64        `json:"rule_version"`
			Basis       int64        `json:"extra_threshold_basis_points"`
			Amount      domain.Money `json:"extra_threshold_minor"`
			Triggered   bool         `json:"triggered"`
			Items       []struct {
				Reasons []string `json:"reasons"`
			} `json:"items"`
		} `json:"route_decision"`
	}
	json.Unmarshal(raw, &frozen)
	d := frozen.Decision
	if d.RuleID != "route" || d.RuleVersion != 1 || d.Basis != 2000 || d.Amount != 20 ||
		!d.Triggered || len(d.Items) != 1 || len(d.Items[0].Reasons) != 2 {
		t.Fatalf("未冻结政策或OR判定理由: %s", raw)
	}
}

// TestRouteThresholdPublicationPreservesAndLocksValues 验证阈值随政策发布并拒绝覆盖历史版本。
// 参数：t 为隔离配置命令测试上下文。
// 返回值：无。
func TestRouteThresholdPublicationPreservesAndLocksValues(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	admin := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.config.manage", "expense.policy.publish"}}
	body := map[string]any{"command_id": "threshold-save", "org_id": "org", "route_type": "TECHNICAL",
		"valid_from": "2026-01-01", "base_role": "PROJECT_EXPENSE_OWNER", "extra_role": "TECHNICAL_DIRECTOR",
		"accounting_role": "ACCOUNTANT", "extra_threshold_basis_points": 2000, "extra_threshold_minor": "20000"}
	draft := runCommand(t, s, admin, "config.save", "route_rules", body)
	id := draft["id"].(string)
	runCommand(t, s, admin, "config.publish", id,
		map[string]any{"command_id": "threshold-publish", "kind": "route_rules", "expected_version": 1})
	var row store.Config
	if err := db.First(&row, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	var rule domain.RouteRule
	json.Unmarshal([]byte(row.ContentJSON), &rule)
	if !row.Published || rule.ExtraThresholdBasisPoints != 2000 || rule.ExtraThresholdMinor != 20000 {
		t.Fatalf("发布丢失阈值: %s", row.ContentJSON)
	}
	body["id"], body["command_id"], body["expected_version"] = id, "threshold-overwrite", 2
	body["extra_threshold_basis_points"] = 1000
	raw, _ := json.Marshal(body)
	_, err := s.Execute(context.Background(), admin, "config.save", "route_rules", raw)
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "PUBLISHED_IMMUTABLE" {
		t.Fatalf("已发布阈值被覆盖: %v", err)
	}
}

// TestRouteThresholdLegacySnapshotOmitsNewDecision 验证没有新决策的历史快照保持原摘要结构。
// 参数：t 为无外部依赖的快照兼容测试上下文。
// 返回值：无。
func TestRouteThresholdLegacySnapshotOmitsNewDecision(t *testing.T) {
	claim := domain.Claim{ID: "legacy", CurrentRound: 1}
	plan := approvalPlan{RuleVersion: "legacy-route"}
	hash, err := approvedContentHash(claim, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim.ContentHash = hash
	raw, err := json.Marshal(approvalSnapshot{Claim: claim, Approval: plan})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "route_decision") {
		t.Fatalf("历史结构新增非空字段: %s", raw)
	}
	if err := VerifyApprovalSnapshot(raw, hash); err != nil {
		t.Fatal(err)
	}
}

// routeThresholdFixture 构造独立数据库和已经重算标准的费用规划输入。
// 参数：t 为测试上下文，requested/limit 为每项金额和标准，count 为费用项数量。
// 返回值：隔离库、费用单和真实项目适配夹具产生的引用。
func routeThresholdFixture(t *testing.T, requested, limit int64, count int) (*gorm.DB, domain.Claim, []domain.ProjectReference) {
	t.Helper()
	_, db, project := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	claim, _, err := loadClaim(db, actor.TenantID, id, false)
	if err != nil {
		t.Fatal(err)
	}
	item := claim.Items[0]
	item.Requested, item.StandardLimit = domain.Money(requested), domain.Money(limit)
	item.OverStandard, item.ExceptionReason = requested > limit, "合理业务原因"
	claim.Items = nil
	for i := 0; i < count; i++ {
		copy := item
		copy.ID = string(rune('a' + i))
		claim.Items = append(claim.Items, copy)
	}
	refs, err := project.Validate(context.Background(), actor, id, 1, claimReferences(claim))
	if err != nil {
		t.Fatal(err)
	}
	return db, claim, refs
}

// setRouteThresholdFixture 只更新专用测试政策JSON以驱动真实审批规划。
// 参数：t 为测试上下文，db 为隔离库，basis 为基点，amount 为超额分。
// 返回值：无。
func setRouteThresholdFixture(t *testing.T, db *gorm.DB, basis int64, amount domain.Money) {
	t.Helper()
	var row store.Config
	if err := db.First(&row, "id = ?", "route").Error; err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(row.ContentJSON), &body); err != nil {
		t.Fatal(err)
	}
	body["extra_threshold_basis_points"], body["extra_threshold_minor"] = basis, amount
	raw, _ := json.Marshal(body)
	if err := db.Model(&row).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
}
