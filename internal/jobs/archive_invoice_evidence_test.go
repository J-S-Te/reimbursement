package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"strings"
	"testing"
)

// TestArchiveIncludesFrozenInvoiceSupplementAndRisk 验证补充来源原件及请求时风险状态进入封存版本。
// 参数：t 为隔离归档worker单场景。
// 返回值：无；不声明真实文件网关验收。
func TestArchiveIncludesFrozenInvoiceSupplementAndRisk(t *testing.T) {
	for _, payload := range []string{"", "{}", `{"claim_id":"other","invoice_evidence":null}`} {
		t.Run("leased-payload-"+payload, func(t *testing.T) {
			assertArchiveFrozenInvoiceEvidence(t, payload)
		})
	}
}

// assertArchiveFrozenInvoiceEvidence 验证租约载荷副本不能覆盖持久请求中的冻结补证。
// 参数：t 为本地归档上下文，recordPayload 为有意过期或缺省的租约载荷副本。
// 返回值：无；持久冻结来源、补充原件或风险快照遗漏时失败。
func assertArchiveFrozenInvoiceEvidence(t *testing.T, recordPayload string) {
	t.Helper()
	f := newArchiveStateFixture(t)
	hash := sha256.Sum256([]byte("evidence"))
	meta, _ := json.Marshal(map[string]string{"sha256": hex.EncodeToString(hash[:])})
	sourceJSON := `{"method":"MANUAL_LINK","reason":"补充OFD"}`
	sourceHash := sha256.Sum256([]byte(sourceJSON))
	if err := f.processor.DB.Create(&store.Document{ID: "supplement", TenantID: "t", FileID: "supplement-file", Status: "READY", ContentJSON: string(meta)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.processor.DB.Create(&store.InvoiceSource{ID: "source", TenantID: "t", InvoiceID: "invoice", DocumentID: "supplement", SourcesJSON: sourceJSON, SourcesHash: hex.EncodeToString(sourceHash[:]), CreatedAt: f.cutoff}).Error; err != nil {
		t.Fatal(err)
	}
	f.request["invoice_evidence"] = domain.ArchivedInvoiceEvidence{SourceIDs: []string{"source"}, Controls: []domain.ArchivedInvoiceControl{{Kind: "RISK", Record: json.RawMessage(`{"id":"risk","tenant_id":"t","status":"OPEN"}`), Content: json.RawMessage(`{"evidence_file_ids":["detail-proof"],"reason":"补充财务复核"}`)}}}
	evidence := f.request["invoice_evidence"].(domain.ArchivedInvoiceEvidence)
	evidence.Controls = append(evidence.Controls, domain.ArchivedInvoiceControl{Kind: "IMPACT", Record: json.RawMessage(`{"tenant_id":"t","claim_id":"c","status":"RECONFIRMED"}`), Content: json.RawMessage(`{"reason":"申请人重新确认"}`)})
	f.request["invoice_evidence"] = evidence
	raw, _ := archiveStateJSON(t, f.request)
	if err := f.processor.DB.Model(&store.Job{}).Where("id = ?", "job").Update("payload_json", raw).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.processor.DB.Model(&store.Archive{}).Where("id = ?", "archive").Update("manifest_json", raw).Error; err != nil {
		t.Fatal(err)
	}
	f.record.PayloadJSON = recordPayload
	if err := f.processor.archive(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
	var archive store.Archive
	if err := f.processor.DB.First(&archive, "id = ?", "archive").Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(archive.ManifestJSON, "supplement-file") || !strings.Contains(archive.ManifestJSON, `"status":"OPEN"`) || !strings.Contains(archive.ManifestJSON, "补充财务复核") {
		t.Fatal("补充原件或冻结风险内容未进入清单")
	}
}

// TestArchivePersistedInvoiceEvidenceFailsClosed 验证冻结请求的非法补证不能因租约副本缺省而被忽略。
// 参数：t 为隔离归档测试上下文，包含类型错误、重复来源、缺失来源和跨租户控制记录。
// 返回值：无；非法证据被封存或错误码不符时失败。
func TestArchivePersistedInvoiceEvidenceFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence any
		code     string
	}{
		{"invalid-shape", "not-an-object", "ARCHIVE_REQUEST_INVALID"},
		{"duplicate-source", domain.ArchivedInvoiceEvidence{
			SourceIDs: []string{"s", "s"}}, "ARCHIVE_INVOICE_EVIDENCE_LIMIT"},
		{"missing-source", domain.ArchivedInvoiceEvidence{
			SourceIDs: []string{"missing"}}, "ARCHIVE_INVOICE_SOURCE_MISSING"},
		{"foreign-control", domain.ArchivedInvoiceEvidence{
			Controls: []domain.ArchivedInvoiceControl{{Kind: "RISK",
				Record: json.RawMessage(`{"tenant_id":"other"}`), Content: json.RawMessage(`{}`)}},
		}, "ARCHIVE_INVOICE_CONTROL_SCOPE_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchiveStateFixture(t)
			f.request["invoice_evidence"] = tc.evidence
			raw, _ := archiveStateJSON(t, f.request)
			if err := f.processor.DB.Model(&store.Job{}).Where("id = ?", "job").Update("payload_json", raw).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.processor.DB.Model(&store.Archive{}).Where("id = ?", "archive").Update("manifest_json", raw).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.processor.archive(context.Background(), f.record); err == nil || err.Error() != tc.code {
				t.Fatal("invalid frozen evidence was ignored", err)
			}
			var archive store.Archive
			if err := f.processor.DB.First(&archive, "id = ?", "archive").Error; err != nil || archive.Status == "SEALED" {
				t.Fatal("invalid evidence sealed", archive.Status, err)
			}
		})
	}
}

// TestArchiveLegacyInvoiceEvidenceKeepsValidatedClaimScope 验证旧请求省略单据ID时仍保留已有冻结补证。
// 参数：t 为隔离请求读取上下文，归属仅从已核验的档案记录派生。
// 返回值：无；既有冻结控制丢失或跨单据控制被接受时失败。
func TestArchiveLegacyInvoiceEvidenceKeepsValidatedClaimScope(t *testing.T) {
	f := newArchiveStateFixture(t)
	for _, claim := range []string{"c", "foreign"} {
		evidence := domain.ArchivedInvoiceEvidence{Controls: []domain.ArchivedInvoiceControl{{
			Kind: "IMPACT", Record: json.RawMessage(`{"tenant_id":"t","claim_id":"` + claim + `"}`),
			Content: json.RawMessage(`{}`),
		}}}
		raw, _ := archiveStateJSON(t, map[string]any{
			"projection_revision": 1, "invoice_evidence": evidence,
		})
		if err := f.processor.DB.Model(&store.Job{}).Where("id = ?", "job").Update("payload_json", raw).Error; err != nil {
			t.Fatal(err)
		}
		archive := store.Archive{ID: "archive", ClaimID: "c", PackageVersion: 5}
		request, err := f.processor.readArchiveRequest(context.Background(), f.record, archive)
		if err != nil || !request.Legacy || request.ClaimID != "c" {
			t.Fatal("legacy claim scope was not normalized", request.ClaimID, err)
		}
		result, err := f.processor.readInvoiceArchiveEvidence(context.Background(), f.record, request, map[string]bool{})
		if claim == "c" && (err != nil || result["status"] != "FROZEN") {
			t.Fatal("legacy frozen evidence lost", result, err)
		}
		if claim != "c" && (err == nil || err.Error() != "ARCHIVE_INVOICE_CONTROL_SCOPE_INVALID") {
			t.Fatal("foreign legacy claim scope accepted", err)
		}
	}
}
