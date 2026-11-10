package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// TestInvoicePolicyPublishesOriginalMappingAndDefaultBlock 验证真实发布保留费用映射与默认日期策略。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestInvoicePolicyPublishesOriginalMappingAndDefaultBlock(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	admin := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.config.manage", "expense.policy.publish"}}
	body := map[string]any{"command_id": "mapping-save", "invoice_type": "TRAIN", "active": true,
		"identity_fields": []string{"invoice_number"}, "fee_type": "TRAIN_TICKET",
		"default_claim_type": "TRAVEL", "max_invoice_age_days": 30}
	draft := runCommand(t, s, admin, "config.save", "invoice_rules", body)
	id := draft["id"].(string)
	runCommand(t, s, admin, "config.publish", id, map[string]any{
		"command_id": "mapping-publish", "kind": "invoice_rules", "expected_version": 1,
	})
	var row store.Config
	if err := db.First(&row, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(row.ContentJSON), &stored); err != nil {
		t.Fatal(err)
	}
	if !row.Published || stored["fee_type"] != "TRAIN_TICKET" ||
		stored["default_claim_type"] != "TRAVEL" || stored["date_policy"] != "BLOCK" {
		t.Fatalf("发布丢失映射或默认策略: %s", row.ContentJSON)
	}
}

// TestInvoiceExpenseSuggestionsRespectTrustedInvoiceAndTenant 验证建议从数据库确认票据和发布政策生成。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestInvoiceExpenseSuggestionsRespectTrustedInvoiceAndTenant(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, _ := submissionFixture(t, db, true)
	actor.Permissions = []string{"expense.invoice.read", "expense.claim.create"}
	setInvoiceDatePolicyFixture(t, db, time.Now().Format("2006-01-02"), "WARN")
	var row store.Config
	db.First(&row, "id = ?", "rule")
	var policy map[string]any
	json.Unmarshal([]byte(row.ContentJSON), &policy)
	policy["fee_type"], policy["default_claim_type"] = "TRAIN_TICKET", "TRAVEL"
	raw, _ := json.Marshal(policy)
	db.Model(&row).Update("content_json", string(raw))
	result, err := s.InvoiceExpenseSuggestions(context.Background(), actor, []string{"invoice"})
	if err != nil || len(result) != 1 {
		t.Fatalf("无法生成确认票据建议: %+v %v", result, err)
	}
	if result[0].FeeType != "TRAIN_TICKET" || result[0].Requested != 100 ||
		result[0].DefaultClaimType != "TRAVEL" || result[0].RuleID != "rule" {
		t.Fatalf("建议未采用发布政策及票面金额: %+v", result)
	}
	actor.TenantID = "other"
	if _, err = s.InvoiceExpenseSuggestions(context.Background(), actor, []string{"invoice"}); err == nil {
		t.Fatal("跨租户票据产生了建议")
	}
	actor.TenantID, actor.SubjectID = "t", "other"
	if _, err = s.InvoiceExpenseSuggestions(context.Background(), actor, []string{"invoice"}); err == nil {
		t.Fatal("其他人的票据产生了建议")
	}
	var invoice store.Invoice
	db.First(&invoice, "id = ?", "invoice")
	var facts domain.Invoice
	json.Unmarshal([]byte(invoice.ContentJSON), &facts)
	if facts.VerificationStatus != "NOT_CHECKED" || invoice.ReservationClaimID != "" {
		t.Fatal("生成建议改变验真或占用事实")
	}
}

// TestInvoiceExpenseSuggestionsRejectUnavailableFacts 验证缺映射、未确认和占用不会生成费用建议。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestInvoiceExpenseSuggestionsRejectUnavailableFacts(t *testing.T) {
	cases := []struct{ name, review, reservation, fee, want string }{
		{"未配置费用映射", "CONFIRMED", "AVAILABLE", "", "INVOICE_MAPPING_MISSING"},
		{"解析候选未确认", "PENDING", "AVAILABLE", "TRAIN", "INVOICE_NOT_AVAILABLE"},
		{"票据已消费", "CONFIRMED", "CONSUMED", "TRAIN", "INVOICE_NOT_AVAILABLE"},
		{"票据已占用", "CONFIRMED", "RESERVED", "TRAIN", "INVOICE_NOT_AVAILABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			actor, _ := submissionFixture(t, db, true)
			actor.Permissions = []string{"expense.invoice.read", "expense.claim.create"}
			setInvoiceDatePolicyFixture(t, db, time.Now().Format("2006-01-02"), "BLOCK")
			var row store.Invoice
			db.First(&row, "id = ?", "invoice")
			var invoice domain.Invoice
			json.Unmarshal([]byte(row.ContentJSON), &invoice)
			invoice.ReviewStatus = tc.review
			raw, _ := json.Marshal(invoice)
			db.Model(&row).Updates(map[string]any{"reservation_state": tc.reservation, "content_json": string(raw)})
			var policy store.Config
			db.First(&policy, "id = ?", "rule")
			var fields map[string]any
			json.Unmarshal([]byte(policy.ContentJSON), &fields)
			fields["fee_type"] = tc.fee
			raw, _ = json.Marshal(fields)
			db.Model(&policy).Update("content_json", string(raw))
			_, err := s.InvoiceExpenseSuggestions(context.Background(), actor, []string{"invoice"})
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != tc.want {
				t.Fatalf("应拒绝%s，实际%v", tc.want, err)
			}
		})
	}
}

// TestInvoiceExpenseSuggestionsRequireCapabilitiesAndBoundSelection 验证建议接口能力与数量边界。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestInvoiceExpenseSuggestionsRequireCapabilitiesAndBoundSelection(t *testing.T) {
	s, _, _ := setupWorkflow(t)
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read"}}
	_, err := s.InvoiceExpenseSuggestions(context.Background(), actor, []string{"invoice"})
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Status != 403 {
		t.Fatalf("无草稿创建能力应拒绝，实际%v", err)
	}
	actor.Permissions = []string{"expense.claim.create"}
	_, err = s.InvoiceExpenseSuggestions(context.Background(), actor, []string{"invoice"})
	if !errors.As(err, &problem) || problem.Status != 403 {
		t.Fatalf("无票据读取能力应拒绝，实际%v", err)
	}
	actor.Permissions = []string{"expense.claim.create", "expense.invoice.read"}
	for _, ids := range [][]string{nil, make([]string, 101), {""}} {
		_, err = s.InvoiceExpenseSuggestions(context.Background(), actor, ids)
		if !errors.As(err, &problem) || problem.Code != "INVALID_INVOICE_SELECTION" {
			t.Fatalf("非法选择应拒绝，实际%v", err)
		}
	}
}

// TestInvoiceDatePolicyUsesBeijingMidnight 验证有效天数以北京日历日计数。
// 参数：t 为固定时钟测试上下文。
// 返回值：无。
func TestInvoiceDatePolicyUsesBeijingMidnight(t *testing.T) {
	rule := domain.InvoicePolicy{MaxInvoiceAgeDays: 30, DatePolicy: "BLOCK"}
	invoice := domain.Invoice{InvoiceDate: "2026-10-12"}
	before := time.Date(2026, 11, 11, 15, 59, 59, 0, time.UTC)
	if _, err := invoiceDateWarning(invoice, rule, before); err != nil {
		t.Fatalf("北京截止日内应可用: %v", err)
	}
	if _, err := invoiceDateWarning(invoice, rule, before.Add(time.Second)); err == nil {
		t.Fatal("北京午夜后超过30天必须阻断")
	}
}

// TestInvoiceDateWarningIsFrozenInApprovalSnapshot 验证真实提交将日期告警纳入不可变审批证据。
// 参数：t 为隔离业务事务测试上下文。
// 返回值：无。
func TestInvoiceDateWarningIsFrozenInApprovalSnapshot(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	date := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).AddDate(0, 0, -31)
	setInvoiceDatePolicyFixture(t, db, date.Format("2006-01-02"), "WARN")
	result := runCommand(t, s, actor, "claim.submit", id,
		map[string]any{"command_id": "warn-submit", "expected_version": 1})
	warnings, ok := result["invoice_date_warnings"].([]any)
	if !ok || len(warnings) != 1 {
		t.Fatalf("提交未返回冻结告警: %+v", result)
	}
	var snapshot store.Snapshot
	if err := db.Where("claim_id = ?", id).First(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	var frozen approvalSnapshot
	if err := json.Unmarshal([]byte(snapshot.ContentJSON), &frozen); err != nil {
		t.Fatal(err)
	}
	if len(frozen.Claim.InvoiceDateWarnings) != 1 {
		t.Fatal("审批快照遗漏时效风险")
	}
	if err := VerifyApprovalSnapshot(json.RawMessage(snapshot.ContentJSON), snapshot.ContentHash); err != nil {
		t.Fatalf("冻结告警快照摘要错误: %v", err)
	}
	frozen.Claim.InvoiceDateWarnings[0].AgeDays++
	raw, _ := json.Marshal(frozen)
	if err := VerifyApprovalSnapshot(raw, snapshot.ContentHash); err == nil {
		t.Fatal("修改日期告警未破坏审批内容摘要")
	}
}

// TestInvoicePolicyConfigurationRejectsInvalidExtensions 验证扩展配置不会被忽略后发布。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestInvoicePolicyConfigurationRejectsInvalidExtensions(t *testing.T) {
	cases := []struct {
		name, field string
		value       any
	}{
		{"费用编码过长", "fee_type", strings.Repeat("A", 65)},
		{"费用编码空白", "fee_type", " HOTEL "},
		{"费用编码控制字符", "fee_type", "HOTEL\n"},
		{"报销类型非法", "default_claim_type", "BENEFIT"},
		{"有效期负数", "max_invoice_age_days", -1},
		{"有效期过长", "max_invoice_age_days", 3651},
		{"有效期小数", "max_invoice_age_days", 1.5},
		{"日期策略非法", "date_policy", "IGNORE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"invoice_type": "VAT", "identity_fields": []string{"invoice_number"}}
			body[tc.field] = tc.value
			raw, _ := json.Marshal(body)
			if err := validateConfig("invoice_rules", string(raw)); err == nil {
				t.Fatalf("非法扩展配置被接受: %s", raw)
			}
		})
	}
}

// TestInvoiceDatePolicyBlocksAndWarnsDuringReservation 验证真实占票路径使用服务器日期。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestInvoiceDatePolicyBlocksAndWarnsDuringReservation(t *testing.T) {
	day := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600))
	cases := []struct {
		name, date, policy, want string
		warnings                 int
	}{
		{"超期默认阻断", day.AddDate(0, 0, -31).Format("2006-01-02"), "", "INVOICE_DATE_EXPIRED", 0},
		{"超期告警", day.AddDate(0, 0, -31).Format("2006-01-02"), "WARN", "", 1},
		{"截止日可用", day.AddDate(0, 0, -30).Format("2006-01-02"), "BLOCK", "", 0},
		{"非法日期阻断", "2026-02-30", "WARN", "INVOICE_DATE_INVALID", 0},
		{"未来日期阻断", day.AddDate(0, 0, 1).Format("2006-01-02"), "WARN", "INVOICE_DATE_INVALID", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, db, _ := setupWorkflow(t)
			submissionFixture(t, db, true)
			setInvoiceDatePolicyFixture(t, db, tc.date, tc.policy)
			claim, _, err := loadClaim(db, "t", "claim", false)
			if err != nil {
				t.Fatal(err)
			}
			claim.InvoiceDateWarnings = []domain.InvoiceDateWarning{{Code: "CLIENT_FORGED"}}
			err = db.Transaction(func(tx *gorm.DB) error { return reserveInvoices(tx, &claim) })
			if tc.want != "" {
				var problem *domain.Problem
				if !errors.As(err, &problem) || problem.Code != tc.want || problem.Status != 422 {
					t.Fatalf("应拒绝%s，实际%v", tc.want, err)
				}
				var row store.Invoice
				db.First(&row, "id = ?", "invoice")
				if row.ReservationClaimID != "" {
					t.Fatal("日期失败仍占票")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(claim)
			var frozen struct {
				Warnings []map[string]any `json:"invoice_date_warnings"`
			}
			json.Unmarshal(raw, &frozen)
			if len(frozen.Warnings) != tc.warnings {
				t.Fatalf("告警未冻结: %s", raw)
			}
			if tc.warnings > 0 && (frozen.Warnings[0]["invoice_id"] != "invoice" || frozen.Warnings[0]["rule_id"] != "rule") {
				t.Fatalf("告警缺少票据及政策来源: %s", raw)
			}
		})
	}
}

// setInvoiceDatePolicyFixture 将指定时效和日期写入专用测试库。
// 参数：t 为测试上下文，db 为隔离库，date 为票面日期，policy 为过期策略。
// 返回值：无。
func setInvoiceDatePolicyFixture(t *testing.T, db *gorm.DB, date, policy string) {
	t.Helper()
	var row store.Invoice
	if err := db.First(&row, "id = ?", "invoice").Error; err != nil {
		t.Fatal(err)
	}
	var inv domain.Invoice
	if err := json.Unmarshal([]byte(row.ContentJSON), &inv); err != nil {
		t.Fatal(err)
	}
	inv.InvoiceDate = date
	raw, _ := json.Marshal(inv)
	if err := db.Model(&row).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	rule := map[string]any{"id": "rule", "version": 1, "invoice_type": "VAT", "active": true,
		"identity_fields": []string{"invoice_number"}, "max_invoice_age_days": 30, "date_policy": policy}
	raw, _ = json.Marshal(rule)
	if err := db.Model(&store.Config{}).Where("id = ?", "rule").Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
}
