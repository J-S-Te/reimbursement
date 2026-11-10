package application

import (
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"testing"
)

// TestMergeKeepsEveryCandidateRegion 防止同文件同run的第二候选归并时丢失独立页区来源。
// 参数：t为测试上下文。
// 返回值：无。
func TestMergeKeepsEveryCandidateRegion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&store.InvoiceSource{}); err != nil {
		t.Fatal(err)
	}
	for _, source := range []store.InvoiceSource{{ID: "first", TenantID: "t", InvoiceID: "canonical", DocumentID: "doc", ParseRunID: "run", CandidateIndex: 0, SourcesJSON: `[{"page":1}]`, SourcesHash: parseSourceHash(`[{"page":1}]`)}, {ID: "second", TenantID: "t", InvoiceID: "candidate", DocumentID: "doc", ParseRunID: "run", CandidateIndex: 1, SourcesJSON: `[{"page":2}]`, SourcesHash: parseSourceHash(`[{"page":2}]`)}} {
		if err = db.Create(&source).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err = CopyInvoiceSources(db, "t", "candidate", "canonical"); err != nil {
		t.Fatal(err)
	}
	var rows []store.InvoiceSource
	if err = db.Where("tenant_id = ? AND invoice_id = ?", "t", "canonical").Order("candidate_index ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].CandidateIndex != 0 || rows[1].CandidateIndex != 1 {
		t.Fatalf("source region lost during canonical merge: %+v", rows)
	}
}
