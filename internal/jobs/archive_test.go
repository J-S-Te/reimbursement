package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/internal/integration"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestArchiveRequiresFrozenInvoiceOriginal 使用旧简化效果验证原件归档，完整支出协议另测。
// 参数：t为测试上下文。
// 返回值：无。
func TestArchiveRequiresFrozenInvoiceOriginal(t *testing.T) {
	// 此旧夹具缺少完整effect/hash，仅验证原件与冲正证据，不证明完整财务快照协议。
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("voucher"))
	meta, _ := json.Marshal(map[string]string{"sha256": hex.EncodeToString(hash[:])})
	frozen := struct {
		Claim    domain.Claim `json:"claim"`
		Approval struct {
			Tasks       []domain.Task             `json:"tasks"`
			References  []domain.ProjectReference `json:"references"`
			RuleVersion string                    `json:"rule_version"`
		} `json:"approval"`
		Invoices []domain.Invoice `json:"invoices"`
	}{Claim: domain.Claim{ID: "c", CurrentRound: 1, Items: []domain.Item{{ID: "item", InvoiceUsages: []domain.InvoiceUsage{{InvoiceID: "invoice"}}}}}, Invoices: []domain.Invoice{{ID: "invoice", FileIDs: []string{"missing-original"}}}}
	canonical, _ := json.Marshal(frozen)
	approved := sha256.Sum256(canonical)
	approvedHash := hex.EncodeToString(approved[:])
	frozen.Claim.ContentHash = approvedHash
	frozenJSON, _ := json.Marshal(frozen)
	until := time.Now().UTC().Add(time.Hour)
	rows := []any{&store.Job{ID: "job1", TenantID: "t", ResourceID: "archive", Status: "PROCESSING", Version: 1, LeaseOwner: "worker", LeaseUntil: &until, PayloadJSON: "{}"}, &store.Archive{ID: "archive", TenantID: "t", ClaimID: "c", PackageVersion: 1, Status: "PENDING", ManifestJSON: "{}"}, &store.Snapshot{ID: "snapshot", TenantID: "t", ClaimID: "c", Round: 1, ContentHash: approvedHash, ContentJSON: string(frozenJSON)}, &store.Payment{ID: "payment", TenantID: "t", ClaimID: "c", Method: "BANK", SourceAccountRef: "source", TransactionNo: "transaction", ContentJSON: `{"voucher_file_ids":["vf"]}`}, &store.Document{ID: "voucher", TenantID: "t", Owner: "cashier", Status: "READY", FileID: "vf", ContentJSON: string(meta)}}
	spend, _ := json.Marshal(domain.SpendSnapshot{ClaimID: "c", ApprovalRound: 1, ProjectionRevision: 1, Effects: []domain.SpendEffect{{EffectID: "payment", PaymentRecordID: "payment", Kind: "PAYMENT_RECORDED"}}})
	rows = append(rows, &store.Outbox{ID: "event", EventID: "event", TenantID: "t", ResourceID: "c", Topic: "project", PayloadJSON: string(spend)})
	for _, row := range rows {
		if err = db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth2/token":
			w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"download","expires_in":300}`))
		case "/api/v2/files/vf/download-tickets", "/api/v2/files/missing-original/download-tickets", "/api/v2/files/refund/download-tickets":
			w.Write([]byte(`{"data":{"ticket":"once"}}`))
		case "/api/v2/files/vf/content", "/api/v2/files/missing-original/content", "/api/v2/files/refund/content":
			w.Write([]byte("voucher"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p := Processor{DB: db, Files: integration.NewFiles(db, integration.NewClient(server.URL, server.URL, "client", "secret", "download"))}
	if err = p.archive(context.Background(), Record{ID: "job1", TenantID: "t", ResourceID: "archive", Version: 1, LeaseOwner: "worker"}); err == nil {
		t.Fatal("archive sealed without frozen invoice original")
	}
	var archive store.Archive
	db.First(&archive, "id = ?", "archive")
	if archive.Status == "SEALED" {
		t.Fatal("missing invoice falsely sealed")
	}
	if err = db.Create(&store.Document{ID: "original", TenantID: "t", Owner: "applicant", Status: "PARSED", FileID: "missing-original", ContentJSON: string(meta)}).Error; err != nil {
		t.Fatal(err)
	}
	if err = p.archive(context.Background(), Record{ID: "job1", TenantID: "t", ResourceID: "archive", Version: 1, LeaseOwner: "worker"}); err != nil {
		t.Fatalf("complete frozen archive failed: %v", err)
	}
	db.First(&archive, "id = ?", "archive")
	if archive.Status != "SEALED" || archive.ManifestHash == "" {
		t.Fatal("complete archive not sealed")
	}
	var manifest struct {
		Files           []map[string]string `json:"files"`
		LegacyFallbacks []string            `json:"legacy_fallbacks"`
	}
	if json.Unmarshal([]byte(archive.ManifestJSON), &manifest) != nil || len(manifest.Files) != 2 || len(manifest.LegacyFallbacks) == 0 {
		t.Fatal("archive does not contain original and voucher")
	}

	reversalJSON := `{"request":{"id":"request","status":"APPROVED","evidence_file_ids":["refund"],"fact_kind":"FULL_REFUND"}}`
	next, _ := json.Marshal(domain.SpendSnapshot{ClaimID: "c", ApprovalRound: 1, ProjectionRevision: 2, Effects: []domain.SpendEffect{{EffectID: "payment", PaymentRecordID: "payment", Kind: "PAYMENT_RECORDED"}, {EffectID: "reversal", PaymentRecordID: "payment", ReversesEffectID: "payment", Kind: "PAYMENT_FULLY_REVERSED"}}})
	for _, row := range []any{&store.Job{ID: "job2", TenantID: "t", ResourceID: "archive2", Status: "PROCESSING", Version: 1, LeaseOwner: "worker", LeaseUntil: &until, PayloadJSON: "{}"}, &store.Reversal{ID: "reversal", TenantID: "t", ClaimID: "c", PaymentID: "payment", ContentJSON: reversalJSON}, &store.ReversalRequest{ID: "request", TenantID: "t", ClaimID: "c", PaymentID: "payment", Status: "APPROVED", ContentJSON: reversalJSON}, &store.Archive{ID: "archive2", TenantID: "t", ClaimID: "c", PackageVersion: 2, Status: "PENDING", ManifestJSON: "{}"}, &store.Outbox{ID: "event2", EventID: "event2", TenantID: "t", ResourceID: "c", Topic: "project", PayloadJSON: string(next)}} {
		if err = db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err = p.archive(context.Background(), Record{ID: "job2", TenantID: "t", ResourceID: "archive2", Version: 1, LeaseOwner: "worker"}); err == nil {
		t.Fatal("refund archive sealed without reversal evidence")
	}
	if err = db.Create(&store.Document{ID: "refund-doc", TenantID: "t", Owner: "finance", Status: "READY", FileID: "refund", ContentJSON: string(meta)}).Error; err != nil {
		t.Fatal(err)
	}
	if err = p.archive(context.Background(), Record{ID: "job2", TenantID: "t", ResourceID: "archive2", Version: 1, LeaseOwner: "worker"}); err != nil {
		t.Fatalf("complete reversal evidence failed: %v", err)
	}
	var second store.Archive
	db.First(&second, "id = ?", "archive2")
	if second.Status != "SEALED" {
		t.Fatal("complete reversal archive not sealed")
	}
	var full struct {
		Files   []map[string]string `json:"files"`
		History struct {
			Reversals []store.Reversal `json:"reversals"`
		} `json:"history"`
	}
	if json.Unmarshal([]byte(second.ManifestJSON), &full) != nil || len(full.Files) != 3 || len(full.History.Reversals) != 1 {
		t.Fatal("reversal evidence/history missing from sealed manifest")
	}

}
