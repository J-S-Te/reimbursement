package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

// frozenArchiveReferencesFixture 建立一份已冻结来源及同原件同毫秒的无关来源。
// 参数：t 为本地内存 SQLite 归档测试上下文。
// 返回值：请求只含选定来源的归档场景，同原件另有其他票据来源。
func frozenArchiveReferencesFixture(t *testing.T) archiveStateFixture {
	t.Helper()
	f := newArchiveStateFixture(t)
	completed := f.cutoff.Add(-time.Minute)
	hash := sha256.Sum256([]byte("[]"))
	for _, name := range []string{"frozen", "sibling"} {
		run := store.InvoiceParseRun{ID: name + "-run", TenantID: "t", DocumentID: "detail-proof",
			JobID: name + "-job", Attempt: 1,
			Status: "COMPLETED", ResultHash: "result-hash", CreatedAt: completed, CompletedAt: &completed}
		source := store.InvoiceSource{ID: name + "-source", TenantID: "t", InvoiceID: name + "-invoice",
			DocumentID: "detail-proof", ParseRunID: run.ID, SourcesJSON: "[]",
			SourcesHash: hex.EncodeToString(hash[:]), CreatedAt: f.cutoff}
		for _, row := range []any{&run, &source} {
			if err := f.processor.DB.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	f.request["invoice_evidence"] = domain.ArchivedInvoiceEvidence{
		SourceIDs: []string{"frozen-source"}, Controls: []domain.ArchivedInvoiceControl{},
	}
	persistFrozenArchiveReferenceRequest(t, f)
	return f
}

// persistFrozenArchiveReferenceRequest 同步保存测试中的不可变任务及档案请求副本。
// 参数：t 为测试上下文，f 为携带待保存冻结请求的本地场景。
// 返回值：无；任一持久请求写入失败立即终止。
func persistFrozenArchiveReferenceRequest(t *testing.T, f archiveStateFixture) {
	t.Helper()
	raw, _ := archiveStateJSON(t, f.request)
	if err := f.processor.DB.Model(&store.Job{}).Where("id = ?", "job").Update("payload_json", raw).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.processor.DB.Model(&store.Archive{}).Where("id = ?", "archive").Update("manifest_json", raw).Error; err != nil {
		t.Fatal(err)
	}
}

// TestArchiveFrozenSourceExcludesSiblingAndSameTimeReferences 验证新请求的顶层来源和解析严格来自冻结集合。
// 参数：t 为单个本地归档封存场景。
// 返回值：无；同原件其他票据或同毫秒未选来源混入清单时失败。
func TestArchiveFrozenSourceExcludesSiblingAndSameTimeReferences(t *testing.T) {
	f := frozenArchiveReferencesFixture(t)
	if err := f.processor.archive(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
	var archive store.Archive
	if err := f.processor.DB.First(&archive, "id = ?", "archive").Error; err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Sources []archiveSourceRef   `json:"invoice_sources"`
		Runs    []archiveParseRunRef `json:"parse_runs"`
	}
	if err := json.Unmarshal([]byte(archive.ManifestJSON), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Sources) != 1 || manifest.Sources[0].ID != "frozen-source" ||
		len(manifest.Runs) != 1 || manifest.Runs[0].ID != "frozen-run" {
		t.Fatalf("归档混入未冻结来源或解析运行: sources=%+v runs=%+v", manifest.Sources, manifest.Runs)
	}
}

// TestArchiveFrozenSourceIgnoresUnrelatedReparse 验证未选取的重解析不会阻止旧证据归档。
// 参数：t 为单个本地归档封存场景。
// 返回值：无；同原件无关重解析在请求后完成导致封存失败时失败。
func TestArchiveFrozenSourceIgnoresUnrelatedReparse(t *testing.T) {
	f := frozenArchiveReferencesFixture(t)
	completed := f.cutoff.Add(time.Minute)
	run := store.InvoiceParseRun{ID: "unrelated-reparse", TenantID: "t", DocumentID: "detail-proof",
		JobID: "unrelated-job", Attempt: 1,
		Status: "COMPLETED", CreatedAt: f.cutoff.Add(-time.Minute), CompletedAt: &completed}
	if err := f.processor.DB.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.processor.archive(context.Background(), f.record); err != nil {
		t.Fatal("无关重解析阻止冻结来源封存", err)
	}
}

// TestArchiveFrozenSourceRejectsInvalidSelectedRun 验证精确集合不能绕过运行存在性、租户或原件核验。
// 参数：t 为逐项运行的本地归档边界场景。
// 返回值：无；选中运行缺失、越租户、错原件或尚未完成仍封存时失败。
func TestArchiveFrozenSourceRejectsInvalidSelectedRun(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
		code        string
	}{
		{"missing", "id", "removed-run", "ARCHIVE_INVOICE_PARSE_RUN_MISSING"},
		{"foreign", "tenant_id", "foreign", "ARCHIVE_INVOICE_PARSE_RUN_MISSING"},
		{"wrong-document", "document_id", "other-document", "ARCHIVE_PARSE_SOURCE_DOCUMENT_MISMATCH"},
		{"running", "status", "RUNNING", "ARCHIVE_PARSE_IN_PROGRESS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := frozenArchiveReferencesFixture(t)
			if err := f.processor.DB.Model(&store.InvoiceParseRun{}).Where("id = ?", "frozen-run").
				Update(tc.field, tc.value).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.processor.archive(context.Background(), f.record); err == nil || err.Error() != tc.code {
				t.Fatal("非法选中运行未被准确拒绝", err)
			}
		})
	}
}

// TestArchiveFrozenEmptySourcesRemainEmpty 验证显式空来源集合不回退为按原件查全部来源。
// 参数：t 为单个本地归档封存场景。
// 返回值：无；空集合被后续状态填充时失败。
func TestArchiveFrozenEmptySourcesRemainEmpty(t *testing.T) {
	f := frozenArchiveReferencesFixture(t)
	f.request["invoice_evidence"] = domain.ArchivedInvoiceEvidence{
		SourceIDs: []string{}, Controls: []domain.ArchivedInvoiceControl{},
	}
	persistFrozenArchiveReferenceRequest(t, f)
	if err := f.processor.archive(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
	var archive store.Archive
	if err := f.processor.DB.First(&archive, "id = ?", "archive").Error; err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Sources []archiveSourceRef   `json:"invoice_sources"`
		Runs    []archiveParseRunRef `json:"parse_runs"`
	}
	if err := json.Unmarshal([]byte(archive.ManifestJSON), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Sources) != 0 || len(manifest.Runs) != 0 {
		t.Fatal("空冻结来源被原件当前状态覆盖", manifest)
	}
}

// TestArchiveLegacySourceReferencesKeepExplicitFallback 验证缺省冻结字段的旧请求仍兼容且明确标注。
// 参数：t 为单个旧格式请求的本地封存场景。
// 返回值：无；旧解析历史丢失或未标注兼容回退时失败。
func TestArchiveLegacySourceReferencesKeepExplicitFallback(t *testing.T) {
	f := newArchiveStateFixture(t)
	if err := f.processor.archive(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
	var archive store.Archive
	if err := f.processor.DB.First(&archive, "id = ?", "archive").Error; err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Runs      []archiveParseRunRef `json:"parse_runs"`
		Fallbacks []string             `json:"legacy_fallbacks"`
		Evidence  struct {
			Status string `json:"status"`
		} `json:"invoice_evidence"`
	}
	if err := json.Unmarshal([]byte(archive.ManifestJSON), &manifest); err != nil {
		t.Fatal(err)
	}
	flag := false
	for _, value := range manifest.Fallbacks {
		if value == "LEGACY_INVOICE_REFERENCES_WITHOUT_FROZEN_SOURCE_IDS" {
			flag = true
		}
	}
	if !flag || manifest.Evidence.Status != "LEGACY_NOT_FROZEN" ||
		len(manifest.Runs) != 1 || manifest.Runs[0].ID != "parse-between" {
		t.Fatal("旧来源兼容历史或明确回退标记缺失", manifest)
	}
}
