package application

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"testing"
)

// TestRecoveryPreservesPayloadAndReplays 验证恢复仅重置失败队列并冻结幂等结果。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRecoveryPreservesPayloadAndReplays(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	if e := db.AutoMigrate(RecoveryModels()...); e != nil {
		t.Fatal(e)
	}
	c := domain.Claim{ID: "c", TenantID: "t", OrgID: "org", Version: 2}
	raw, _ := json.Marshal(c)
	db.Create(&store.Claim{ID: "c", TenantID: "t", OrgID: "org", ContentJSON: string(raw), Version: 2})
	db.Create(&store.Outbox{ID: "o", EventID: "o", TenantID: "t", ResourceID: "c", Topic: "project", PayloadJSON: `{"immutable":"original"}`, Status: "FAILED", Version: 7, Attempt: 12})
	actor := domain.Actor{TenantID: "t", SubjectID: "ops", Permissions: []string{"expense.integration.manage", "expense.scope.tenant_all"}}
	input := RecoveryInput{CommandInput: CommandInput{CommandID: "retry", ExpectedVersion: 7}, Reason: "依赖恢复"}
	first, e := s.RecoverIntegration(context.Background(), actor, "outbox", "o", input)
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.RecoverIntegration(context.Background(), actor, "outbox", "o", input)
	if e != nil || string(first) != string(second) {
		t.Fatalf("replay %s %v", second, e)
	}
	var row store.Outbox
	db.First(&row, "id = ?", "o")
	if row.PayloadJSON != `{"immutable":"original"}` || row.Status != "PENDING" || row.Version != 8 || row.Attempt != 0 {
		t.Fatalf("changed authority %+v", row)
	}
	actor.TenantID = "other"
	if _, e = s.RecoverIntegration(context.Background(), actor, "outbox", "o", input); e == nil {
		t.Fatal("cross tenant accepted")
	}
	actor.TenantID = "t"
	actor.Permissions = []string{"expense.scope.tenant_all"}
	if _, e = s.RecoverIntegration(context.Background(), actor, "outbox", "o", input); e == nil {
		t.Fatal("revoked permission replayed")
	}
	input.Reason = "modified"
	actor.Permissions = append(actor.Permissions, "expense.integration.manage")
	if _, e = s.RecoverIntegration(context.Background(), actor, "outbox", "o", input); e == nil {
		t.Fatal("mutated replay accepted")
	}
}

// TestScopedRecoveryIncludesParseAndArchive 验证组织范围死信包含解析及档案资源并隐藏其他组织。
// 参数：t 为测试上下文。
// 返回值：无。
func TestScopedRecoveryIncludesParseAndArchive(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	db.AutoMigrate(RecoveryModels()...)
	db.Create(&store.Config{ID: "scope", TenantID: "t", Kind: "scopes", Published: true, ContentJSON: `{"subject_id":"ops","org_ids":["org"]}`})
	db.Create(&store.Config{ID: "employee", TenantID: "t", Kind: "employee_profiles", Published: true, ContentJSON: `{"subject_id":"app","org_id":"org","active":true}`})
	raw, _ := encode(domain.Claim{ID: "c", TenantID: "t", OrgID: "org"})
	db.Create(&store.Claim{ID: "c", TenantID: "t", OrgID: "org", ContentJSON: raw})
	db.Create(&store.Archive{ID: "archive", TenantID: "t", ClaimID: "c", ManifestJSON: "{}"})
	for _, row := range []store.Job{{ID: "parse-dead", TenantID: "t", ResourceID: "fixture-file", Kind: "INVOICE_PARSE", PayloadJSON: "{}", Status: "FAILED", Version: 2}, {ID: "archive-dead", TenantID: "t", ResourceID: "archive", Kind: "ARCHIVE", PayloadJSON: "{}", Status: "FAILED", Version: 2}, {ID: "other", TenantID: "other", ResourceID: "fixture-file", Status: "FAILED", Version: 2, PayloadJSON: "{}"}} {
		if e := db.Create(&row).Error; e != nil {
			t.Fatal(e)
		}
	}
	a := domain.Actor{TenantID: "t", SubjectID: "ops", Permissions: []string{"expense.integration.manage"}}
	rawJSON, e := s.OperationsQuery(context.Background(), a, "jobs", "", nil)
	if e != nil {
		t.Fatal(e)
	}
	var page struct{ Total int }
	json.Unmarshal(rawJSON, &page)
	if page.Total != 2 {
		t.Fatalf("dead jobs hidden %s", rawJSON)
	}
	for _, id := range []string{"parse-dead", "archive-dead"} {
		if _, e = s.RecoverIntegration(context.Background(), a, "job", id, RecoveryInput{CommandInput: CommandInput{CommandID: id, ExpectedVersion: 2}, Reason: "依赖恢复"}); e != nil {
			t.Fatal(e)
		}
	}
}

// TestApplicantReadsSyncWithoutRecoveryPermission 验证本人可读最小同步状态且不可重投。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApplicantReadsSyncWithoutRecoveryPermission(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	raw, _ := encode(domain.Claim{ID: "c", TenantID: "t", Applicant: "app", OrgID: "org"})
	db.Create(&store.Claim{ID: "c", TenantID: "t", Applicant: "app", OrgID: "org", ContentJSON: raw})
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	if _, e := s.OperationsQuery(context.Background(), a, "sync-status", "c", nil); e != nil {
		t.Fatal(e)
	}
}

// TestMissingRecoveryCommandKeepsNotFound 验证普通用户未知命令保持404且不伪装数据库故障。
// 参数：t 为测试上下文。
// 返回值：无。
func TestMissingRecoveryCommandKeepsNotFound(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	db.AutoMigrate(RecoveryModels()...)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read"}}
	_, e := s.QueryRecoveryCommand(context.Background(), a, "unknown")
	var p *domain.Problem
	if !errors.As(e, &p) || p.Code != "COMMAND_NOT_FOUND" || p.Status != 404 {
		t.Fatalf("unknown command changed semantics: %v", e)
	}
	if e = db.Migrator().DropTable(&store.RecoveryCommand{}); e != nil {
		t.Fatal(e)
	}
	_, e = s.QueryRecoveryCommand(context.Background(), a, "unknown")
	if e == nil || errors.As(e, &p) {
		t.Fatalf("database error hidden as business not found: %v", e)
	}
}
