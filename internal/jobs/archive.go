package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// archive 从批准快照及对应完整付款快照封存清单，逐件验证原件存在与哈希。
// 参数：ctx为归档超时；row的ResourceID是档案版本ID。
// 返回值：封存或失败；已SEALED版本保持不可变。
func (p *Processor) archive(ctx context.Context, row Record) (err error) {
	var archive store.Archive
	if err = p.DB.WithContext(ctx).Where("tenant_id = ? AND id = ?", row.TenantID, row.ResourceID).First(&archive).Error; err != nil {
		return err
	}
	if archive.Status == "SEALED" {
		return nil
	}
	defer func() {
		if err != nil {
			failureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = p.withLease(failureCtx, row, func(tx *gorm.DB) error {
				return tx.Model(&store.Archive{}).Where("id = ? AND tenant_id = ? AND status <> ?", archive.ID, row.TenantID, "SEALED").Update("status", "FAILED").Error
			})
		}
	}()
	request, err := p.readArchiveRequest(ctx, row, archive)
	if err != nil {
		return err
	}
	spend, err := p.readArchiveSpend(ctx, row.TenantID, archive.ClaimID, &request)
	if err != nil {
		return err
	}
	if archive.CreatedAt.IsZero() {
		return errors.New("ARCHIVE_REQUEST_TIME_MISSING")
	}
	var snapshot store.Snapshot
	if err = p.DB.WithContext(ctx).Where("tenant_id = ? AND claim_id = ? AND round = ?", row.TenantID, archive.ClaimID, spend.ApprovalRound).First(&snapshot).Error; err != nil {
		return err
	}
	if request.Legacy && request.ContentHash != "" && request.ContentHash != snapshot.ContentHash {
		return errors.New("ARCHIVE_REQUEST_CONTENT_HASH_MISMATCH")
	}
	if err = application.VerifyApprovalSnapshot(json.RawMessage(snapshot.ContentJSON), snapshot.ContentHash); err != nil {
		return err
	}
	var frozen struct {
		Claim    domain.Claim     `json:"claim"`
		Invoices []domain.Invoice `json:"invoices"`
	}
	if err = json.Unmarshal([]byte(snapshot.ContentJSON), &frozen); err != nil {
		return err
	}
	if frozen.Claim.ID != archive.ClaimID || !request.Legacy && (frozen.Claim.TenantID != row.TenantID || uint64(frozen.Claim.CurrentRound) != spend.ApprovalRound) {
		return errors.New("ARCHIVE_CLAIM_SNAPSHOT_MISSING")
	}
	files := map[string]bool{}
	if !request.Legacy {
		if err = collectEvidenceIDs(string(request.DocumentSnapshot), files); err != nil {
			return err
		}
	}
	if err = collectEvidenceIDs(snapshot.ContentJSON, files); err != nil {
		return err
	}
	invoices := []json.RawMessage{}
	invoiceMap := map[string]domain.Invoice{}
	for _, invoice := range frozen.Invoices {
		invoiceMap[invoice.ID] = invoice
	}
	for _, item := range frozen.Claim.Items {
		for _, usage := range item.InvoiceUsages {
			invoice, exists := invoiceMap[usage.InvoiceID]
			if !exists || len(invoice.FileIDs) == 0 {
				return errors.New("ARCHIVE_INVOICE_SOURCE_MISSING")
			}
			for _, id := range invoice.FileIDs {
				files[id] = true
			}
			raw, e := json.Marshal(invoice)
			if e != nil {
				return e
			}
			invoices = append(invoices, raw)
		}
	}
	history, e := p.archiveHistory(ctx, row.TenantID, archive.ClaimID, spend, archive.CreatedAt, request, files)
	if e != nil {
		return e
	}
	invoiceEvidence, err := p.readInvoiceArchiveEvidence(ctx, row, request, files)
	if err != nil {
		return err
	}
	if len(files) == 0 || len(files) > 100 {
		return errors.New("ARCHIVE_FILE_LIMIT_OR_MISSING")
	}
	if p.Files == nil {
		return errors.New("ARCHIVE_FILE_GATEWAY_UNAVAILABLE")
	}
	manifestFiles := []map[string]string{}
	documentIDs := []string{}
	for id := range files {
		var document store.Document
		if err = p.DB.WithContext(ctx).Where("tenant_id = ? AND file_id = ? AND status IN ?", row.TenantID, id, []string{"READY", "PARSED", "REVIEW_REQUIRED"}).First(&document).Error; err != nil {
			return errors.New("ARCHIVE_FILE_MISSING")
		}
		documentIDs = append(documentIDs, document.ID)
		bytes, err := p.Files.ReadDocument(ctx, row.TenantID, document.ID)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(bytes)
		manifestFiles = append(manifestFiles, map[string]string{"file_id": id, "document_id": document.ID, "sha256": hex.EncodeToString(hash[:])})
	}
	parseRuns, sourceRefs, e := p.archiveRequestedParseReferences(
		ctx, row.TenantID, documentIDs, archive.CreatedAt, &request)
	if e != nil {
		return e
	}
	sort.Slice(manifestFiles, func(i, j int) bool { return manifestFiles[i]["file_id"] < manifestFiles[j]["file_id"] })
	manifest := map[string]any{"claim_snapshot": json.RawMessage(snapshot.ContentJSON), "document_at_archive_request": request.DocumentSnapshot, "current_approval_round": request.CurrentApprovalRound, "archive_requested_at": archive.CreatedAt.UTC().Format(time.RFC3339Nano), "legacy_fallbacks": request.LegacyFallbacks, "approval_content_hash": snapshot.ContentHash, "payment_snapshot": spend, "invoices": invoices, "history": history, "parse_runs": parseRuns, "invoice_sources": sourceRefs, "files": manifestFiles, "package_version": archive.PackageVersion, "predecessor_package_id": request.PredecessorPackageID, "sealed_at": time.Now().UTC().Format(time.RFC3339Nano)}
	manifest["invoice_evidence"] = invoiceEvidence
	body, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(body) > 20<<20 {
		return errors.New("ARCHIVE_MANIFEST_LIMIT")
	}
	hash := sha256.Sum256(body)
	return p.withLease(ctx, row, func(tx *gorm.DB) error {
		result := tx.Model(&store.Archive{}).Where("tenant_id = ? AND id = ? AND status <> ?", row.TenantID, archive.ID, "SEALED").Updates(map[string]any{"status": "SEALED", "manifest_json": string(body), "manifest_hash": hex.EncodeToString(hash[:])})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var current store.Archive
			if err := tx.Where("tenant_id=? AND id=? AND status=?", row.TenantID, archive.ID, "SEALED").Take(&current).Error; err != nil {
				return errors.New("ARCHIVE_STATE_CHANGED")
			}
			return nil
		}
		return application.AppendWorkerAudit(tx, row.TenantID, archive.ClaimID, "archive.sealed", row.ID, "SUCCESS", "")
	})
}
