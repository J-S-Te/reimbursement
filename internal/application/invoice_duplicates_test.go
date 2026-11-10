package application

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// duplicateInvoice 构造具有明确不同业务身份的同日同销售方票据。
// 参数：id为票据及业务身份后缀。
// 返回值：可在临时库落盘的已确认票据。
func duplicateInvoice(id string) domain.Invoice {
	return domain.Invoice{ID: id, TenantID: "t", Owner: "app", Version: 1,
		IdentityKey: "key-" + id, InvoiceNumber: "number-" + id, InvoiceDate: "2026-11-10",
		SellerTaxID: "seller", Total: 100, ReviewStatus: "CONFIRMED", ReservationState: "AVAILABLE"}
}

// seedDuplicateInvoice 向独立临时SQLite写入票面及其正式归属索引。
// 参数：t/db为测试上下文，invoice为需要测试的明确票面事实。
// 返回值：无。
func seedDuplicateInvoice(t *testing.T, db *gorm.DB, invoice domain.Invoice) {
	t.Helper()
	raw, err := json.Marshal(invoice)
	if err != nil {
		t.Fatal(err)
	}
	var key *string
	if invoice.IdentityKey != "" {
		key = &invoice.IdentityKey
	}
	if err = db.Create(&store.Invoice{ID: invoice.ID, TenantID: invoice.TenantID,
		Owner: invoice.Owner, Version: invoice.Version, IdentityKey: key,
		ReservationState: invoice.ReservationState, ReservationClaimID: invoice.ReservationClaimID,
		ContentJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
}

// duplicateDetail 通过真实详情Query读取疑似提示，不绕过正式票据授权。
// 参数：t/s为测试服务，actor为当前权限，id为请求票据。
// 返回值：详情中的候选及是否还有已授权匹配。
func duplicateDetail(t *testing.T, s *Service, actor domain.Actor, id string) ([]any, bool) {
	t.Helper()
	raw, err := s.Query(context.Background(), actor, "invoice", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err = json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	duplicates, ok := view["suspected_duplicates"].([]any)
	if !ok {
		t.Fatalf("单票详情缺少明确的疑似重复候选数组: %s", raw)
	}
	more, ok := view["suspected_duplicates_more"].(bool)
	if !ok {
		t.Fatalf("单票详情缺少授权范围内的更多标志: %s", raw)
	}
	return duplicates, more
}

// TestInvoiceDuplicatesMatchVisibleBusinessFacts 防止不同票号但同日同金额同销售方的风险遗漏。
// 参数：t为测试上下文。
// 返回值：无。
func TestInvoiceDuplicatesMatchVisibleBusinessFacts(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedDuplicateInvoice(t, db, duplicateInvoice("source"))
	seedDuplicateInvoice(t, db, duplicateInvoice("candidate"))
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read"}}
	items, more := duplicateDetail(t, s, actor, "source")
	if len(items) != 1 || more {
		t.Fatalf("应只提示一张可见疑似票: %+v more=%t", items, more)
	}
	item := items[0].(map[string]any)
	if item["invoice_id"] != "candidate" || item["invoice_number"] != "number-candidate" ||
		item["invoice_date"] != "2026-11-10" || item["total_minor"] != "100" {
		t.Fatalf("候选应保留授权票面坐标与精确金额: %+v", item)
	}
	if len(item) != 4 {
		t.Fatalf("风险候选不应额外公开客户税号、人员或原件: %+v", item)
	}
}

// TestInvoiceDuplicatesExcludeNonMatchingAndRetiredInvoices 防止不相同或已合并作废票据误报。
// 参数：t为测试上下文。
// 返回值：无。
func TestInvoiceDuplicatesExcludeNonMatchingAndRetiredInvoices(t *testing.T) {
	for _, name := range []string{"seller", "date", "amount", "merged", "void", "unknown-seller", "unknown-identity"} {
		t.Run(name, func(t *testing.T) {
			s, db, _ := setupWorkflow(t)
			source, candidate := duplicateInvoice("source"), duplicateInvoice("candidate")
			switch name {
			case "seller":
				candidate.SellerTaxID = "different-seller"
			case "date":
				candidate.InvoiceDate = "2026-11-09"
			case "amount":
				candidate.Total = 101
			case "merged":
				candidate.MergedIntoInvoiceID = "canonical"
			case "void":
				candidate.ReservationState = "VOID"
			case "unknown-seller":
				source.SellerTaxID, candidate.SellerTaxID = "", ""
			case "unknown-identity":
				candidate.IdentityKey = ""
			}
			seedDuplicateInvoice(t, db, source)
			seedDuplicateInvoice(t, db, candidate)
			actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read"}}
			items, more := duplicateDetail(t, s, actor, "source")
			if len(items) != 0 || more {
				t.Fatalf("不满足完整同票面条件不应猜测: %+v more=%t", items, more)
			}
		})
	}
}

// TestInvoiceDuplicatesNeverCountHiddenOwnersOrTenants 防止候选数量或更多标记泄漏无权票据。
// 参数：t为测试上下文。
// 返回值：无。
func TestInvoiceDuplicatesNeverCountHiddenOwnersOrTenants(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedDuplicateInvoice(t, db, duplicateInvoice("source"))
	for i := 0; i < 8; i++ {
		candidate := duplicateInvoice(fmt.Sprintf("hidden-%d", i))
		candidate.Owner = "another-owner"
		seedDuplicateInvoice(t, db, candidate)
		candidate.ID, candidate.TenantID, candidate.Owner = fmt.Sprintf("foreign-%d", i), "foreign", "app"
		seedDuplicateInvoice(t, db, candidate)
	}
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read"}}
	items, more := duplicateDetail(t, s, actor, "source")
	if len(items) != 0 || more {
		t.Fatalf("不可读候选不得通过提示或计数泄漏: %+v more=%t", items, more)
	}
}

// TestInvoiceDuplicatesBoundVisibleResults 防止大量疑似匹配导致详情无限返回。
// 参数：t为测试上下文。
// 返回值：无。
func TestInvoiceDuplicatesBoundVisibleResults(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedDuplicateInvoice(t, db, duplicateInvoice("source"))
	for i := 0; i < 6; i++ {
		seedDuplicateInvoice(t, db, duplicateInvoice(fmt.Sprintf("candidate-%d", i)))
	}
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read"}}
	items, more := duplicateDetail(t, s, actor, "source")
	if len(items) != 5 || !more {
		t.Fatalf("六条已授权匹配应最多展示五条并标更多: %d %t", len(items), more)
	}
}

// TestInvoiceDuplicatesReuseFinancialClaimScope 防止财务提示越过既有整单范围或漏掉授权关联票据。
// 参数：t为测试上下文。
// 返回值：无。
func TestInvoiceDuplicatesReuseFinancialClaimScope(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	for _, org := range []string{"allowed-org", "hidden-org"} {
		claim := domain.Claim{ID: org, TenantID: "t", OrgID: org, Applicant: "employee", Status: "DRAFT", Version: 1}
		raw, _ := json.Marshal(claim)
		if err := db.Create(&store.Claim{ID: org, TenantID: "t", OrgID: org, Applicant: "employee",
			Status: "DRAFT", Version: 1, ContentJSON: string(raw)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	seedPolicy(t, db, "scopes", domain.Scope{ID: "duplicate-finance-scope", SubjectID: "acct",
		OrgIDs: []string{"allowed-org"}})
	source, visible := duplicateInvoice("source"), duplicateInvoice("visible")
	source.Owner, visible.Owner = "employee", "another-employee"
	source.ReservationClaimID, visible.ReservationClaimID = "allowed-org", "allowed-org"
	seedDuplicateInvoice(t, db, source)
	seedDuplicateInvoice(t, db, visible)
	for i := 0; i < 6; i++ {
		hidden := duplicateInvoice(fmt.Sprintf("hidden-%d", i))
		hidden.Owner, hidden.ReservationClaimID = "hidden-employee", "hidden-org"
		seedDuplicateInvoice(t, db, hidden)
	}
	actor := domain.Actor{TenantID: "t", SubjectID: "acct", Permissions: []string{
		"expense.invoice.read", "expense.finance.review", "expense.claim.read",
	}}
	items, more := duplicateDetail(t, s, actor, "source")
	if len(items) != 1 || more || items[0].(map[string]any)["invoice_id"] != "visible" {
		t.Fatalf("财务提示必须与原整单授权一致: %+v more=%t", items, more)
	}
	if _, err := s.Query(context.Background(), actor, "invoice", "hidden-0", nil); err == nil {
		t.Fatal("风险提示接线扩大了原详情读取范围")
	}
}

// TestInvoiceDuplicatesPreserveExplicitTenantWideScope 防止明确全租户权限漏掉未占用票但仍跨租户泄漏。
// 参数：t为测试上下文。
// 返回值：无。
func TestInvoiceDuplicatesPreserveExplicitTenantWideScope(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedDuplicateInvoice(t, db, duplicateInvoice("source"))
	visible, foreign := duplicateInvoice("visible"), duplicateInvoice("foreign")
	visible.Owner = "other-owner"
	foreign.Owner, foreign.TenantID = "other-owner", "foreign"
	seedDuplicateInvoice(t, db, visible)
	seedDuplicateInvoice(t, db, foreign)
	actor := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{
		"expense.invoice.read", "expense.scope.tenant_all",
	}}
	items, more := duplicateDetail(t, s, actor, "source")
	if len(items) != 1 || more || items[0].(map[string]any)["invoice_id"] != "visible" {
		t.Fatalf("明确全租户仍不得跨租户提示: %+v more=%t", items, more)
	}
}
