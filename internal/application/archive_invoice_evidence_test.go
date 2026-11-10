package application

import (
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"strings"
	"testing"
)

// TestArchiveFreezesInvoiceControlAndSourceSet 验证请求时固定风险内容及来源集合，不混入后来修订。
// 参数：t 为临时数据库内的单个归档冻结场景。
// 返回值：无。
func TestArchiveFreezesInvoiceControlAndSourceSet(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	c := domain.Claim{ID: "claim", TenantID: "t", Items: []domain.Item{{InvoiceUsages: []domain.InvoiceUsage{{InvoiceID: "invoice"}}}}}
	rows := []any{&store.InvoiceSource{ID: "source-before", TenantID: "t", InvoiceID: "invoice", DocumentID: "doc", SourcesJSON: "{}"}, &store.InvoiceRisk{ID: "risk", TenantID: "t", InvoiceID: "invoice", OriginalInvoiceID: "invoice", Kind: "VOID", Status: "OPEN", ContentJSON: `{"reason":"需核查","evidence_file_ids":["proof"]}`}}
	for _, row := range rows {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	frozen, err := archiveHistoryReferences(db, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&store.InvoiceRisk{}).Where("id = ?", "risk").Update("status", "CLOSED").Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&store.InvoiceSource{ID: "source-after", TenantID: "t", InvoiceID: "invoice", DocumentID: "future", SourcesJSON: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "source-before") || strings.Contains(text, "source-after") || !strings.Contains(text, `"status":"OPEN"`) || !strings.Contains(text, "proof") {
		t.Fatalf("请求未冻结票据证据: %s", text)
	}
}

// TestPaidInvoiceSupplementQueuesNewArchiveWithoutNewSpend 验证已付票据补充原件追加档案而不改变资金事实。
// 参数：t 为单个已付款补证场景。
// 返回值：无。
func TestPaidInvoiceSupplementQueuesNewArchiveWithoutNewSpend(t *testing.T) {
	service, db, claim, app, _ := approvedControlFixture(t)
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record"}}
	runCommand(t, service, cashier, "payment.record", claim.ID, bindingPaymentBody("supplement-paid", claim.Version, "supplement-transaction"))
	var before int64
	if err := db.Model(&store.Archive{}).Where("claim_id = ?", claim.ID).Count(&before).Error; err != nil {
		t.Fatal(err)
	}
	var invoice store.Invoice
	if err := db.First(&invoice, "id = ?", "invoice").Error; err != nil {
		t.Fatal(err)
	}
	app.Permissions = append(app.Permissions, "expense.invoice.review", "expense.invoice.read")
	runCommand(t, service, app, "invoice.source.append", invoice.ID, map[string]any{"command_id": "supplement-source", "expected_version": invoice.Version, "reason": "补齐同票OFD权威原件", "file_ids": []string{"ofd-file"}})
	var after, effects int64
	if err := db.Model(&store.Archive{}).Where("claim_id = ?", claim.ID).Count(&after).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&store.Effect{}).Where("claim_id = ?", claim.ID).Count(&effects).Error; err != nil {
		t.Fatal(err)
	}
	current, _, err := loadClaim(db, "t", claim.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if after != before+1 || effects != 1 || current.ProjectionRevision != 1 {
		t.Fatalf("补证未追加档案或改变资金: archives %d→%d effects=%d revision=%d", before, after, effects, current.ProjectionRevision)
	}
}
