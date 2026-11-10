package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestClaimEditingPersistsTypeRemarkAndDerivedAllocations 防止单据编辑字段丢失及分摊索引使用客户端金额。
// 参数：t 为测试上下文。
// 返回值：无。
func TestClaimEditingPersistsTypeRemarkAndDerivedAllocations(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{
		"expense.claim.create", "expense.claim.update", "expense.claim.read",
	}}
	body := map[string]any{
		"command_id": "edit-ratio-create", "title": "比例分摊", "claim_type": "TRAVEL",
		"remark": "完整备注", "items": []any{map[string]any{
			"id": "item", "requested_minor": "101", "allocation_mode": "RATIO",
			"allocation_rule_version": "client-value", "allocations": []any{
				map[string]any{"project_id": "b", "ratio_basis_points": 5000},
				map[string]any{"project_id": "a", "ratio_basis_points": 5000},
			},
		}},
	}
	created := runCommand(t, s, a, "claim.create", "", body)
	if created["claim_type"] != "TRAVEL" || created["remark"] != "完整备注" {
		t.Fatalf("单据字段未保存: %+v", created)
	}
	var refs []store.ClaimReference
	if err := db.Order("project_id").Find(&refs).Error; err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].Amount != 51 || refs[1].Amount != 50 {
		t.Fatalf("索引应保存派生整数分: %+v", refs)
	}
	body["command_id"], body["expected_version"] = "edit-ratio-update", 1
	body["claim_type"], body["remark"] = "PROJECT_EXECUTION", "修改备注"
	updated := runCommand(t, s, a, "claim.update", created["id"].(string), body)
	if updated["claim_type"] != "PROJECT_EXECUTION" || updated["remark"] != "修改备注" {
		t.Fatalf("新字段未更新: %+v", updated)
	}
}

// TestClaimEditingRejectsInvalidTypeRemarkAndRatio 防止非法新字段与零分比例持久化。
// 参数：t 为测试上下文。
// 返回值：无。
func TestClaimEditingRejectsInvalidTypeRemarkAndRatio(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
	}{
		{"type", map[string]any{"claim_type": "COST_CENTER"}},
		{"remark", map[string]any{"remark": strings.Repeat("字", 2001)}},
		{"ratio", map[string]any{"items": []any{map[string]any{
			"requested_minor": "1", "allocation_mode": "RATIO", "allocations": []any{
				map[string]any{"project_id": "a", "ratio_basis_points": 5000},
				map[string]any{"project_id": "b", "ratio_basis_points": 5000},
			},
		}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.create"}}
			tc.body["command_id"] = "invalid-edit-" + tc.name
			raw, _ := json.Marshal(tc.body)
			_, err := s.Execute(context.Background(), a, "claim.create", "", raw)
			var p *domain.Problem
			if !errors.As(err, &p) || p.Status != 422 {
				t.Fatalf("非法编辑应422拒绝: %v", err)
			}
			var count int64
			if err = db.Model(&store.Claim{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("拒绝后不应留下草稿: %d, %v", count, err)
			}
		})
	}
}

// TestClaimEditingKeepsLegacyTypeEmptyAndUnicodeRemark 防止历史空类型回填和中文备注按字节误限。
// 参数：t 为测试上下文。
// 返回值：无。
func TestClaimEditingKeepsLegacyTypeEmptyAndUnicodeRemark(t *testing.T) {
	s, _, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.create"}}
	remark := strings.Repeat("字", 2000)
	r := runCommand(t, s, a, "claim.create", "", map[string]any{
		"command_id": "legacy-type-empty", "remark": remark,
	})
	if typ, _ := r["claim_type"].(string); typ != "" || r["remark"] != remark {
		t.Fatal("历史空类型应保留，2000字备注应可保存")
	}
}

// TestClaimEditingClearsServerInvoiceDateWarnings 防止客户端或上一轮日期结论进入新草稿。
// 参数：t 为测试上下文。
// 返回值：无。
func TestClaimEditingClearsServerInvoiceDateWarnings(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{
		"expense.claim.create", "expense.claim.update",
	}}
	created := runCommand(t, s, a, "claim.create", "", map[string]any{
		"command_id": "warning-create", "invoice_date_warnings": []any{
			map[string]any{"invoice_id": "forged", "code": "CLIENT_WARNING"},
		},
	})
	if _, ok := created["invoice_date_warnings"]; ok {
		t.Fatal("不应接受客户端日期风险结论")
	}
	id := created["id"].(string)
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	c.InvoiceDateWarnings = []domain.InvoiceDateWarning{{InvoiceID: "old", Code: "OLD_WARNING"}}
	raw, _ := json.Marshal(c)
	if err = db.Model(&store.Claim{}).Where("id = ?", id).
		Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	updated := runCommand(t, s, a, "claim.update", id, map[string]any{
		"command_id": "warning-update", "expected_version": 1,
	})
	if _, ok := updated["invoice_date_warnings"]; ok {
		t.Fatal("编辑新草稿应清除上一轮服务器日期结论")
	}
}
