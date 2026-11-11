package application

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// frozenCostItems 提取批准费用逐项分摊并核对金额守恒，避免项目汇总丢失费用身份。
// 参数：c为冻结审批单据，hash为该批准内容摘要。
// 返回值：稳定排序的最小费用行；缺身份、日期或守恒失败返回错误。
func frozenCostItems(c domain.Claim, hash string) ([]domain.CostItem, error) {
	rows := []domain.CostItem{}
	seen := map[string]bool{}
	for _, item := range c.Items {
		if item.ID == "" || (item.ExpenseNature != "ACTUAL" && item.ExpenseNature != "PREPAYMENT") || seen[item.ID] || item.Requested <= 0 {
			return nil, domain.ErrSpendProtocol
		}
		seen[item.ID] = true
		if _, err := time.Parse("2006-01-02", item.OccurredDate); err != nil {
			return nil, domain.ErrSpendProtocol
		}
		evidence, _ := json.Marshal(struct {
			Invoices []domain.InvoiceUsage
			Files    []string
		}{item.InvoiceUsages, item.NoInvoiceEvidenceFileIDs})
		digest := sha256.Sum256(evidence)
		var sum int64
		allocations := map[string]bool{}
		for _, allocation := range item.Allocations {
			raw, _ := json.Marshal([]string{allocation.ProjectID, allocation.ServiceItemID})
			key := sha256.Sum256(raw)
			stable := hex.EncodeToString(key[:])
			amount := int64(allocation.Amount)
			if allocation.ProjectID == "" || amount <= 0 || allocations[stable] || amount > int64(item.Requested)-sum {
				return nil, domain.ErrSpendProtocol
			}
			allocations[stable] = true
			sum += amount
			rows = append(rows, domain.CostItem{OrganizationID: c.OrgID, Applicant: c.Applicant, ClaimType: c.ClaimType, ApprovedContentHash: hash, ItemID: item.ID, AllocationKey: stable, ProjectID: allocation.ProjectID, ServiceItemID: allocation.ServiceItemID, ExpenseNature: item.ExpenseNature, FeeType: item.FeeType, OccurredDate: item.OccurredDate, AmountMinor: amount, EvidenceDigest: hex.EncodeToString(digest[:])})
		}
		if sum != int64(item.Requested) {
			return nil, domain.ErrSpendProtocol
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].ItemID+rows[i].AllocationKey < rows[j].ItemID+rows[j].AllocationKey
	})
	return rows, nil
}

// CostCandidates 在同一只读RR事务读取完整付款历史及对应批准快照。
// 参数：ctx控制读取边界，tenant来自验签身份，claimID可精确筛选单据。
// 返回值：有界完整候选；超限、缺数据库或损坏事实返回明确错误。
func (s *Service) CostCandidates(ctx context.Context, tenant, claimID string) (domain.CostSnapshot, error) {
	out := domain.CostSnapshot{SchemaVersion: 1, SourceApplication: "reimbursement", SourceEnvironment: s.deps.Environment, TenantID: tenant, SnapshotAt: time.Now().UTC().Format(time.RFC3339Nano), Complete: false, Claims: []domain.CostClaim{}}
	if s.db == nil || tenant == "" || out.SourceEnvironment == "" {
		return out, domain.Fail("COST_SOURCE_UNAVAILABLE", "成本候选来源未配置", 503)
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		out.SnapshotAt = time.Now().UTC().Format(time.RFC3339Nano)
		var claims []store.Claim
		q := tx.Where("tenant_id = ? AND EXISTS (SELECT 1 FROM exp_payment_effect e WHERE e.claim_id=exp_claim.id AND e.tenant_id=exp_claim.tenant_id)", tenant)
		if claimID != "" {
			q = q.Where("id = ?", claimID)
		}
		if err := q.Order("id").Limit(1001).Find(&claims).Error; err != nil {
			return err
		}
		if len(claims) > 1000 {
			return domain.Fail("COST_SOURCE_LIMIT", "完整候选超过1000单，请缩小业务范围", 503)
		}
		count := 0
		for _, record := range claims {
			var c domain.Claim
			if json.Unmarshal([]byte(record.ContentJSON), &c) != nil || c.TenantID != tenant || c.ID != record.ID {
				return domain.ErrSpendProtocol
			}
			item, err := s.costClaim(tx, c)
			if err != nil {
				return err
			}
			count += len(item.FrozenItems)
			if count > 10000 {
				return domain.Fail("COST_SOURCE_LIMIT", "完整候选超过10000分摊行", 503)
			}
			out.Claims = append(out.Claims, item)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if len(raw) > (8<<20)-64 {
			return domain.Fail("COST_SOURCE_LIMIT", "完整候选超过8MiB", 503)
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err == nil {
		out.Complete = true
	}
	return out, err
}

// costClaim 关联付款效果的批准摘要并保留历史未知，禁止使用当前草稿替代批准费用。
// 参数：tx为只读事务，c为同租户单据元数据。
// 返回值：单据完整付款历史与冻结费用；损坏事实返回错误。
func (s *Service) costClaim(tx *gorm.DB, c domain.Claim) (domain.CostClaim, error) {
	out := domain.CostClaim{ClaimID: c.ID, ClaimNo: c.ClaimNo, DocumentRevision: c.Version, ApprovalRound: c.CurrentRound, ProjectionRevision: c.ProjectionRevision, Currency: c.Currency, Applicant: c.Applicant, OrganizationID: c.OrgID, ClaimType: c.ClaimType, Status: "KNOWN", FrozenItems: []domain.CostItem{}}
	effects, err := readSpendEffects(tx, c)
	if err != nil {
		return out, err
	}
	out.Effects = effects
	seen := map[string]bool{}
	for _, effect := range effects {
		hash := effect.ApprovedContentHash
		if seen[hash] {
			continue
		}
		seen[hash] = true
		var rows []store.Snapshot
		if err = tx.Where("tenant_id = ? AND claim_id = ? AND content_hash = ?", c.TenantID, c.ID, hash).Order("round DESC").Limit(1).Find(&rows).Error; err != nil {
			return out, err
		}
		if len(rows) == 0 {
			out.Status = "UNKNOWN"
			out.UnknownReason = "缺少付款对应批准快照"
			continue
		}
		var frozen approvalSnapshot
		if json.Unmarshal([]byte(rows[0].ContentJSON), &frozen) != nil || frozen.Claim.ID != c.ID || frozen.Claim.TenantID != c.TenantID {
			return out, domain.ErrSpendProtocol
		}
		actual, e := approvedContentHash(frozen.Claim, frozen.Approval, frozen.Invoices)
		if e != nil || actual != hash {
			return out, domain.ErrSpendProtocol
		}
		items, e := frozenCostItems(frozen.Claim, hash)
		if e != nil {
			out.Status = "UNKNOWN"
			out.UnknownReason = "历史费用缺稳定条目或发生日"
			continue
		}
		out.Applicant = frozen.Claim.Applicant
		out.OrganizationID = frozen.Claim.OrgID
		out.ClaimType = frozen.Claim.ClaimType
		out.FrozenItems = append(out.FrozenItems, items...)
	}
	return out, nil
}
