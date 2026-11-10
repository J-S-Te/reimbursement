package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
)

type archiveHistory struct {
	ExceptionFacts   []store.PaymentExceptionFact   `json:"payment_exception_facts"`
	ExceptionReviews []store.PaymentExceptionReview `json:"payment_exception_reviews"`
	ActionDetails    []store.ActionDetail           `json:"approval_action_details"`
	Snapshots        []store.Snapshot               `json:"approval_snapshots"`
	Actions          []store.Action                 `json:"approval_actions"`
	Payments         []store.Payment                `json:"payments"`
	Reversals        []store.Reversal               `json:"reversals"`
	ReversalRequests []store.ReversalRequest        `json:"reversal_requests"`
	InvoiceRevisions []store.InvoiceRevision        `json:"invoice_revisions"`
	Verifications    []store.Verification           `json:"verifications"`
}

// collectEvidenceIDs 收集冻结JSON明确证据字段并拒绝非字符串引用，不把外部URL当文件。
// 参数：raw为不可变财务、批准或请求JSON，files为本次原件核验集合。
// 返回值：JSON或引用格式错误；空列表保留为空，不推断额外证据。
func collectEvidenceIDs(raw string, files map[string]bool) error {
	var value any
	if len(raw) > 12<<20 || json.Unmarshal([]byte(raw), &value) != nil {
		return errors.New("ARCHIVE_EVIDENCE_INVALID")
	}
	return collectArchiveEvidenceNode(value, files, 0)
}

// collectArchiveEvidenceNode 有界遍历已冻结证据白名单，包含历史尾款专用字段。
// 参数：node为JSON节点，files为输出集合，depth为递归深度。
// 返回值：深度、引用形状或文件集合超限错误。
func collectArchiveEvidenceNode(node any, files map[string]bool, depth int) error {
	if depth > 64 {
		return errors.New("ARCHIVE_EVIDENCE_DEPTH_LIMIT")
	}
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "evidence_file_ids" || key == "voucher_file_ids" || key == "historical_tail_evidence_file_ids" || key == "no_invoice_evidence_file_ids" {
				if child == nil {
					continue
				}
				list, ok := child.([]any)
				if !ok {
					return errors.New("ARCHIVE_EVIDENCE_INVALID")
				}
				for _, entry := range list {
					id, ok := entry.(string)
					if !ok || validateArchiveIDs([]string{id}) != nil {
						return errors.New("ARCHIVE_EVIDENCE_INVALID")
					}
					files[id] = true
					if len(files) > 100 {
						return errors.New("ARCHIVE_FILE_LIMIT_OR_MISSING")
					}
				}
			} else if err := collectArchiveEvidenceNode(child, files, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := collectArchiveEvidenceNode(child, files, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// archiveHistory 以不可变效果ID关联财务原件，并保留所有已发生审批及票面证据。
// 参数：ctx为任务边界；tenant/claimID为归属；spend为该档案对应完整快照；cutoff为归档请求时刻，request为冻结ID和当前轮次，files为待核验集合。
// 返回值：完整历史清单或缺失记录错误，不仅依赖时间戳猜测付款与冲正集合。
func (p *Processor) archiveHistory(ctx context.Context, tenant, claimID string, spend domain.SpendSnapshot, cutoff time.Time, request archiveFrozenRequest, files map[string]bool) (archiveHistory, error) {
	history := archiveHistory{}
	db := p.DB.WithContext(ctx)
	var historyBytes int64
	paymentIDs, reversalIDs := []string{}, []string{}
	for _, effect := range spend.Effects {
		paymentIDs = append(paymentIDs, effect.PaymentRecordID)
		if effect.Kind == "PAYMENT_FULLY_REVERSED" {
			reversalIDs = append(reversalIDs, effect.EffectID)
		}
	}
	for _, entry := range []struct {
		model any
		field string
		ids   []string
	}{{&store.Snapshot{}, "", nil}, {&store.Payment{}, "id", paymentIDs}, {&store.Reversal{}, "id", reversalIDs}} {
		query := db.Model(entry.model).Where("tenant_id=? AND claim_id=?", tenant, claimID)
		if entry.field == "" {
			query = query.Where("round<=?", request.CurrentApprovalRound)
		} else {
			if len(entry.ids) == 0 {
				continue
			}
			query = query.Where("id IN ?", entry.ids)
		}
		var size int64
		if err := query.Select("COALESCE(SUM(LENGTH(content_json)),0)").Scan(&size).Error; err != nil {
			return history, err
		}
		historyBytes += size
		if historyBytes > 12<<20 {
			return history, errors.New("ARCHIVE_MANIFEST_LIMIT")
		}
	}

	invoiceIDs := map[string]bool{}
	if err := db.Where("tenant_id = ? AND claim_id = ? AND round <= ?", tenant, claimID, request.CurrentApprovalRound).Order("round ASC").Limit(101).Find(&history.Snapshots).Error; err != nil {
		return history, err
	}
	if len(history.Snapshots) == 0 || len(history.Snapshots) > 100 || uint64(len(history.Snapshots)) != request.CurrentApprovalRound {
		return history, errors.New("ARCHIVE_APPROVAL_HISTORY_LIMIT")
	}
	for index, snapshot := range history.Snapshots {
		if snapshot.Round != index+1 {
			return history, errors.New("ARCHIVE_APPROVAL_ROUND_MISSING")
		}
		if err := application.VerifyApprovalSnapshot(json.RawMessage(snapshot.ContentJSON), snapshot.ContentHash); err != nil {
			return history, err
		}
		var frozen struct {
			Claim    domain.Claim     `json:"claim"`
			Invoices []domain.Invoice `json:"invoices"`
		}
		if json.Unmarshal([]byte(snapshot.ContentJSON), &frozen) != nil {
			return history, errors.New("ARCHIVE_APPROVAL_HISTORY_INVALID")
		}
		if !request.Legacy && (frozen.Claim.ID != claimID || frozen.Claim.TenantID != tenant || frozen.Claim.CurrentRound != snapshot.Round || snapshot.CreatedAt.After(cutoff)) {
			return history, errors.New("ARCHIVE_APPROVAL_HISTORY_SCOPE_MISMATCH")
		}
		if !request.Legacy && uint64(snapshot.Round) == request.CurrentApprovalRound && snapshot.ContentHash != request.ContentHash {
			return history, errors.New("ARCHIVE_CURRENT_APPROVAL_HASH_MISMATCH")
		}
		if err := collectEvidenceIDs(snapshot.ContentJSON, files); err != nil {
			return history, err
		}
		for _, invoice := range frozen.Invoices {
			invoiceIDs[invoice.ID] = true
			for _, id := range invoice.FileIDs {
				files[id] = true
			}
		}
	}
	if err := archivePinnedHistory(db, tenant, claimID, request, cutoff, &history, files, &historyBytes); err != nil {
		return history, err
	}
	payments := map[string]bool{}
	approvedHashes := map[string]bool{}
	for _, snapshot := range history.Snapshots {
		approvedHashes[snapshot.ContentHash] = true
	}
	for _, effect := range spend.Effects {
		if (!request.Legacy || effect.ApprovedContentHash != "") && !approvedHashes[effect.ApprovedContentHash] {
			return history, errors.New("ARCHIVE_EFFECT_APPROVAL_HASH_MISMATCH")
		}
		if !payments[effect.PaymentRecordID] {
			var payment store.Payment
			if err := db.Where("tenant_id = ? AND claim_id = ? AND id = ?", tenant, claimID, effect.PaymentRecordID).Take(&payment).Error; err != nil {
				return history, errors.New("ARCHIVE_PAYMENT_FACT_MISSING")
			}
			if !request.Legacy {
				var frozenPayment application.PaymentResult
				if json.Unmarshal([]byte(payment.ContentJSON), &frozenPayment) != nil || frozenPayment.ID != payment.ID || frozenPayment.ClaimID != claimID || frozenPayment.ContentHash != effect.ApprovedContentHash || len(frozenPayment.VoucherFileIDs) == 0 {
					return history, errors.New("ARCHIVE_PAYMENT_CONTENT_MISMATCH")
				}
			}
			history.Payments = append(history.Payments, payment)
			payments[payment.ID] = true
			if err := collectEvidenceIDs(payment.ContentJSON, files); err != nil {
				return history, err
			}
		}
		if effect.Kind != "PAYMENT_FULLY_REVERSED" {
			continue
		}
		var reversal store.Reversal
		if err := db.Where("tenant_id = ? AND claim_id = ? AND id = ? AND payment_id = ?", tenant, claimID, effect.EffectID, effect.PaymentRecordID).Take(&reversal).Error; err != nil {
			return history, errors.New("ARCHIVE_REVERSAL_FACT_MISSING")
		}
		history.Reversals = append(history.Reversals, reversal)
		if err := collectEvidenceIDs(reversal.ContentJSON, files); err != nil {
			return history, err
		}
		var frozen struct {
			Request application.ReversalData `json:"request"`
		}
		if json.Unmarshal([]byte(reversal.ContentJSON), &frozen) != nil || frozen.Request.ID == "" || len(frozen.Request.EvidenceFileIDs) == 0 {
			return history, errors.New("ARCHIVE_REVERSAL_EVIDENCE_MISSING")
		}
		var request store.ReversalRequest
		if err := db.Where("tenant_id = ? AND id = ? AND payment_id = ?", tenant, frozen.Request.ID, effect.PaymentRecordID).Take(&request).Error; err != nil {
			return history, errors.New("ARCHIVE_REVERSAL_APPROVAL_MISSING")
		}
		historyBytes += int64(len(request.ContentJSON))
		if historyBytes > 12<<20 {
			return history, errors.New("ARCHIVE_MANIFEST_LIMIT")
		}
		history.ReversalRequests = append(history.ReversalRequests, request)
	}
	if err := verifyArchiveExceptionLinks(history, spend); err != nil {
		return history, err
	}
	ids := make([]string, 0, len(invoiceIDs))
	for id := range invoiceIDs {
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		for _, model := range []any{&store.InvoiceRevision{}, &store.Verification{}} {
			var size int64
			if err := db.Model(model).Select("COALESCE(SUM(LENGTH(content_json)),0)").Where("tenant_id = ? AND invoice_id IN ? AND created_at <= ?", tenant, ids, cutoff).Scan(&size).Error; err != nil {
				return history, err
			}
			historyBytes += size
			if historyBytes > 12<<20 {
				return history, errors.New("ARCHIVE_MANIFEST_LIMIT")
			}
		}

		if err := db.Where("tenant_id = ? AND invoice_id IN ? AND created_at <= ?", tenant, ids, cutoff).Order("created_at,id").Limit(2001).Find(&history.InvoiceRevisions).Error; err != nil {
			return history, err
		}
		if err := db.Where("tenant_id = ? AND invoice_id IN ? AND created_at <= ?", tenant, ids, cutoff).Order("created_at,id").Limit(2001).Find(&history.Verifications).Error; err != nil {
			return history, err
		}
		if len(history.InvoiceRevisions) > 2000 || len(history.Verifications) > 2000 {
			return history, errors.New("ARCHIVE_INVOICE_HISTORY_LIMIT")
		}
		for _, verification := range history.Verifications {
			if err := collectEvidenceIDs(verification.ContentJSON, files); err != nil {
				return history, err
			}
		}
	}
	return history, nil
}

type archiveParseRunRef struct {
	ID            string     `json:"run_id"`
	DocumentID    string     `json:"document_id"`
	ParserVersion string     `json:"parser_version"`
	RulesVersion  string     `json:"rules_version"`
	InputSHA256   string     `json:"input_sha256"`
	Status        string     `json:"status"`
	ResultHash    string     `json:"result_hash"`
	FailureCode   string     `json:"failure_code"`
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at"`
}
type archiveSourceRef struct {
	ID             string `json:"source_id"`
	InvoiceID      string `json:"invoice_id"`
	DocumentID     string `json:"document_id"`
	ParseRunID     string `json:"parse_run_id"`
	CandidateIndex int    `json:"candidate_index"`
	SourcesHash    string `json:"sources_hash"`
}

// archiveParseReferences 保存不可变解析和来源的ID及摘要，清单不复制全部票面原文。
// 参数：ctx为任务边界；tenant/documents为已校验文件归属；cutoff为归档请求资料截止时刻。
// 返回值：解析运行及来源清单；进行中或超限记录拒绝封存。
func (p *Processor) archiveParseReferences(ctx context.Context, tenant string, documents []string, cutoff time.Time) ([]archiveParseRunRef, []archiveSourceRef, error) {
	runs := []archiveParseRunRef{}
	sources := []archiveSourceRef{}
	db := p.DB.WithContext(ctx)
	if err := db.Model(&store.InvoiceParseRun{}).Select("id,document_id,parser_version,rules_version,input_sha256,status,result_hash,failure_code,created_at,completed_at").Where("tenant_id = ? AND document_id IN ? AND created_at <= ?", tenant, documents, cutoff).Order("created_at,id").Limit(1001).Scan(&runs).Error; err != nil {
		return nil, nil, err
	}
	if len(runs) > 1000 {
		return nil, nil, errors.New("ARCHIVE_PARSE_HISTORY_LIMIT")
	}
	for _, run := range runs {
		if run.CompletedAt != nil && run.CompletedAt.After(cutoff) {
			return nil, nil, errors.New("ARCHIVE_PARSE_COMPLETED_AFTER_REQUEST")
		}
		if run.Status == "RUNNING" {
			return nil, nil, errors.New("ARCHIVE_PARSE_IN_PROGRESS")
		}
	}
	if err := db.Model(&store.InvoiceSource{}).Select("id,invoice_id,document_id,parse_run_id,candidate_index,sources_hash").Where("tenant_id = ? AND document_id IN ? AND created_at <= ?", tenant, documents, cutoff).Order("created_at,id").Limit(1001).Scan(&sources).Error; err != nil {
		return nil, nil, err
	}
	if len(sources) > 1000 {
		return nil, nil, errors.New("ARCHIVE_SOURCE_HISTORY_LIMIT")
	}
	for _, source := range sources {
		if source.SourcesHash == "" {
			return nil, nil, errors.New("ARCHIVE_SOURCE_HASH_MISSING")
		}
	}
	return runs, sources, nil
}
