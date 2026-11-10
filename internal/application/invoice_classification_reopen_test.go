package application

import (
	"encoding/json"
	"testing"

	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// TestInvoiceClassificationEvidenceKeepsConsumedOwnInvoice 保护付款冲正后同单重开草稿的合法用票。
// 参数：t 为隔离数据库测试上下文。
// 返回值：无；同单已消费票允许保存依据，其他单据消费票仍拒绝。
func TestInvoiceClassificationEvidenceKeepsConsumedOwnInvoice(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	_, id := submissionFixture(t, db, true)
	c, _, err := loadClaim(db, "t", id, false)
	if err != nil {
		t.Fatal(err)
	}
	classificationJSONFixture(t, db, &store.Config{}, "rule", "content_mappings",
		json.RawMessage(`[{"id":"travel","keywords":["差旅"],"fee_type":"TRAVEL"}]`))
	if err = db.Model(&store.Invoice{}).Where("id = ?", "invoice").Updates(
		map[string]any{"reservation_claim_id": c.ID, "reservation_state": "CONSUMED"}).Error; err != nil {
		t.Fatal(err)
	}
	if err = rebuildDraftClassificationEvidence(db, &c); err != nil {
		t.Fatalf("本单已消费票阻断合法重开草稿: %v", err)
	}
	if err = db.Model(&store.Invoice{}).Where("id = ?", "invoice").Update("reservation_claim_id", "other").Error; err != nil {
		t.Fatal(err)
	}
	if err = rebuildDraftClassificationEvidence(db, &c); err == nil {
		t.Fatal("不能使用其他单据已消费票")
	}
}
