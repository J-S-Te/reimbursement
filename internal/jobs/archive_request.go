package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

type archiveFrozenRequest struct {
	ClaimID                 string                          `json:"claim_id"`
	ProjectionRevision      uint64                          `json:"projection_revision"`
	ContentHash             string                          `json:"content_hash"`
	PredecessorPackageID    string                          `json:"predecessor_package_id,omitempty"`
	PackageID               string                          `json:"package_id"`
	PackageVersion          int64                           `json:"package_version"`
	CurrentApprovalRound    uint64                          `json:"current_approval_round"`
	DocumentSnapshot        json.RawMessage                 `json:"document_snapshot"`
	ExceptionFactIDs        []string                        `json:"exception_fact_ids"`
	ExceptionReviewIDs      []string                        `json:"exception_review_ids"`
	ClaimActionIDs          []string                        `json:"claim_action_ids"`
	ApprovalActionDetailIDs []string                        `json:"approval_action_detail_ids"`
	InvoiceEvidence         *domain.ArchivedInvoiceEvidence `json:"invoice_evidence,omitempty"`
	Legacy                  bool                            `json:"-"`
	LegacyFallbacks         []string                        `json:"-"`
}

// readArchiveRequest 读取请求时冻结载荷并验证包、单据、财务版本和当前文档的一致性。
// 参数：ctx为任务边界，row为租约任务，archive为尚未封存版本。
// 返回值：精确历史集合；只有旧无现代字段载荷才允许带明确标注的兼容回退。
func (p *Processor) readArchiveRequest(ctx context.Context, row Record, archive store.Archive) (archiveFrozenRequest, error) {
	request := archiveFrozenRequest{}
	var job store.Job
	if err := p.DB.WithContext(ctx).Where("tenant_id=? AND id=? AND resource_id=?", row.TenantID, row.ID, archive.ID).Take(&job).Error; err != nil {
		return request, err
	}
	if len(job.PayloadJSON) > 4<<20 || json.Unmarshal([]byte(job.PayloadJSON), &request) != nil {
		return request, errors.New("ARCHIVE_REQUEST_INVALID")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(job.PayloadJSON), &fields) != nil || fields == nil {
		return request, errors.New("ARCHIVE_REQUEST_INVALID")
	}
	modern := false
	for _, key := range []string{"document_snapshot", "package_id", "package_version", "current_approval_round", "exception_fact_ids", "exception_review_ids", "claim_action_ids", "approval_action_detail_ids"} {
		if _, exists := fields[key]; exists {
			modern = true
		}
	}
	if !modern {
		if request.ClaimID != "" && request.ClaimID != archive.ClaimID {
			return request, errors.New("ARCHIVE_REQUEST_DOCUMENT_MISMATCH")
		}
		request.ClaimID = archive.ClaimID
		request.Legacy = true
		request.LegacyFallbacks = []string{"LEGACY_ARCHIVE_REQUEST_WITHOUT_FROZEN_DOCUMENT_AND_IDS"}
		if request.ProjectionRevision == 0 {
			if archive.PackageVersion < 1 {
				return request, errors.New("ARCHIVE_REQUEST_INVALID")
			}
			request.ProjectionRevision = uint64(archive.PackageVersion)
			request.LegacyFallbacks = append(request.LegacyFallbacks, "LEGACY_PACKAGE_VERSION_AS_PROJECTION_REVISION")
		}
		return request, nil
	}
	if strings.TrimSpace(archive.ManifestJSON) != strings.TrimSpace(job.PayloadJSON) {
		return request, errors.New("ARCHIVE_REQUEST_CONFLICT")
	}
	for _, key := range []string{"claim_id", "projection_revision", "content_hash", "package_id", "package_version", "current_approval_round", "document_snapshot"} {
		if _, exists := fields[key]; !exists {
			return request, errors.New("ARCHIVE_REQUEST_FIELDS_MISSING")
		}
	}
	for _, key := range []string{"exception_fact_ids", "exception_review_ids", "claim_action_ids", "approval_action_detail_ids"} {
		raw, exists := fields[key]
		if !exists {
			return request, errors.New("ARCHIVE_REQUEST_REFERENCE_SET_MISSING")
		}
		var ids []string
		if string(raw) == "null" || json.Unmarshal(raw, &ids) != nil || ids == nil {
			return request, errors.New("ARCHIVE_REQUEST_REFERENCE_SET_INVALID")
		}
		if err := validateArchiveIDs(ids); err != nil {
			return request, err
		}
	}
	var document domain.Claim
	if json.Unmarshal(request.DocumentSnapshot, &document) != nil || request.ClaimID != archive.ClaimID || request.PackageID != archive.ID || request.PackageVersion != archive.PackageVersion || request.ProjectionRevision == 0 || request.CurrentApprovalRound == 0 || request.CurrentApprovalRound > 100 || document.ID != archive.ClaimID || document.TenantID != row.TenantID || document.CurrentRound != int(request.CurrentApprovalRound) || document.ProjectionRevision < 1 || uint64(document.ProjectionRevision) != request.ProjectionRevision || document.ContentHash != request.ContentHash {
		return request, errors.New("ARCHIVE_REQUEST_DOCUMENT_MISMATCH")
	}
	if request.PredecessorPackageID != "" {
		var parent store.Archive
		if err := p.DB.WithContext(ctx).Where("tenant_id=? AND claim_id=? AND id=? AND package_version < ? AND status=?", row.TenantID, archive.ClaimID, request.PredecessorPackageID, archive.PackageVersion, "FAILED").Take(&parent).Error; err != nil {
			return request, errors.New("ARCHIVE_PREDECESSOR_INVALID")
		}
	}
	hash, err := hex.DecodeString(request.ContentHash)
	if err != nil || len(hash) != 32 {
		return request, errors.New("ARCHIVE_REQUEST_CONTENT_HASH_INVALID")
	}
	return request, nil
}

// validateArchiveIDs 校验精确引用集合的大小、唯一性和稳定标识，显式空集合保持为空。
// 参数：ids为请求冻结的某类历史ID。
// 返回值：超限、重复或非法标识错误，不去重掩盖请求冲突。
func validateArchiveIDs(ids []string) error {
	if len(ids) > 2000 {
		return errors.New("ARCHIVE_REFERENCE_LIMIT")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || len(id) > 128 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\x00\r\n") || seen[id] {
			return errors.New("ARCHIVE_REFERENCE_ID_INVALID")
		}
		seen[id] = true
	}
	return nil
}

// readArchiveSpend 按冻结财务版本读取不可变投影，不把包版本当资金版本。
// 参数：ctx为任务边界，tenant/claim为归属，request为已校验冻结请求。
// 返回值：经过协议及归属验证的财务快照；旧简化历史明确记录兼容边界。
func (p *Processor) readArchiveSpend(ctx context.Context, tenant, claim string, request *archiveFrozenRequest) (domain.SpendSnapshot, error) {
	var source store.Outbox
	var spend domain.SpendSnapshot
	if err := p.DB.WithContext(ctx).Where("tenant_id = ? AND resource_id = ? AND topic IN ? AND JSON_EXTRACT(payload_json,'$.projection_revision') = ?", tenant, claim, []string{"project", "project_spend"}, request.ProjectionRevision).Order("created_at,id").Take(&source).Error; err != nil {
		return spend, errors.New("ARCHIVE_PAYMENT_SNAPSHOT_MISSING")
	}
	if len(source.PayloadJSON) > domain.SpendSnapshotMaxBytes || json.Unmarshal([]byte(source.PayloadJSON), &spend) != nil || spend.ClaimID != claim || spend.ProjectionRevision != request.ProjectionRevision || spend.ApprovalRound == 0 || spend.TenantID != "" && spend.TenantID != tenant || spend.SourceApplication != "" && spend.SourceApplication != "reimbursement" {
		return spend, errors.New("ARCHIVE_PAYMENT_SNAPSHOT_INVALID")
	}
	if !request.Legacy {
		if spend.TenantID != tenant || domain.ValidateSpendSnapshot(spend) != nil {
			return spend, errors.New("ARCHIVE_PAYMENT_SNAPSHOT_INVALID")
		}
	} else {
		request.LegacyFallbacks = append(request.LegacyFallbacks, "LEGACY_SPEND_PROTOCOL_NOT_FULLY_VALIDATED")
	}
	if request.CurrentApprovalRound == 0 {
		request.CurrentApprovalRound = spend.ApprovalRound
		request.LegacyFallbacks = append(request.LegacyFallbacks, "LEGACY_APPROVAL_ROUND_FROM_PAYMENT")
	}
	if request.CurrentApprovalRound < spend.ApprovalRound || request.CurrentApprovalRound > 100 {
		return spend, errors.New("ARCHIVE_APPROVAL_ROUND_INVALID")
	}
	return spend, nil
}

// verifyArchiveContentHash 按应用同口径复算不可变异常事实摘要。
// 参数：value为已解码领域事实，expected为持久化摘要。
// 返回值：内容与摘要是否完全匹配。
func verifyArchiveContentHash(value any, expected string) bool {
	raw, err := json.Marshal(value)
	if err != nil || expected == "" {
		return false
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]) == expected
}
