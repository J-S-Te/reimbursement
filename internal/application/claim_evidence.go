package application

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"sort"
)

// claimEvidenceIDs 收集本单显式历史尾款和行程人工确认证据，不截断引用。
// 参数：c 为费用单据。
// 返回值：稳定去重证据集合。
func claimEvidenceIDs(c domain.Claim) []string {
	seen := map[string]bool{}
	for _, item := range c.Items {
		for _, id := range append(append([]string{}, item.HistoricalTailEvidenceFileIDs...), item.NoInvoiceEvidenceFileIDs...) {
			seen[id] = true
		}
		if item.TravelConfirmation != nil {
			for _, id := range item.TravelConfirmation.EvidenceFileIDs {
				seen[id] = true
			}
		}
	}
	out := []string{}
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// validateClaimEvidence 在事务外核验所有附件属于申请人且用途精确为费用证据。
// 参数：ctx 为请求边界，c 为已重算单据。
// 返回值：归属、用途或外部READY检查错误。
func (s *Service) validateClaimEvidence(ctx context.Context, c domain.Claim) error {
	ids := claimEvidenceIDs(c)
	if len(ids) == 0 {
		return nil
	}
	if s.deps.Files == nil {
		return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "费用证据文件服务未配置", 503)
	}
	actor := domain.Actor{TenantID: c.TenantID, SubjectID: c.Applicant}
	for start := 0; start < len(ids); start += 20 {
		end := start + 20
		if end > len(ids) {
			end = len(ids)
		}
		if e := s.deps.Files.Validate(ctx, actor, ids[start:end], "reimbursement.claim.evidence"); e != nil {
			return e
		}
	}
	return nil
}

// authorizeClaimEvidenceUpload 只允许本人草稿更新或提交能力上传费用证据。
// 参数：a 为可信当前身份。
// 返回值：无相关能力时拒绝，不附带审批或付款权限。
func authorizeClaimEvidenceUpload(a domain.Actor) error {
	if a.Has("expense.claim.update") {
		return require(a, "expense.claim.update")
	}
	return require(a, "expense.claim.submit")
}

// DownloadClaimEvidence 验证显式附件归属及当前整单或完整费用责任后签发下载票据。
// 参数：ctx 为上下文，a 为身份，claimID/fileID 为已绑定费用与证据。
// 返回值：一次性网关回执，混合范围共享文件不自动开放。
func (s *Service) DownloadClaimEvidence(ctx context.Context, a domain.Actor, claimID, fileID string) (json.RawMessage, error) {
	if e := require(a, "expense.claim.read"); e != nil {
		return nil, e
	}
	db := s.db.WithContext(ctx)
	c, _, e := loadClaim(db, a.TenantID, claimID, false)
	if e != nil {
		return nil, e
	}
	if e = s.canRead(db, a, c); e != nil {
		return nil, e
	}
	found := false
	for _, id := range claimEvidenceIDs(c) {
		found = found || id == fileID
	}
	if !found {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "费用没有绑定此证据", 404)
	}
	var doc store.Document
	if e = db.Where("tenant_id = ? AND file_id = ? AND owner = ? AND status IN ?", c.TenantID, fileID, c.Applicant, []string{"READY", "PARSED", "REVIEW_REQUIRED"}).First(&doc).Error; e != nil {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "证据文件不存在或归属不正确", 404)
	}
	var metadata struct {
		Purpose string `json:"purpose"`
	}
	if e = json.Unmarshal([]byte(doc.ContentJSON), &metadata); e != nil {
		return nil, e
	}
	if metadata.Purpose != "reimbursement.claim.evidence" {
		return nil, domain.Fail("RESOURCE_NOT_FOUND", "证据用途不匹配", 404)
	}
	full, e := s.fullRead(db, a, c)
	if e != nil {
		return nil, e
	}
	if !full {
		allowed, err := taskCoversEvidence(db, a, c, fileID)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, domain.Fail("RESOURCE_NOT_FOUND", "共享证据包含范围外费用，需完整财务权限", 404)
		}
	}
	if s.deps.Files == nil {
		return nil, domain.Fail("FILE_GATEWAY_UNAVAILABLE", "费用证据文件服务未配置", 503)
	}
	return s.deps.Files.Download(ctx, a, fileID)
}

// taskCoversEvidence 要求当前受理人的责任集合覆盖该文件所关联的全部费用分摊。
// 参数：db 为数据库，a 为身份，c 为单据，fileID 为共享证据。
// 返回值：完整覆盖标志与读取错误。
func taskCoversEvidence(db *gorm.DB, a domain.Actor, c domain.Claim, fileID string) (bool, error) {
	var rows []store.Task
	if e := db.Where("tenant_id = ? AND claim_id = ? AND round = ? AND assignee = ?", c.TenantID, c.ID, c.CurrentRound, a.SubjectID).Find(&rows).Error; e != nil {
		return false, e
	}
	keys := map[string]bool{}
	for _, row := range rows {
		var task domain.Task
		if e := json.Unmarshal([]byte(row.ContentJSON), &task); e != nil {
			return false, e
		}
		for _, key := range task.AllocationKeys {
			keys[key] = true
		}
	}
	used := false
	for _, item := range c.Items {
		if !itemHasEvidence(item, fileID) {
			continue
		}
		used = true
		for _, allocation := range item.Allocations {
			if !keys["*"] && !keys[item.ID+"/"+allocation.ProjectID+"/"+allocation.ServiceItemID] {
				return false, nil
			}
		}
	}
	return used, nil
}

// itemHasEvidence 判定一项费用是否显式引用给定费用证明。
// 参数：item 为费用，fileID 为当前证明。
// 返回值：是否存在绑定。
func itemHasEvidence(item domain.Item, fileID string) bool {
	for _, id := range append(append([]string{}, item.HistoricalTailEvidenceFileIDs...), item.NoInvoiceEvidenceFileIDs...) {
		if id == fileID {
			return true
		}
	}
	if item.TravelConfirmation != nil {
		for _, id := range item.TravelConfirmation.EvidenceFileIDs {
			if id == fileID {
				return true
			}
		}
	}
	return false
}
