package application

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type projectFixture struct {
	unavailable bool
	references  []domain.ProjectReference
}

// Validate 模拟已授权项目权威响应，保持真实业务服务和数据库。
// 参数：ctx 为上下文，actor 为身份，claim 为单据，version 为草稿版本，refs 为引用。
// 返回值：短期有效引用或依赖故障。
func (p *projectFixture) Validate(ctx context.Context, a domain.Actor, id string, v int64, refs []domain.ReferenceRequest) ([]domain.ProjectReference, error) {
	if p.unavailable {
		return nil, domain.Fail("PROJECT_REFERENCE_UNAVAILABLE", "项目不可用", 503)
	}
	if p.references != nil {
		out := append([]domain.ProjectReference{}, p.references...)
		for i := range out {
			out[i].ValidatedAt = time.Now().UTC()
			out[i].ValidUntil = time.Now().Add(time.Minute)
		}
		return out, nil
	}
	r := []domain.ProjectReference{}
	for _, x := range refs {
		lead := "lead"
		if x.ProjectID == "p2" {
			lead = "lead2"
		}
		field := "project_manager_id"
		if x.ServiceItemID == "" {
			field = "manager_identity_id"
		}
		r = append(r, domain.ProjectReference{ProjectID: x.ProjectID, ServiceItemID: x.ServiceItemID, Eligible: true, RequiredApproverRefs: []domain.ApproverRef{{SubjectID: lead, NodeRole: "PROJECT_EXPENSE_OWNER", SourceField: field, SourceVersion: 1}, {SubjectID: "technical-director", NodeRole: "TECHNICAL_DIRECTOR", SourceField: "role:technical_director", SourceDigest: strings.Repeat("a", 64), SourceOrgID: "technical-org"}, {SubjectID: "sales-director", NodeRole: "SALES_DIRECTOR", SourceField: "role:sales_director", SourceDigest: strings.Repeat("b", 64), SourceOrgID: x.ApprovalOrgID}}, ReferenceVersion: "1", ValidatedAt: time.Now().UTC(), ValidUntil: time.Now().Add(time.Minute)})
	}
	return r, nil
}

// Search 提供测试项目搜索边界。
// 参数：上下文、身份、关键字、游标和页大小。
// 返回值：空的授权搜索结果。
func (p *projectFixture) Search(context.Context, domain.Actor, string, string, int) (json.RawMessage, error) {
	return json.RawMessage(`{"items":[]}`), nil
}

type filesFixture struct{}

// Validate 验证受控测试文件已就绪。
// 参数：上下文、身份、文件标识和用途。
// 返回值：无错误。
func (filesFixture) Validate(context.Context, domain.Actor, []string, string) error { return nil }

// Download 返回测试下载结果。
// 参数：上下文、身份和文件标识。
// 返回值：空的网关响应。
func (filesFixture) Download(context.Context, domain.Actor, string) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// setupWorkflow 使用明确临时 SQLite 运行真实应用事务。
// 参数：t 为测试上下文。
// 返回值：服务、临时库和受控远端夹具。
func setupWorkflow(t *testing.T) (*Service, *gorm.DB, *projectFixture) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "expense.sqlite")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	// 统一模型入口已含票据控制表，使重大更正与红冲风险链路在真实事务内被覆盖。
	if err = db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, fileID := range []string{"file", "pdf-file", "ofd-file"} {
		if e := db.Create(&store.Document{ID: "fixture-" + fileID, TenantID: "t", Owner: "app", FileID: fileID, Status: "READY", Version: 1, ContentJSON: `{"purpose":"reimbursement.invoice.original","media_type":"application/pdf"}`}).Error; e != nil {
			t.Fatal(e)
		}
	}
	p := &projectFixture{}
	seedPolicy(t, db, "payment_accounts", domain.PaymentAccount{ID: "selection-source",
		Name: "公司日常付款", FinanceEntityID: "entity", AccountRef: "source", Active: true})
	return New(db, Dependencies{Projects: p, Files: filesFixture{}, Personnel: workflowPersonnel{}, Environment: "test"}), db, p
}

// runCommand 在测试中执行真实命令并解析冻结响应。
// 参数：t 为测试上下文，s 为服务，a 为身份，op 为动作，id 为对象，body 为请求。
// 返回值：响应 JSON 字段。
func runCommand(t *testing.T, s *Service, a domain.Actor, op, id string, body any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	r, err := s.Execute(context.Background(), a, op, id, raw)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	var out map[string]any
	if err = json.Unmarshal(r, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDraftPersistsAndEnforcesTenant 防止草稿仅留前端或跨租户访问。
// 参数：t 为测试上下文。
// 返回值：无。
func TestDraftPersistsAndEnforcesTenant(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "employee_profiles", domain.EmployeeProfile{ID: "test-employee", SubjectID: "app", OrgID: "org", Grade: "G1", RouteType: "TECHNICAL", Active: true})
	a := domain.Actor{TenantID: "t", SubjectID: "app", OrgIDs: []string{"org"}, Permissions: []string{"expense.claim.create", "expense.claim.read"}}
	r := runCommand(t, s, a, "claim.create", "", map[string]any{"command_id": "draft-1", "title": "出差", "org_id": "org", "route_type": "TECHNICAL", "finance_entity_id": "f", "payee_profile_id": "bank", "currency": "CNY"})
	if r["id"] == nil || r["version"] != float64(1) {
		t.Fatalf("没有持久草稿和版本: %s", r)
	}
	var n int64
	db.Model(&store.Claim{}).Where("tenant_id = ? AND applicant = ?", "t", "app").Count(&n)
	if n != 1 {
		t.Fatal("草稿没有事务持久化")
	}
}

// seedPolicy 只向测试隔离库装载已发布政策夹具。
// 参数：t/db 为临时库，kind 为配置种类，value 为手工业务夹具。
// 返回值：无。
func seedPolicy(t *testing.T, db *gorm.DB, kind string, value any) {
	t.Helper()
	raw, _ := json.Marshal(value)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	id, _ := fields["id"].(string)
	if err := db.Create(&store.Config{ID: id, TenantID: "t", Kind: kind, Version: 1, Published: true, ContentJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
}

// TestPaymentReversalReplayKeepsRepaymentSlot 验证两负责人ALL、独立付款和旧冲正重放不清空重付。
// 参数：t 为测试上下文。
// 返回值：无。
func TestPaymentReversalReplayKeepsRepaymentSlot(t *testing.T) {
	s, db, projects := setupWorkflow(t)
	app := domain.Actor{TenantID: "t", SubjectID: "app", OrgIDs: []string{"org"}, Permissions: []string{"expense.claim.create", "expense.claim.update", "expense.claim.read", "expense.claim.submit", "expense.invoice.upload", "expense.invoice.read", "expense.invoice.review"}}
	seedPolicy(t, db, "route_rules", domain.RouteRule{ID: "route", OrgID: "org", RouteType: "TECHNICAL", ValidFrom: "2026-01-01", BaseRole: "PROJECT_EXPENSE_OWNER", ExtraRole: "TECHNICAL_DIRECTOR", AccountingRole: "ACCOUNTANT", Version: 1, Published: true})
	seedPolicy(t, db, "employee_profiles", domain.EmployeeProfile{ID: "employee", SubjectID: "app", OrgID: "org", Grade: "G1", RouteType: "TECHNICAL", Version: 1, Active: true})
	seedPolicy(t, db, "standards", domain.Standard{ID: "standard", OrgID: "org", FeeType: "TRAVEL", Grade: "G1", Unit: "PER_OCCURRENCE", Limit: 10000, ValidFrom: "2026-01-01", Version: 1, Published: true})
	seedPolicy(t, db, "finance_entities", domain.FinanceEntity{ID: "entity", Name: "测试主体", TaxID: "tax", OrgIDs: []string{"org"}, Active: true})
	seedPolicy(t, db, "payee_profiles", domain.PayeeProfile{ID: "payee", SubjectID: "app", AccountRef: "bank-ref", Version: 1, Active: true, Reviewed: true})
	seedPolicy(t, db, "assignments", domain.Assignment{ID: "accountant-map", OrgID: "org", Role: "ACCOUNTANT", SubjectID: "accountant", ValidFrom: "2026-01-01", Published: true})
	seedPolicy(t, db, "invoice_rules", domain.InvoiceRule{ID: "vat-rule", InvoiceType: "VAT", IdentityFields: []string{"invoice_type", "invoice_number"}, RequiredBuyerTaxID: true, Active: true, Version: 1})
	for _, subject := range []string{"accountant", "cashier", "director"} {
		seedPolicy(t, db, "scopes", domain.Scope{ID: "scope-" + subject, SubjectID: subject, OrgIDs: []string{"org"}, Published: true, Version: 1})
	}
	invoiceBody := map[string]any{"command_id": "invoice-1", "invoice_type": "VAT", "invoice_number": "123456", "invoice_date": "2026-11-10", "seller_tax_id": "seller", "buyer_tax_id": "tax", "total_minor": "101", "file_ids": []string{"file"}}
	invoice := runCommand(t, s, app, "invoice.create", "", invoiceBody)
	invoiceID := invoice["id"].(string)
	invoiceBody["command_id"] = "confirm-1"
	invoiceBody["expected_version"] = 1
	invoiceBody["reason"] = "人工核对原件"
	runCommand(t, s, app, "invoice.confirm", invoiceID, invoiceBody)
	claim := runCommand(t, s, app, "claim.create", "", map[string]any{"command_id": "claim-1", "title": "双团队差旅", "org_id": "org", "route_type": "TECHNICAL", "finance_entity_id": "entity", "payee_profile_id": "payee", "items": []domain.Item{{ID: "item", FeeType: "TRAVEL", OccurredDate: "2026-11-10", Requested: 101, Units: 1, Allocations: []domain.Allocation{{ProjectID: "p1", ServiceItemID: "s1", Amount: 60}, {ProjectID: "p2", ServiceItemID: "s2", Amount: 41}}, InvoiceUsages: []domain.InvoiceUsage{{InvoiceID: invoiceID, Used: 101}}}}})
	claimID := claim["id"].(string)
	submitted := runCommand(t, s, app, "claim.submit", claimID, map[string]any{"command_id": "submit-1", "expected_version": 1})
	if submitted["payable_minor"] != "101" {
		t.Fatal("总额应为101分")
	}
	var tasks []store.Task
	if err := db.Where("claim_id = ?", claimID).Order("node,assignee").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Fatalf("两个团队负责人必须生成两个ALL任务+会计任务，实际%d", len(tasks))
	}
	version := int64(2)
	for _, task := range tasks {
		a := domain.Actor{TenantID: "t", SubjectID: task.Assignee, Permissions: []string{"expense.claim.read", "expense.approval.act", "expense.finance.review"}}
		if err := db.First(&task, "id = ?", task.ID).Error; err != nil {
			t.Fatal(err)
		}
		r := runCommand(t, s, a, "approval.action", task.ID, map[string]any{"command_id": "approve-" + task.ID, "expected_version": version, "task_version": task.Version, "round": 1, "action": "APPROVE"})
		version++
		if task.Node == 1 && r["status"] == "APPROVED_PENDING_PAYMENT" {
			t.Fatal("会计尚未批准不能待付款")
		}
	}
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record", "expense.payment.reversal.request"}}
	body := map[string]any{"command_id": "pay-1", "expected_version": version, "amount_minor": "101", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "txn1", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}}
	paid := runCommand(t, s, cashier, "payment.record", claimID, body)
	paymentID := paid["id"].(string)
	version++
	projects.unavailable = true
	replayed := runCommand(t, s, cashier, "payment.record", claimID, body)
	if replayed["id"] != paymentID {
		t.Fatal("依赖失联时重放必须返回原付款")
	}
	projects.unavailable = false
	request := runCommand(t, s, cashier, "reversal.request", paymentID, map[string]any{"command_id": "reverse-request", "expected_version": version, "reason": "已全额退回", "fact_kind": "FULL_REFUND", "evidence_file_ids": []string{"refund"}})
	director := domain.Actor{TenantID: "t", SubjectID: "director", Permissions: []string{"expense.claim.read", "expense.archive.read", "expense.payment.reversal.approve"}}
	decision := map[string]any{"command_id": "reverse-approve", "expected_version": 1, "approve": true, "reason": "核实银行退款证据"}
	runCommand(t, s, director, "reversal.decide", request["id"].(string), decision)
	version++
	body["command_id"] = "pay-2"
	body["expected_version"] = version
	body["transaction_no"] = "txn2"
	repaid := runCommand(t, s, cashier, "payment.record", claimID, body)
	runCommand(t, s, director, "reversal.decide", request["id"].(string), decision)
	var notices []store.Outbox
	if err := db.Where("topic = ? AND resource_id = ?", "notification", claimID).Find(&notices).Error; err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, item := range notices {
		var payload map[string]any
		if err := json.Unmarshal([]byte(item.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["kind"] == "REVERSAL_APPROVED" {
			n++
			if payload["assignee_subject_id"] != cashier.SubjectID || payload["task_id"] != nil {
				t.Fatalf("冲正结果收件人或类型错误: %+v", payload)
			}
		}
	}
	if n != 1 {
		t.Fatalf("冲正结果通知数量=%d，预期1", n)
	}
	c, _, err := loadClaim(db, "t", claimID, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.ActivePaymentID != repaid["id"] || c.Status != "PAID" {
		t.Fatal("重试旧冲正清掉了新的有效付款槽位")
	}
	var effects int64
	db.Model(&store.Effect{}).Count(&effects)
	if effects != 3 {
		t.Fatalf("付款/冲正/重付应恰好3个效果，实际%d", effects)
	}
	var outbox []store.Outbox
	db.Where("topic = ?", "project").Find(&outbox)
	if len(outbox) != 3 {
		t.Fatalf("三个财务效果应有三版完整快照，实际%d", len(outbox))
	}
	for _, row := range outbox {
		var snapshot domain.SpendSnapshot
		if err = json.Unmarshal([]byte(row.PayloadJSON), &snapshot); err != nil {
			t.Fatal(err)
		}
		if err = domain.ValidateSpendSnapshot(snapshot); err != nil {
			t.Fatal(err)
		}
	}
}

type rejectingFiles struct{ filesFixture }

// Validate 拒绝不属于当前操作者的网关文件。
// 参数：上下文、可信身份、文件标识和用途。
// 返回值：归属错误。
func (rejectingFiles) Validate(context.Context, domain.Actor, []string, string) error {
	return domain.Fail("FILE_FORBIDDEN", "文件不属于本人", 403)
}

// TestInvoiceCreationRejectsForeignFile 防止直接提交他人file_id绕过上传归属检查。
// 参数：t 为测试上下文。
// 返回值：无。
func TestInvoiceCreationRejectsForeignFile(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	s.deps.Files = rejectingFiles{}
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload"}}
	raw := json.RawMessage(`{"command_id":"foreign-file","file_ids":["foreign"]}`)
	if _, e := s.Execute(context.Background(), a, "invoice.create", "", raw); e == nil {
		t.Fatal("不属于本人的file_id被接受")
	}
	var n int64
	db.Model(&store.Invoice{}).Count(&n)
	if n != 0 {
		t.Fatal("文件归属失败仍创建了票据")
	}
}

// TestDraftCrossTenantAndCommandConflict 防止跨租户读取与同命令异内容覆盖。
// 参数：t 为测试上下文。
// 返回值：无。
func TestDraftCrossTenantAndCommandConflict(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "employee_profiles", domain.EmployeeProfile{ID: "test-employee", SubjectID: "app", OrgID: "org", Grade: "G1", RouteType: "TECHNICAL", Active: true})
	a := domain.Actor{TenantID: "t", SubjectID: "app", OrgIDs: []string{"org"}, Permissions: []string{"expense.claim.create", "expense.claim.read"}}
	body := map[string]any{"command_id": "draft-conflict", "org_id": "org", "title": "原内容"}
	r := runCommand(t, s, a, "claim.create", "", body)
	body["title"] = "篡改内容"
	raw, _ := json.Marshal(body)
	if _, e := s.Execute(context.Background(), a, "claim.create", "", raw); e == nil {
		t.Fatal("同命令不同内容未阻断")
	}
	a.TenantID = "other"
	if _, e := s.Query(context.Background(), a, "claim", r["id"].(string), nil); e == nil {
		t.Fatal("跨租户读取未阻断")
	}
}

// TestSameBusinessInvoiceMergesPDFAndOFD 防止同一张票不同格式丢失原件来源。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSameBusinessInvoiceMergesPDFAndOFD(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload", "expense.invoice.review", "expense.invoice.read"}}
	seedPolicy(t, db, "invoice_rules", domain.InvoiceRule{ID: "rule", InvoiceType: "VAT", IdentityFields: []string{"invoice_type", "invoice_number"}, Active: true})
	var firstID string
	for i, file := range []string{"pdf-file", "ofd-file"} {
		body := map[string]any{"command_id": "source-create-" + file, "invoice_type": "VAT", "invoice_number": "n1", "invoice_date": "2026-11-10", "seller_tax_id": "s", "total_minor": "100", "file_ids": []string{file}}
		candidate := runCommand(t, s, a, "invoice.create", "", body)
		body["command_id"] = "source-confirm-" + file
		body["expected_version"] = 1
		body["reason"] = "人工核对同票原件"
		confirmed := runCommand(t, s, a, "invoice.confirm", candidate["id"].(string), body)
		if i == 0 {
			firstID = confirmed["id"].(string)
		} else {
			if confirmed["id"] != firstID {
				t.Fatal("PDF/OFD未归并到同一业务发票")
			}
			files := confirmed["file_ids"].([]any)
			if len(files) != 2 {
				t.Fatal("归并后丢失来源")
			}
		}
	}
}

// submissionFixture 建立含已确认票据的最小提交夹具，所有数据仅位于临时库。
// 参数：t/db 为隔离测试上下文，accountant 指示是否配置最后审批节点。
// 返回值：提交身份与单据标识。
func submissionFixture(t *testing.T, db *gorm.DB, accountant bool) (domain.Actor, string) {
	t.Helper()
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.read", "expense.claim.submit"}}
	seedPolicy(t, db, "employee_profiles", domain.EmployeeProfile{ID: "employee", SubjectID: "app", OrgID: "org", Grade: "G", RouteType: "TECHNICAL", Active: true})
	seedPolicy(t, db, "standards", domain.Standard{ID: "standard", FeeType: "TRAVEL", OrgID: "org", Grade: "G", Unit: "PER_OCCURRENCE", Limit: 200, ValidFrom: "2026-01-01"})
	seedPolicy(t, db, "route_rules", domain.RouteRule{ID: "route", OrgID: "org", RouteType: "TECHNICAL", BaseRole: "PROJECT_EXPENSE_OWNER", ExtraRole: "TECHNICAL_DIRECTOR", AccountingRole: "ACCOUNTANT", ValidFrom: "2026-01-01"})
	seedPolicy(t, db, "finance_entities", domain.FinanceEntity{ID: "entity", Name: "测试", TaxID: "tax", OrgIDs: []string{"org"}, Active: true})
	seedPolicy(t, db, "payee_profiles", domain.PayeeProfile{ID: "payee", SubjectID: "app", AccountRef: "ref", Version: 1, Active: true, Reviewed: true})
	seedPolicy(t, db, "invoice_rules", domain.InvoiceRule{ID: "rule", InvoiceType: "VAT", IdentityFields: []string{"invoice_number"}, Active: true})
	if accountant {
		seedPolicy(t, db, "scopes", domain.Scope{ID: "fixture-acct-scope", SubjectID: "acct", OrgIDs: []string{"org"}})
		seedPolicy(t, db, "assignments", domain.Assignment{ID: "accountant", OrgID: "org", Role: "ACCOUNTANT", SubjectID: "acct", ValidFrom: "2026-01-01"})
	}
	identity := "key"
	inv := domain.Invoice{ID: "invoice", Owner: "app", TenantID: "t", Version: 1, IdentityKey: identity, InvoiceType: "VAT", Total: 100, ReviewStatus: "CONFIRMED", VerificationStatus: "NOT_CHECKED", FileIDs: []string{"file"}}
	raw, _ := json.Marshal(inv)
	if e := db.Create(&store.Invoice{ID: inv.ID, TenantID: "t", Owner: "app", IdentityKey: &identity, Version: 1, ReservationState: "AVAILABLE", ContentJSON: string(raw)}).Error; e != nil {
		t.Fatal(e)
	}
	c := domain.Claim{ID: "claim", TenantID: "t", Applicant: "app", Title: "测试", OrgID: "org", RouteType: "TECHNICAL", Status: "DRAFT", Version: 1, Currency: "CNY", FinanceEntityID: "entity", PayeeProfileID: "payee", Items: []domain.Item{{ID: "item", FeeType: "TRAVEL", OccurredDate: "2026-11-10", Requested: 100, Units: 1, Allocations: []domain.Allocation{{ProjectID: "p1", ServiceItemID: "s", Amount: 100}}, InvoiceUsages: []domain.InvoiceUsage{{InvoiceID: "invoice", Used: 100}}}}}
	raw, _ = json.Marshal(c)
	if e := db.Create(&store.Claim{ID: c.ID, TenantID: "t", Applicant: "app", OrgID: "org", Status: c.Status, Version: 1, ContentJSON: string(raw)}).Error; e != nil {
		t.Fatal(e)
	}
	return a, c.ID
}

// TestSubmissionRollbackDoesNotReserveInvoice 验证占票后负责人缺失回滚全部本地事实。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSubmissionRollbackDoesNotReserveInvoice(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a, id := submissionFixture(t, db, false)
	_, e := s.Execute(context.Background(), a, "claim.submit", id, json.RawMessage(`{"command_id":"missing-approver","expected_version":1}`))
	if e == nil {
		t.Fatal("缺会计负责人必须失败")
	}
	var inv store.Invoice
	if e = db.First(&inv, "id = ?", "invoice").Error; e != nil {
		t.Fatal(e)
	}
	if inv.ReservationState != "AVAILABLE" || inv.ReservationClaimID != "" {
		t.Fatal("事务失败留下票据占用")
	}
	var snapshots, outbox int64
	db.Model(&store.Snapshot{}).Count(&snapshots)
	db.Model(&store.Outbox{}).Count(&outbox)
	if snapshots != 0 || outbox != 0 {
		t.Fatal("失败提交留下快照或outbox")
	}
}

// TestAuditFailureRollsBackBusiness 验证审计落库失败不会产生无审计成功业务。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAuditFailureRollsBackBusiness(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	if e := db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON exp_outbox WHEN NEW.topic = 'audit' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; e != nil {
		t.Fatal(e)
	}
	if _, e := s.Execute(context.Background(), a, "claim.submit", id, json.RawMessage(`{"command_id":"audit-fail","expected_version":1}`)); e == nil {
		t.Fatal("审计写失败仍报告成功")
	}
	c, _, e := loadClaim(db, "t", id, false)
	if e != nil {
		t.Fatal(e)
	}
	if c.Status != "DRAFT" || c.Version != 1 {
		t.Fatal("审计失败没有回滚业务")
	}
	var inv store.Invoice
	db.First(&inv, "id = ?", "invoice")
	if inv.ReservationState != "AVAILABLE" {
		t.Fatal("审计失败没有回滚占票")
	}
	var tasks, commands int64
	db.Model(&store.Task{}).Count(&tasks)
	db.Model(&store.Command{}).Count(&commands)
	if tasks != 0 || commands != 0 {
		t.Fatal("审计失败留下任务或成功命令")
	}
}

// TestSharedInvoiceCannotBeReservedBySecondClaim 验证同票不能跨报销单拆分占用。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSharedInvoiceCannotBeReservedBySecondClaim(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	c, _, e := loadClaim(db, "t", id, false)
	if e != nil {
		t.Fatal(e)
	}
	c.ID = "claim2"
	raw, _ := json.Marshal(c)
	if e = db.Create(&store.Claim{ID: c.ID, TenantID: "t", Applicant: "app", OrgID: "org", Status: "DRAFT", Version: 1, ContentJSON: string(raw)}).Error; e != nil {
		t.Fatal(e)
	}
	runCommand(t, s, a, "claim.submit", id, map[string]any{"command_id": "first", "expected_version": 1})
	if _, e = s.Execute(context.Background(), a, "claim.submit", c.ID, json.RawMessage(`{"command_id":"second","expected_version":1}`)); e == nil {
		t.Fatal("同票跨两单占用被放行")
	}
	var inv store.Invoice
	db.First(&inv, "id = ?", "invoice")
	if inv.ReservationClaimID != id {
		t.Fatal("后提交覆盖原单票据占用")
	}
}

// TestApprovalSnapshotDetectsAllocationTampering 验证归档可复算固定内容并拒绝分摊篡改。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalSnapshotDetectsAllocationTampering(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	runCommand(t, s, a, "claim.submit", id, map[string]any{"command_id": "snapshot-submit", "expected_version": 1})
	var row store.Snapshot
	if e := db.First(&row, "claim_id = ?", id).Error; e != nil {
		t.Fatal(e)
	}
	if e := VerifyApprovalSnapshot(json.RawMessage(row.ContentJSON), row.ContentHash); e != nil {
		t.Fatal(e)
	}
	var content approvalSnapshot
	if e := json.Unmarshal([]byte(row.ContentJSON), &content); e != nil {
		t.Fatal(e)
	}
	content.Claim.Items[0].Allocations[0].Amount = 99
	changed, _ := json.Marshal(content)
	if VerifyApprovalSnapshot(changed, row.ContentHash) == nil {
		t.Fatal("冻结快照分摊篡改未被摘要发现")
	}
}

type workflowPersonnel struct{}

// CurrentApplicant 提供隔离业务夹具明确声明的当前本人任职，不读取真实平台。
// 参数：ctx 为测试边界；actor 为夹具当前用户。
// 返回值：本人在测试组织的当前任职与名称。
func (workflowPersonnel) CurrentApplicant(ctx context.Context, actor domain.Actor) (domain.PersonOption, error) {
	return domain.PersonOption{ID: actor.SubjectID, Name: "测试申请人", Organizations: []domain.OrganizationOption{
		{ID: "org", Name: "测试部门", IsPrimary: true},
		{ID: "sales-org", Name: "测试销售部"},
	}}, nil
}

// VerifyApprover 仅允许夹具明确声明的主体和各自精确节点能力。
// 参数：上下文、可信申请人、目标受理人及节点权限。
// 返回值：夹具未声明的主体或权限拒绝。
func (workflowPersonnel) VerifyApprover(_ context.Context, a domain.Actor, target, permission string) error {
	expected := map[string]string{"lead": "expense.approval.act", "lead2": "expense.approval.act", "lead3": "expense.approval.act", "acct": "expense.finance.review", "accountant": "expense.finance.review", "director": "expense.approval.act", "technical-director": "expense.approval.act", "finance-director": "expense.approval.act", "sales-director": "expense.approval.act"}
	if a.TenantID != "t" || expected[target] != permission {
		return domain.Fail("APPROVER_INELIGIBLE", "测试目录未授予该主体精确节点能力", 422)
	}
	return nil
}
