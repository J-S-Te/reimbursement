package application

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestArchiveFreezeRejectsAggregateSourceLimit 固定多票各自未超限而整单来源超限时的显式拒绝行为。
// 参数：t 为临时 SQLite 归档冻结边界测试，不执行真实付款或外部调用。
// 返回值：无；超过2000项被截断或误报成功时失败。
func TestArchiveFreezeRejectsAggregateSourceLimit(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	c := domain.Claim{ID: "wide-claim", TenantID: "t"}
	rows := []store.InvoiceSource{}
	for invoice := 0; invoice < 3; invoice++ {
		id := fmt.Sprintf("wide-invoice-%d", invoice)
		c.Items = append(c.Items, domain.Item{InvoiceUsages: []domain.InvoiceUsage{{InvoiceID: id}}})
		for index := 0; index < 667; index++ {
			rows = append(rows, store.InvoiceSource{
				ID: fmt.Sprintf("wide-source-%d-%d", invoice, index), TenantID: "t", InvoiceID: id,
				DocumentID: "original", ParseRunID: "run", CandidateIndex: index, SourcesJSON: "[]",
			})
		}
	}
	if err := db.CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	evidence, err := freezeInvoiceArchiveEvidence(db, c)
	sourceErrorCode(t, err, "ARCHIVE_HISTORY_LIMIT", 422)
	if len(evidence.SourceIDs) != 2001 {
		t.Fatal("边界预检静默截断来源集合", len(evidence.SourceIDs))
	}
}

// TestArchiveFreezeRejectsAggregateControlBytes 固定控制条数未超限而总JSON超限时的显式拒绝行为。
// 参数：t 为临时 SQLite 归档冻结边界测试，案件内容为合成合法JSON。
// 返回值：无；超过2MiB被截断或误报成功时失败。
func TestArchiveFreezeRejectsAggregateControlBytes(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	c := domain.Claim{ID: "large-control-claim", TenantID: "t", Items: []domain.Item{
		{InvoiceUsages: []domain.InvoiceUsage{{InvoiceID: "large-invoice"}}},
	}}
	content, err := json.Marshal(map[string]any{
		"reason":           strings.Repeat("x", 2000),
		"previous":         map[string]any{"invoice_number": strings.Repeat("1", 100)},
		"proposed_invoice": map[string]any{"invoice_number": strings.Repeat("2", 100)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := []store.InvoiceCorrection{}
	for index := 0; index < 1000; index++ {
		rows = append(rows, store.InvoiceCorrection{ID: fmt.Sprintf("large-control-%d", index),
			TenantID: "t", InvoiceID: "large-invoice", Status: "REJECTED", ContentJSON: string(content)})
	}
	if err := db.CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	_, err = freezeInvoiceArchiveEvidence(db, c)
	sourceErrorCode(t, err, "ARCHIVE_HISTORY_LIMIT", 422)
}
