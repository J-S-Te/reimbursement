package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type claimPreviewer interface {
	PreviewClaim(context.Context, domain.Actor, json.RawMessage) (any, error)
}

// previewRequest 构造仅含真实草稿版本及可编辑字段的预览请求。
// 参数：t 为测试上下文，c 为隔离数据库中的本人草稿。
// 返回值：不包含身份和持久化结果字段的请求字节。
func previewRequest(t *testing.T, c domain.Claim) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"claim_id": c.ID, "expected_version": c.Version, "title": c.Title,
		"claim_type": c.ClaimType, "remark": c.Remark, "org_id": c.OrgID,
		"route_type": c.RouteType, "finance_entity_id": c.FinanceEntityID,
		"payee_profile_id": c.PayeeProfileID, "items": c.Items,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestPreviewClaimUsesEditedAmountsWithoutWriting 验证编辑中金额按正式标准重算且预览零写入。
// 参数：t 为临时 SQLite 测试上下文。
// 返回值：无。
func TestPreviewClaimUsesEditedAmountsWithoutWriting(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	actor.Permissions = append(actor.Permissions, "expense.claim.update")
	c, before, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	c.Items[0].Requested = 300
	c.Items[0].Allocations[0].Amount = 300
	c.Items[0].InvoiceUsages = nil
	c.Items[0].NoInvoiceReason = "特殊费用"
	c.Items[0].ExceptionReason = "项目必要支出"
	var standard store.Config
	if err = db.First(&standard, "id = ?", "standard").Error; err != nil {
		t.Fatal(err)
	}
	var policy domain.Standard
	if err = json.Unmarshal([]byte(standard.ContentJSON), &policy); err != nil {
		t.Fatal(err)
	}
	policy.AllowNoInvoice = true
	raw, _ := json.Marshal(policy)
	if err = db.Model(&standard).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	previewer, ok := any(s).(claimPreviewer)
	if !ok {
		t.Fatal("只读预览服务尚未实现")
	}
	value, err := previewer.PreviewClaim(context.Background(), actor, previewRequest(t, c))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	var result struct {
		Items []domain.Item          `json:"items"`
		Route routeThresholdDecision `json:"route_decision"`
		Nodes []struct {
			Code  string `json:"node_code"`
			Label string `json:"node_label"`
		} `json:"approval_nodes"`
		Only bool `json:"preview_only"`
	}
	if err = json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Only || len(result.Items) != 1 || result.Items[0].StandardLimit != 200 ||
		!result.Items[0].OverStandard || !result.Route.Triggered || len(result.Nodes) != 3 ||
		result.Nodes[1].Code != "TECHNICAL_DIRECTOR" || result.Nodes[1].Label == "" {
		t.Fatalf("预览未采用编辑事实和正式审批算法: %s", encoded)
	}
	var after store.Claim
	if err = db.First(&after, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if after.ContentJSON != before.ContentJSON || after.Version != 1 {
		t.Fatal("预览修改草稿")
	}
	for _, model := range []any{&store.Task{}, &store.Snapshot{}, &store.Command{}, &store.Outbox{}} {
		var count int64
		if err = db.Model(model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("预览产生持久化副作用 %T", model)
		}
	}
	var inv store.Invoice
	if err = db.First(&inv, "id = ?", "invoice").Error; err != nil {
		t.Fatal(err)
	}
	if inv.Version != 1 || inv.ReservationClaimID != "" || inv.ReservationState != "AVAILABLE" {
		t.Fatal("预览占用或修改票据")
	}
}

// TestPreviewClaimRejectsStaleForeignAndUnavailable 验证预览版本、本人边界与项目依赖失败关闭。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无。
func TestPreviewClaimRejectsStaleForeignAndUnavailable(t *testing.T) {
	cases := []struct {
		name, subject, tenant, permission string
		version                           int64
		unavailable                       bool
		code                              string
	}{
		{"其他申请人", "other", "t", "expense.claim.update", 1, false, "FORBIDDEN"},
		{"其他租户", "app", "other", "expense.claim.update", 1, false, "RESOURCE_NOT_FOUND"},
		{"旧版本", "app", "t", "expense.claim.update", 2, false, "VERSION_CONFLICT"},
		{"缺少能力", "app", "t", "expense.claim.read", 1, false, "FORBIDDEN"},
		{"项目依赖失败", "app", "t", "expense.claim.update", 1, true, "PROJECT_REFERENCE_UNAVAILABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, db, p := setupWorkflow(t)
			_, id := submissionFixture(t, db, true)
			c, _, err := loadClaim(db, "t", id, false)
			if err != nil {
				t.Fatal(err)
			}
			c.Version = tc.version
			p.unavailable = tc.unavailable
			previewer, ok := any(s).(claimPreviewer)
			if !ok {
				t.Fatal("只读预览服务尚未实现")
			}
			_, err = previewer.PreviewClaim(context.Background(), domain.Actor{
				TenantID: tc.tenant, SubjectID: tc.subject, Permissions: []string{tc.permission},
			}, previewRequest(t, c))
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != tc.code {
				t.Fatalf("应拒绝%s，实际%v", tc.code, err)
			}
		})
	}
}

// TestPreviewClaimWithInvoicesWorksOnReadOnlyConnection 验证含票草稿预览在数据库禁止写入时仍可成功。
// 参数：t 为真实临时 SQLite 数据库测试上下文。
// 返回值：无。
func TestPreviewClaimWithInvoicesWorksOnReadOnlyConnection(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	actor.Permissions = append(actor.Permissions, "expense.claim.update")
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("PRAGMA query_only = ON").Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("PRAGMA query_only = OFF")
	value, err := s.PreviewClaim(context.Background(), actor, previewRequest(t, c))
	if err != nil {
		t.Fatalf("真正只读连接不能预览: %v", err)
	}
	raw, _ := json.Marshal(value)
	var result struct {
		Items []domain.Item `json:"items"`
		Nodes []any         `json:"approval_nodes"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].StandardLimit != 200 || result.Items[0].OverStandard || len(result.Nodes) != 2 {
		t.Fatalf("含票预览标准或节点错误: %s", raw)
	}
}

// TestPreviewClaimRejectsIdentityAndTerminalState 验证预览拒绝身份声明及不可编辑状态。
// 参数：t 为隔离草稿测试上下文。
// 返回值：无。
func TestPreviewClaimRejectsIdentityAndTerminalState(t *testing.T) {
	for _, field := range []string{"tenant_id", "Tenant_Id", "applicant_subject_id", "permissions", "status", "version"} {
		t.Run(field, func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			actor, id := submissionFixture(t, db, true)
			actor.Permissions = append(actor.Permissions, "expense.claim.update")
			c, _, err := loadClaim(db, "t", id, false)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err = json.Unmarshal(previewRequest(t, c), &body); err != nil {
				t.Fatal(err)
			}
			body[field] = nil
			raw, _ := json.Marshal(body)
			_, err = s.PreviewClaim(context.Background(), actor, raw)
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != "INVALID_REQUEST" {
				t.Fatalf("身份或状态声明应拒绝: %v", err)
			}
		})
	}
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	actor.Permissions = append(actor.Permissions, "expense.claim.update")
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	c.Status = "IN_APPROVAL"
	raw, _ := json.Marshal(c)
	if err = db.Model(&store.Claim{}).Where("id = ?", id).Updates(map[string]any{"status": c.Status, "content_json": string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = s.PreviewClaim(context.Background(), actor, previewRequest(t, c))
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "STATE_CONFLICT" {
		t.Fatalf("终态申请不得预览: %v", err)
	}
}

// TestPreviewClaimFailsClosedForInvoiceAndDirectoryFacts 验证预览保留正式票据与当前目录的拒绝结果。
// 参数：t 为隔离标准、票据和远端适配器测试上下文。
// 返回值：无。
func TestPreviewClaimFailsClosedForInvoiceAndDirectoryFacts(t *testing.T) {
	cases := []struct {
		name, code string
		change     func(*Service, *gorm.DB)
	}{
		{"未配人员目录", "PERSONNEL_DIRECTORY_UNAVAILABLE", func(s *Service, _ *gorm.DB) { s.deps.Personnel = nil }},
		{"未配文件网关", "FILE_GATEWAY_UNAVAILABLE", func(s *Service, _ *gorm.DB) { s.deps.Files = nil }},
		{"专票仍需外部验真", "INVOICE_EXTERNAL_VERIFICATION_REQUIRED", func(_ *Service, db *gorm.DB) {
			patchPolicyFixture(t, db, "rule", map[string]any{"required_external_verification": true})
		}},
		{"发票已被其他单占用", "INVOICE_RESERVED", func(_ *Service, db *gorm.DB) {
			if err := db.Model(&store.Invoice{}).Where("id = ?", "invoice").Updates(map[string]any{"reservation_claim_id": "other", "reservation_state": "RESERVED"}).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"会计范围撤销", "APPROVER_FINANCE_SCOPE_MISSING", func(_ *Service, db *gorm.DB) {
			if err := db.Where("id = ?", "fixture-acct-scope").Delete(&store.Config{}).Error; err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			actor, id := submissionFixture(t, db, true)
			actor.Permissions = append(actor.Permissions, "expense.claim.update")
			c, _, err := loadClaim(db, "t", id, false)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(s, db)
			_, err = s.PreviewClaim(context.Background(), actor, previewRequest(t, c))
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != tc.code {
				t.Fatalf("应失败关闭%s，实际%v", tc.code, err)
			}
		})
	}
}

// TestPreviewClaimMissingInvoiceUsesBusinessError 验证未找到用票时返回正式业务错误而非数据库错误。
// 参数：t 为临时数据库测试上下文。
// 返回值：无。
func TestPreviewClaimMissingInvoiceUsesBusinessError(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	actor, id := submissionFixture(t, db, true)
	actor.Permissions = append(actor.Permissions, "expense.claim.update")
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Where("id = ?", "invoice").Delete(&store.Invoice{}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = s.PreviewClaim(context.Background(), actor, previewRequest(t, c))
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "INVOICE_NOT_AVAILABLE" {
		t.Fatalf("缺票未返回业务拒绝: %v", err)
	}
}
