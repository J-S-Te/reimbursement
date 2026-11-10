package mysql_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/bootstrap"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/migrations"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type mysqlProjects struct{ unavailable bool }

// Validate 模拟权威项目边界，数据库、命令和所有财务账本均使用真实 MySQL。
// 参数：上下文、主体、单据与版本及引用。
// 返回值：短有效引用或显式依赖故障。
func (p *mysqlProjects) Validate(_ context.Context, _ domain.Actor, _ string, _ int64, refs []domain.ReferenceRequest) ([]domain.ProjectReference, error) {
	if p.unavailable {
		return nil, domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "隔离故障", 503)
	}
	out := []domain.ProjectReference{}
	for _, ref := range refs {
		out = append(out, domain.ProjectReference{ProjectID: ref.ProjectID, ServiceItemID: ref.ServiceItemID, Eligible: true, ValidatedAt: time.Now().UTC(), ValidUntil: time.Now().Add(time.Minute), RequiredApproverRefs: []domain.ApproverRef{{SubjectID: "lead", NodeRole: "PROJECT_EXPENSE_OWNER", SourceField: "project_manager_id", SourceVersion: 1}}})
	}
	return out, nil
}

// Search 提供未参与本门禁的最小授权搜索响应。
// 参数：上下文、主体、关键字、游标和页大小。
// 返回值：空授权结果。
func (*mysqlProjects) Search(context.Context, domain.Actor, string, string, int) (json.RawMessage, error) {
	return json.RawMessage(`{"items":[]}`), nil
}

type mysqlPersonnel struct{}

// VerifyApprover 按真实并发夹具的精确主体和权限核验人员边界。
// 参数：上下文、当前申请人、目标和节点所需权限。
// 返回值：未配置的主体或权限返回拒绝。
func (mysqlPersonnel) VerifyApprover(_ context.Context, a domain.Actor, target, permission string) error {
	allowed := map[string]string{"lead": "expense.approval.act", "acct": "expense.finance.review"}
	if a.TenantID != "t" || allowed[target] != permission {
		return domain.Fail("APPROVER_INELIGIBLE", "测试目录未授权目标", 422)
	}
	return nil
}

type mysqlFiles struct{}

// Validate 将受控文件边界与真实 MySQL 并发门禁分开。
// 参数：上下文、当前身份、文件标识与用途。
// 返回值：夹具就绪。
func (mysqlFiles) Validate(context.Context, domain.Actor, []string, string) error { return nil }

// Download 提供本门禁未使用的下载边界。
// 参数：上下文、当前身份和文件标识。
// 返回值：空响应。
func (mysqlFiles) Download(context.Context, domain.Actor, string) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// mysqlFixture 装载两个报销单争用同一确认票据的最小政策与数据。
// 参数：t 为测试上下文，db 为已迁移的全新隔离库。
// 返回值：无。
func mysqlFixture(t *testing.T, db *gorm.DB) {
	t.Helper()
	policies := []struct {
		kind  string
		value any
	}{{"employee_profiles", domain.EmployeeProfile{ID: "profile", SubjectID: "app", OrgID: "org", Grade: "G", RouteType: "TECHNICAL", Active: true}}, {"standards", domain.Standard{ID: "standard", FeeType: "TRAVEL", OrgID: "org", Grade: "G", Limit: 200, Unit: "PER_OCCURRENCE", ValidFrom: "2026-01-01"}}, {"route_rules", domain.RouteRule{ID: "route", OrgID: "org", RouteType: "TECHNICAL", BaseRole: "PROJECT_EXPENSE_OWNER", ExtraRole: "TECHNICAL_DIRECTOR", AccountingRole: "ACCOUNTANT", ValidFrom: "2026-01-01"}}, {"finance_entities", domain.FinanceEntity{ID: "entity", Name: "测试", TaxID: "tax", OrgIDs: []string{"org"}, Active: true}}, {"payee_profiles", domain.PayeeProfile{ID: "payee", SubjectID: "app", AccountRef: "ref", Version: 1, Active: true, Reviewed: true}}, {"invoice_rules", domain.InvoiceRule{ID: "rule", InvoiceType: "VAT", IdentityFields: []string{"invoice_number"}, Active: true}}, {"assignments", domain.Assignment{ID: "accountant", SubjectID: "acct", OrgID: "org", Role: "ACCOUNTANT", ValidFrom: "2026-01-01"}}}
	for _, subject := range []string{"acct", "cashier-a", "cashier-b"} {
		policies = append(policies, struct {
			kind  string
			value any
		}{"scopes", domain.Scope{ID: "scope-" + subject, SubjectID: subject, OrgIDs: []string{"org"}}})
	}
	for _, policy := range policies {
		raw, e := json.Marshal(policy.value)
		if e != nil {
			t.Fatal(e)
		}
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		if e = db.Create(&store.Config{ID: fields["id"].(string), TenantID: "t", Kind: policy.kind, Version: 1, Published: true, ContentJSON: string(raw)}).Error; e != nil {
			t.Fatal(e)
		}
	}
	key := "invoice-identity"
	invoice := domain.Invoice{ID: "invoice", TenantID: "t", Owner: "app", Version: 1, InvoiceType: "VAT", IdentityKey: key, Total: 100, ReviewStatus: "CONFIRMED", VerificationStatus: "NOT_CHECKED", FileIDs: []string{"invoice-file"}}
	raw, _ := json.Marshal(invoice)
	if e := db.Create(&store.Invoice{ID: invoice.ID, TenantID: "t", Owner: "app", Version: 1, IdentityKey: &key, ReservationState: "AVAILABLE", ContentJSON: string(raw)}).Error; e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"claim-a", "claim-b"} {
		claim := domain.Claim{ID: id, TenantID: "t", Applicant: "app", Title: "最小并发门禁", OrgID: "org", RouteType: "TECHNICAL", Status: "DRAFT", Version: 1, Currency: "CNY", FinanceEntityID: "entity", PayeeProfileID: "payee", Items: []domain.Item{{ID: "item", FeeType: "TRAVEL", OccurredDate: "2026-11-10", Requested: 100, Units: 1, Allocations: []domain.Allocation{{ProjectID: "p", ServiceItemID: "s", Amount: 100}}, InvoiceUsages: []domain.InvoiceUsage{{InvoiceID: "invoice", Used: 100}}}}}
		raw, _ = json.Marshal(claim)
		if e := db.Create(&store.Claim{ID: id, TenantID: "t", Applicant: "app", OrgID: "org", Status: "DRAFT", Version: 1, ContentJSON: string(raw)}).Error; e != nil {
			t.Fatal(e)
		}
	}
}

// TestReimbursementMySQLMinimumGate 仅在全新本机隔离库验证迁移、同票竞争和双出纳唯一付款。
// 参数：t 为测试上下文；环境变量必须显式提供本机高端口专用空库 DSN。
// 返回值：无；无 DSN 时明确跳过，不寻找其他数据库。
func TestReimbursementMySQLMinimumGate(t *testing.T) {
	dsn := os.Getenv("REIMBURSEMENT_TEST_DSN")
	if dsn == "" {
		t.Skip("未提供明确隔离的REIMBURSEMENT_TEST_DSN")
	}
	config, e := driver.ParseDSN(dsn)
	if e != nil {
		t.Fatal("隔离DSN格式不合法")
	}
	host, portRaw, e := net.SplitHostPort(config.Addr)
	port, _ := strconv.Atoi(portRaw)
	if e != nil || config.Net != "tcp" || host != "127.0.0.1" || port < 20000 || !strings.HasPrefix(config.DBName, "exp_reimbursement_test_") {
		t.Fatal("拒绝非127.0.0.1高端口exp_reimbursement_test_专用库")
	}
	config.ParseTime = true
	config.MultiStatements = true
	db, e := gorm.Open(gormmysql.Open(config.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), NowFunc: func() time.Time { return time.Now().UTC() }})
	if e != nil {
		t.Fatal(e)
	}
	sqlDB, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(8)
	var tableCount int64
	if e = db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&tableCount).Error; e != nil {
		t.Fatal(e)
	}
	if tableCount != 0 {
		t.Fatal("隔离门禁只允许全新空库，不清理或复用现有数据")
	}
	ctx := context.Background()
	if e = bootstrap.Migrate(ctx, sqlDB, migrations.Files); e != nil {
		t.Fatal(e)
	}
	if e = bootstrap.Migrate(ctx, sqlDB, migrations.Files); e != nil {
		t.Fatal("迁移二次执行不幂等", e)
	}
	mysqlFixture(t, db)
	projects := &mysqlProjects{}
	service := application.New(db, application.Dependencies{Projects: projects, Files: mysqlFiles{}, Personnel: mysqlPersonnel{}, Environment: "test"})
	app := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read", "expense.claim.submit"}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]json.RawMessage, 2)
	failures := make([]error, 2)
	ids := []string{"claim-a", "claim-b"}
	for i := range ids {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			raw, _ := json.Marshal(map[string]any{"command_id": "submit-" + ids[index], "expected_version": 1})
			results[index], failures[index] = service.Execute(ctx, app, "claim.submit", ids[index], raw)
		}(i)
	}
	close(start)
	wg.Wait()
	winner := ""
	successes := 0
	for i, err := range failures {
		if err == nil {
			winner = ids[i]
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("同票两单必须恰好一个提交成功，成功数%d，失败%v", successes, failures)
	}
	var invoice store.Invoice
	if e = db.First(&invoice, "id = ?", "invoice").Error; e != nil {
		t.Fatal(e)
	}
	if invoice.ReservationClaimID != winner {
		t.Fatal("票据占用没有绑定唯一胜出单据")
	}
	var tasks []store.Task
	if e = db.Where("claim_id = ?", winner).Order("node").Find(&tasks).Error; e != nil {
		t.Fatal(e)
	}
	version := int64(2)
	for _, task := range tasks {
		if e = db.First(&task, "id = ?", task.ID).Error; e != nil {
			t.Fatal(e)
		}
		actor := domain.Actor{TenantID: "t", SubjectID: task.Assignee, Permissions: []string{"expense.claim.read", "expense.approval.act", "expense.finance.review"}}
		body, _ := json.Marshal(map[string]any{"command_id": "approve-" + task.ID, "expected_version": version, "task_version": task.Version, "round": 1, "action": "APPROVE"})
		if _, e = service.Execute(ctx, actor, "approval.action", task.ID, body); e != nil {
			t.Fatal(e)
		}
		version++
	}
	start = make(chan struct{})
	paymentBodies := make([]json.RawMessage, 2)
	actors := []domain.Actor{{TenantID: "t", SubjectID: "cashier-a", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record"}}, {TenantID: "t", SubjectID: "cashier-b", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record"}}}
	for i := range actors {
		paymentBodies[i], _ = json.Marshal(map[string]any{"command_id": "payment-" + actors[i].SubjectID, "expected_version": version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "txn-" + actors[i].SubjectID, "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}})
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			results[index], failures[index] = service.Execute(ctx, actors[index], "payment.record", winner, paymentBodies[index])
		}(i)
	}
	close(start)
	wg.Wait()
	successes = 0
	winnerIndex := 0
	for i, err := range failures {
		if err == nil {
			successes++
			winnerIndex = i
		}
	}
	if successes != 1 {
		t.Fatalf("双出纳必须恰好一个成功，成功数%d，失败%v", successes, failures)
	}
	for _, model := range []any{&store.Payment{}, &store.Effect{}, &store.Archive{}} {
		var count int64
		if e = db.Model(model).Count(&count).Error; e != nil {
			t.Fatal(e)
		}
		if count != 1 {
			t.Fatalf("唯一付款及关联事实数量不为1: %T=%d", model, count)
		}
	}
	projects.unavailable = true
	replay, e := service.Execute(ctx, actors[winnerIndex], "payment.record", winner, paymentBodies[winnerIndex])
	if e != nil {
		t.Fatal("已成功命令被后续远端故障阻断", e)
	}
	if string(replay) != string(results[winnerIndex]) {
		t.Fatal("成功重放没有返回冻结原结果")
	}
}
