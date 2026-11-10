package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/internal/integration"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type archiveStateFixture struct {
	processor *Processor
	record    Record
	request   map[string]any
	cutoff    time.Time
}

// archiveStateJSON 生成与应用digest同口径的合成不可变内容及摘要。
// 参数：t为测试上下文，value为领域事实。
// 返回值：JSON和SHA256；编码失败立即终止。
func archiveStateJSON(t *testing.T, value any) (string, string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	return string(raw), hex.EncodeToString(hash[:])
}

// archiveStateApproval 构造可由真实批准摘要校验器复算的最小合成快照。
// 参数：t为测试上下文，round为真实审批轮次。
// 返回值：持久快照及同轮单据。
func archiveStateApproval(t *testing.T, round int) (store.Snapshot, domain.Claim) {
	t.Helper()
	c := domain.Claim{ID: "c", TenantID: "t", ClaimNo: "BX-1", CurrentRound: round, Currency: "CNY", Payable: 100}
	if round == 2 {
		c.Items = []domain.Item{{ID: "item", HistoricalTailEvidenceFileIDs: []string{"tail-evidence"}, TravelConfirmation: &domain.TravelConfirmation{Decision: "MANUAL_CONFIRM", Reason: "合成证据", EvidenceFileIDs: []string{"travel-proof"}}}}
	}
	value := struct {
		Claim    domain.Claim `json:"claim"`
		Approval struct {
			Tasks       []domain.Task             `json:"tasks"`
			References  []domain.ProjectReference `json:"references"`
			RuleVersion string                    `json:"rule_version"`
		} `json:"approval"`
		Invoices []domain.Invoice `json:"invoices"`
	}{Claim: c}
	_, hash := archiveStateJSON(t, value)
	value.Claim.ContentHash = hash
	c.ContentHash = hash
	raw, _ := archiveStateJSON(t, value)
	if err := application.VerifyApprovalSnapshot(json.RawMessage(raw), hash); err != nil {
		t.Fatal(err)
	}
	return store.Snapshot{ID: string(rune('a' + round)), TenantID: "t", ClaimID: "c", Round: round, ContentHash: hash, ContentJSON: raw}, c
}

// newArchiveStateFixture 构造包版本5而财务版本1、当前批准轮2的隔离归档链。
// 参数：t为测试上下文。
// 返回值：冻结精确ID及可核验文件的本地夹具，不使用业务数据库或外部网关。
func newArchiveStateFixture(t *testing.T) archiveStateFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	if err = db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	first, _ := archiveStateApproval(t, 1)
	second, document := archiveStateApproval(t, 2)
	cutoff := time.Now().UTC().Truncate(time.Millisecond)
	first.CreatedAt = cutoff.Add(-time.Hour)
	second.CreatedAt = cutoff.Add(-time.Minute)
	document.Status = "VOID"
	document.Version = 7
	document.ProjectionRevision = 1
	document.Items[0].HistoricalTailEvidenceFileIDs = append(document.Items[0].HistoricalTailEvidenceFileIDs, "request-tail-evidence")
	fact := domain.PaymentExceptionFact{ID: "fact", TenantID: "t", ClaimID: "c", Recorder: "cashier", Amount: 100, Currency: "CNY", VoucherFileIDs: []string{"exception-voucher"}, CreatedAt: cutoff}
	factRaw, factHash := archiveStateJSON(t, fact)
	review := domain.PaymentExceptionReview{ID: "review", ExceptionID: "fact", Actor: "finance", Decision: "RESOLVE_WITH_PAYMENT", Version: 2, EvidenceFileIDs: []string{"review-proof"}, LinkedPaymentID: "payment", CreatedAt: cutoff}
	reviewRaw, reviewHash := archiveStateJSON(t, review)
	paymentRaw, _ := archiveStateJSON(t, application.PaymentResult{ID: "payment", ClaimID: "c", ContentHash: first.ContentHash, VoucherFileIDs: []string{"payment-voucher"}})
	spend := domain.SpendSnapshot{SchemaVersion: 1, EventID: "spend", SourceApplication: "reimbursement", SourceEnvironment: "test", TenantID: "t", ClaimID: "c", ClaimNo: "BX-1", DocumentRevision: 2, ApprovalRound: 1, ProjectionRevision: 1, CompleteSnapshot: true, Trigger: "PAYMENT_RECORDED", Currency: "CNY", Effects: []domain.SpendEffect{{EffectID: "payment", PaymentRecordID: "payment", Kind: "PAYMENT_RECORDED", AmountMinor: "100", OccurredAt: "2026-11-10T01:00:00Z", RecordedAt: "2026-11-10T01:00:00Z", ApprovedContentHash: first.ContentHash, Allocations: []domain.SpendAllocation{{ProjectID: "project", FeeType: "TRAVEL", AmountMinor: "100"}}}}, Contributions: []domain.SpendContribution{{ProjectID: "project", FeeType: "TRAVEL", PaidNetMinor: "100"}}}
	spend.SnapshotHash, _ = domain.SpendSnapshotHash(spend)
	if err = domain.ValidateSpendSnapshot(spend); err != nil {
		t.Fatal(err)
	}
	spendRaw, _ := archiveStateJSON(t, spend)
	request := map[string]any{"claim_id": "c", "projection_revision": 1, "content_hash": document.ContentHash, "package_id": "archive", "package_version": 5, "current_approval_round": 2, "document_snapshot": document, "exception_fact_ids": []string{"fact"}, "exception_review_ids": []string{"review"}, "claim_action_ids": []string{"kept-action"}, "approval_action_detail_ids": []string{"kept-detail"}}
	requestRaw, _ := archiveStateJSON(t, request)
	until := time.Now().UTC().Add(time.Hour)
	rows := []any{&first, &second, &store.Outbox{ID: "spend", EventID: "spend", TenantID: "t", ResourceID: "c", Topic: "project_spend", PayloadJSON: spendRaw, CreatedAt: cutoff.Add(-time.Hour)}, &store.Job{ID: "job", TenantID: "t", ResourceID: "archive", PayloadJSON: requestRaw, Status: "PROCESSING", Version: 1, LeaseOwner: "worker", LeaseUntil: &until}, &store.Archive{ID: "archive", TenantID: "t", ClaimID: "c", PackageVersion: 5, Status: "PENDING", ManifestJSON: requestRaw, CreatedAt: cutoff}, &store.Payment{ID: "payment", TenantID: "t", ClaimID: "c", Method: "BANK", SourceAccountRef: "source", TransactionNo: "normal", ContentJSON: paymentRaw, CreatedAt: cutoff.Add(-time.Hour)}, &store.PaymentExceptionFact{ID: fact.ID, TenantID: "t", ClaimID: "c", Method: "BANK", SourceAccountRef: "source", TransactionNo: "exception", ContentJSON: factRaw, ContentHash: factHash, CreatedAt: cutoff}, &store.PaymentExceptionReview{ID: review.ID, TenantID: "t", ClaimID: "c", ExceptionID: fact.ID, Version: 2, ContentJSON: reviewRaw, ContentHash: reviewHash, CreatedAt: cutoff}, &store.InvoiceParseRun{ID: "parse-between", TenantID: "t", DocumentID: "payment-voucher", Status: "COMPLETED", ResultHash: "result", CreatedAt: cutoff.Add(-time.Minute)}, &store.PaymentExceptionCursor{ID: "fact", TenantID: "t", ClaimID: "c", Status: "FUTURE_CURSOR", Version: 99}, &store.Action{ID: "kept-action", TenantID: "t", ClaimID: "c", Round: 2, Action: "CLAIM_APPROVED_VOID", CreatedAt: cutoff}, &store.Action{ID: "future-action", TenantID: "t", ClaimID: "c", Round: 2, Action: "FUTURE_SAME_MILLISECOND", CreatedAt: cutoff}, &store.ActionDetail{ID: "kept-detail", TenantID: "t", ClaimID: "c", ActionID: "kept-action", ContentJSON: `{"round":2,"evidence_file_ids":["detail-proof"]}`, CreatedAt: cutoff}}
	for _, row := range rows {
		if err = db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	hash := sha256.Sum256([]byte("evidence"))
	meta, _ := json.Marshal(map[string]string{"sha256": hex.EncodeToString(hash[:])})
	for _, file := range []string{"payment-voucher", "exception-voucher", "review-proof", "detail-proof", "tail-evidence", "travel-proof", "request-tail-evidence"} {
		if err = db.Create(&store.Document{ID: file, TenantID: "t", FileID: file, Status: "READY", ContentJSON: string(meta), CreatedAt: cutoff}).Error; err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/oauth2/token":
			w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"download","expires_in":300}`))
		case strings.HasSuffix(r.URL.Path, "/download-tickets"):
			w.Write([]byte(`{"data":{"ticket":"once"}}`))
		case strings.HasSuffix(r.URL.Path, "/content"):
			w.Write([]byte("evidence"))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	return archiveStateFixture{processor: &Processor{DB: db, Files: integration.NewFiles(db, integration.NewClient(server.URL, server.URL, "client", "secret", "download"))}, record: Record{ID: "job", TenantID: "t", ResourceID: "archive", Version: 1, LeaseOwner: "worker", PayloadJSON: requestRaw}, request: request, cutoff: cutoff}
}

// TestArchiveIndependentPackageVersionAndImmutableExceptionHistory 验证归档包版本与财务版本独立且不混入同毫秒未来动作。
// 参数：t为测试上下文。
// 返回值：无；财务轮、当前轮、异常或精确历史缺失时失败。
func TestArchiveIndependentPackageVersionAndImmutableExceptionHistory(t *testing.T) {
	f := newArchiveStateFixture(t)
	if err := f.processor.archive(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
	var archive store.Archive
	f.processor.DB.First(&archive, "id = ?", "archive")
	var manifest struct {
		Payment  domain.SpendSnapshot `json:"payment_snapshot"`
		Document domain.Claim         `json:"document_at_archive_request"`
		History  struct {
			Snapshots []store.Snapshot               `json:"approval_snapshots"`
			Facts     []store.PaymentExceptionFact   `json:"payment_exception_facts"`
			Reviews   []store.PaymentExceptionReview `json:"payment_exception_reviews"`
			Actions   []store.Action                 `json:"approval_actions"`
		} `json:"history"`
		Files     []map[string]string  `json:"files"`
		ParseRuns []archiveParseRunRef `json:"parse_runs"`
	}
	if err := json.Unmarshal([]byte(archive.ManifestJSON), &manifest); err != nil {
		t.Fatal(err)
	}
	if archive.Status != "SEALED" || archive.PackageVersion != 5 || manifest.Payment.ProjectionRevision != 1 || manifest.Document.Status != "VOID" || len(manifest.History.Snapshots) != 2 || len(manifest.History.Facts) != 1 || len(manifest.History.Reviews) != 1 || len(manifest.History.Actions) != 1 || manifest.History.Actions[0].ID != "kept-action" || len(manifest.Files) != 7 || len(manifest.ParseRuns) != 1 || strings.Contains(archive.ManifestJSON, "FUTURE_CURSOR") {
		t.Fatalf("frozen archive mismatch %+v", manifest)
	}
}

// TestArchiveFrozenMissingIDsHashesAndEvidenceFailClosed 验证异常档案缺ID、内容摘要或原始证据时不得封存。
// 参数：t为测试上下文。
// 返回值：无；不完整档案被标记SEALED时失败。
func TestArchiveFrozenMissingIDsHashesAndEvidenceFailClosed(t *testing.T) {
	for _, kind := range []string{"missing-id", "missing-key", "bad-hash", "missing-evidence", "cross-claim", "too-many-ids", "parse-future"} {
		t.Run(kind, func(t *testing.T) {
			f := newArchiveStateFixture(t)
			switch kind {
			case "missing-id":
				f.request["exception_review_ids"] = []string{"missing"}
			case "missing-key":
				delete(f.request, "exception_fact_ids")
			case "bad-hash":
				f.processor.DB.Model(&store.PaymentExceptionReview{}).Where("id = ?", "review").Update("content_hash", strings.Repeat("0", 64))
			case "cross-claim":
				f.processor.DB.Model(&store.PaymentExceptionReview{}).Where("id = ?", "review").Update("claim_id", "foreign")
			case "parse-future":
				f.processor.DB.Model(&store.InvoiceParseRun{}).Where("id = ?", "parse-between").Update("completed_at", f.cutoff.Add(time.Second))
			case "too-many-ids":
				f.request["exception_fact_ids"] = make([]string, 2001)
			case "missing-evidence":
				f.processor.DB.Delete(&store.Document{}, "id = ?", "review-proof")
			}
			raw, _ := archiveStateJSON(t, f.request)
			f.processor.DB.Model(&store.Job{}).Where("id = ?", "job").Update("payload_json", raw)
			f.processor.DB.Model(&store.Archive{}).Where("id = ?", "archive").Update("manifest_json", raw)
			f.record.PayloadJSON = raw
			expected := map[string]string{"missing-id": "ARCHIVE_EXCEPTION_REVIEW_MISSING", "missing-key": "ARCHIVE_REQUEST_REFERENCE_SET_MISSING", "bad-hash": "ARCHIVE_EXCEPTION_REVIEW_HASH_MISMATCH", "missing-evidence": "ARCHIVE_FILE_MISSING", "cross-claim": "ARCHIVE_EXCEPTION_REVIEW_MISSING", "too-many-ids": "ARCHIVE_REFERENCE_LIMIT", "parse-future": "ARCHIVE_PARSE_COMPLETED_AFTER_REQUEST"}
			if err := f.processor.archive(context.Background(), f.record); err == nil || err.Error() != expected[kind] {
				t.Fatal("wrong failure", kind, err)
			}
			var archive store.Archive
			f.processor.DB.First(&archive, "id = ?", "archive")
			if archive.Status == "SEALED" {
				t.Fatal("failed archive became sealed")
			}
		})
	}
}

// TestArchiveExplicitEmptyReferencesExcludeSameInstantFutureRows 验证显式空集合不能回退收录同毫秒后来的异常和动作。
// 参数：t为测试上下文。
// 返回值：无；冻结空历史中出现未来记录时失败。
func TestArchiveExplicitEmptyReferencesExcludeSameInstantFutureRows(t *testing.T) {
	f := newArchiveStateFixture(t)
	for _, key := range []string{"exception_fact_ids", "exception_review_ids", "claim_action_ids", "approval_action_detail_ids"} {
		f.request[key] = []string{}
	}
	raw, _ := archiveStateJSON(t, f.request)
	f.processor.DB.Model(&store.Job{}).Where("id = ?", "job").Update("payload_json", raw)
	f.processor.DB.Model(&store.Archive{}).Where("id = ?", "archive").Update("manifest_json", raw)
	if err := f.processor.archive(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
	var archive store.Archive
	f.processor.DB.First(&archive, "id = ?", "archive")
	var manifest struct {
		History archiveHistory      `json:"history"`
		Files   []map[string]string `json:"files"`
	}
	if err := json.Unmarshal([]byte(archive.ManifestJSON), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.History.ExceptionFacts) != 0 || len(manifest.History.ExceptionReviews) != 0 || len(manifest.History.Actions) != 0 || len(manifest.History.ActionDetails) != 0 || len(manifest.Files) != 4 {
		t.Fatal("empty set became future history", archive.ManifestJSON)
	}
}

// TestArchiveSealAuditAtomicAndReplayOnce 验证成功封存同事务写一条审计，重放不重复，审计失败整体回滚。
// 参数：t为测试上下文。
// 返回值：无；审计与封存分离或重放重复时失败。
func TestArchiveSealAuditAtomicAndReplayOnce(t *testing.T) {
	f := newArchiveStateFixture(t)
	if err := f.processor.archive(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
	if err := f.processor.archive(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
	var count int64
	f.processor.DB.Model(&store.Outbox{}).Where("topic=?", "audit").Count(&count)
	if count != 1 {
		t.Fatal("replayed seal audit", count)
	}
	failed := newArchiveStateFixture(t)
	if err := failed.processor.DB.Exec(`CREATE TRIGGER reject_seal_audit BEFORE INSERT ON exp_outbox WHEN NEW.topic='audit' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := failed.processor.archive(context.Background(), failed.record); err == nil {
		t.Fatal("audit failure ignored")
	}
	var archive store.Archive
	failed.processor.DB.First(&archive, "id = ?", "archive")
	if archive.Status == "SEALED" || archive.ManifestHash != "" {
		t.Fatal("seal survived audit rollback")
	}
}
