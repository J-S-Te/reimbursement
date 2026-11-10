package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

type borrowDirectory struct{ revoked, unavailable bool }

// VerifyApprover 模拟目录实时撤权，申请快照不能代替当前目录能力。
// 参数：ctx 为上下文，a 为操作者，target 为目标，permission 为当前待核能力。
// 返回值：已撤权时明确拒绝，否则确认当前能力。
func (d *borrowDirectory) VerifyApprover(ctx context.Context, a domain.Actor, target, permission string) error {
	if d.unavailable {
		return domain.Fail("PERSONNEL_UNAVAILABLE", "当前人员目录不可用", 503)
	}
	if d.revoked && target == "exporter" {
		return domain.Fail("APPROVER_INELIGIBLE", "当前已撤权", 422)
	}
	return nil
}

// borrowFixture 建立不可变真实清单摘要及不具导出能力的申请人。
// 参数：t 为测试上下文。
// 返回值：服务、临时库、申请人、独立审批人、档案和可撤权目录。
func borrowFixture(t *testing.T) (*Service, *gorm.DB, domain.Actor, domain.Actor, store.Archive, *borrowDirectory) {
	t.Helper()
	s, db, _ := setupWorkflow(t)
	_, archive := archivePackageClaim(t, db)
	sum := sha256.Sum256([]byte(archive.ManifestJSON))
	archive.ManifestHash = hex.EncodeToString(sum[:])
	if e := db.Model(&store.Archive{}).Where("id = ?", archive.ID).Update("manifest_hash", archive.ManifestHash).Error; e != nil {
		t.Fatal(e)
	}
	requester := domain.Actor{TenantID: "t", SubjectID: "exporter", Permissions: []string{"expense.archive.read", "expense.claim.read"}}
	reviewer := domain.Actor{TenantID: "t", SubjectID: "reviewer", Permissions: []string{"expense.archive.read", "expense.claim.read", "expense.archive.export", "expense.scope.tenant_all"}}
	directory := &borrowDirectory{}
	s.deps.Personnel = directory
	return s, db, requester, reviewer, archive, directory
}

// borrowJSON 编码借阅请求的固定测试输入。
// 参数：t 为测试上下文，v 为业务输入。
// 返回值：JSON请求，编码错误终止测试。
func borrowJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}

// borrowMap 转换服务返回值用于业务断言。
// 参数：t 为测试上下文，value 为业务返回值。
// 返回值：响应字段，编码错误终止测试。
func borrowMap(t *testing.T, value any) map[string]any {
	t.Helper()
	raw := borrowJSON(t, value)
	var result map[string]any
	if e := json.Unmarshal(raw, &result); e != nil {
		t.Fatal(e)
	}
	return result
}

// requestBorrow 在真实服务事务申请一个默认七天借阅。
// 参数：t 为测试上下文，s 为服务，a 为身份，archive 为封存版本。
// 返回值：借阅回执字段。
func requestBorrow(t *testing.T, s *Service, a domain.Actor, archive store.Archive) map[string]any {
	t.Helper()
	value, e := s.RequestArchiveBorrow(context.Background(), a, archive.ID, borrowJSON(t, map[string]any{"command_id": "borrow-request", "expected_version": archive.PackageVersion, "reason": "核对历史证据"}))
	if e != nil {
		t.Fatal(e)
	}
	return borrowMap(t, value)
}

// decideBorrow 在真实服务事务由独立人员批准借阅并真实排包。
// 参数：t 为测试上下文，s 为服务，a 为审批身份，id 为借阅。
// 返回值：批准回执字段。
func decideBorrow(t *testing.T, s *Service, a domain.Actor, id string) map[string]any {
	t.Helper()
	value, e := s.DecideArchiveBorrow(context.Background(), a, id, borrowJSON(t, map[string]any{"command_id": "borrow-decide", "expected_version": 1, "decision": "APPROVE", "reason": "独立复核通过"}))
	if e != nil {
		t.Fatal(e)
	}
	return borrowMap(t, value)
}

// TestArchiveBorrowApprovalQueuesActualReviewerJob 验证独立批准、真实建包身份与命令重放。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchiveBorrowApprovalQueuesActualReviewerJob(t *testing.T) {
	s, db, requester, reviewer, archive, _ := borrowFixture(t)
	first := requestBorrow(t, s, requester, archive)
	replay := requestBorrow(t, s, requester, archive)
	if first["id"] != replay["id"] || first["requested_days"] != float64(7) {
		t.Fatalf("申请重放或默认期限错误: %v %v", first, replay)
	}
	approved := decideBorrow(t, s, reviewer, first["id"].(string))
	again := decideBorrow(t, s, reviewer, first["id"].(string))
	if approved["job_id"] != again["job_id"] || approved["status"] != "APPROVED" {
		t.Fatalf("审批重放重复排包: %v %v", approved, again)
	}
	var jobs []store.Job
	if e := db.Where("kind = ?", "ARCHIVE_PACKAGE").Find(&jobs).Error; e != nil {
		t.Fatal(e)
	}
	if len(jobs) != 1 {
		t.Fatalf("实际建包任务数: %d", len(jobs))
	}
	var input struct {
		Actor domain.Actor `json:"actor"`
	}
	if e := json.Unmarshal([]byte(jobs[0].PayloadJSON), &input); e != nil {
		t.Fatal(e)
	}
	if input.Actor.SubjectID != "reviewer" || !input.Actor.Has("expense.archive.export") || requester.Has("expense.archive.export") {
		t.Fatalf("未使用真实审批人身份: %+v", input.Actor)
	}
	_, e := s.RequestArchiveBorrow(context.Background(), requester, archive.ID, borrowJSON(t, map[string]any{"command_id": "borrow-request", "expected_version": archive.PackageVersion, "reason": "篡改目的"}))
	var p *domain.Problem
	if !errors.As(e, &p) || p.Code != "COMMAND_PAYLOAD_CONFLICT" {
		t.Fatalf("异载荷重试必须冲突: %v", e)
	}
}

// TestArchiveBorrowRejectsBoundaryAndCurrentRevocation 验证租户、整单范围、自审和实时撤权。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchiveBorrowRejectsBoundaryAndCurrentRevocation(t *testing.T) {
	s, _, requester, reviewer, archive, directory := borrowFixture(t)
	body := borrowJSON(t, map[string]any{"command_id": "guard", "expected_version": archive.PackageVersion, "reason": "查看"})
	for _, a := range []domain.Actor{{TenantID: "other", SubjectID: requester.SubjectID, Permissions: requester.Permissions}, {TenantID: "t", SubjectID: "outsider", Permissions: requester.Permissions}} {
		if _, e := s.RequestArchiveBorrow(context.Background(), a, archive.ID, body); e == nil {
			t.Fatal("越界申请不应成功")
		}
	}
	first := requestBorrow(t, s, requester, archive)
	self := requester
	self.Permissions = append(append([]string{}, requester.Permissions...), "expense.archive.export")
	if _, e := s.DecideArchiveBorrow(context.Background(), self, first["id"].(string), borrowJSON(t, map[string]any{"command_id": "self", "expected_version": 1, "decision": "APPROVE", "reason": "自审"})); e == nil {
		t.Fatal("不能自审")
	}
	directory.revoked = true
	if _, e := s.DecideArchiveBorrow(context.Background(), reviewer, first["id"].(string), borrowJSON(t, map[string]any{"command_id": "revoked", "expected_version": 1, "decision": "APPROVE", "reason": "复核"})); e == nil {
		t.Fatal("审批必须复核申请人实时能力")
	}
}

// seedBorrowDownload 创建真实包摘要与已完成导出结果，保持真实应用下载校验。
// 参数：t 为测试上下文，db 为临时库，archive 为版本，approved 为批准回执。
// 返回值：可用于比对的包字节。
func seedBorrowDownload(t *testing.T, db *gorm.DB, archive store.Archive, approved map[string]any) []byte {
	t.Helper()
	content := []byte("PK-borrowed-archive")
	sum := sha256.Sum256(content)
	jobID := approved["job_id"].(string)
	if e := db.Model(&store.Job{}).Where("id = ?", jobID).Update("status", "DONE").Error; e != nil {
		t.Fatal(e)
	}
	if e := db.Create(&store.ArchiveDownload{ID: "borrow-file", TenantID: "t", JobID: jobID, ClaimID: archive.ClaimID, ArchiveID: archive.ID, ManifestHash: archive.ManifestHash, SHA256: hex.EncodeToString(sum[:]), Content: content}).Error; e != nil {
		t.Fatal(e)
	}
	return content
}

// TestArchiveBorrowDownloadRechecksBindingAndExpiry 验证借阅人真实下载、期限与清单绑定。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchiveBorrowDownloadRechecksBindingAndExpiry(t *testing.T) {
	s, db, requester, reviewer, archive, _ := borrowFixture(t)
	first := requestBorrow(t, s, requester, archive)
	id := first["id"].(string)
	approved := decideBorrow(t, s, reviewer, id)
	content := seedBorrowDownload(t, db, archive, approved)
	name, download, e := s.ReadArchiveBorrow(context.Background(), requester, id)
	if e != nil || name == "" || !bytes.Equal(content, download) {
		t.Fatalf("已批准且无export的申请人应可下载: %s %v", name, e)
	}
	revoked := requester
	revoked.Permissions = []string{"expense.claim.read"}
	if _, _, e = s.ReadArchiveBorrow(context.Background(), revoked, id); e == nil {
		t.Fatal("撤权后不得凭历史批准下载")
	}
	if e = db.Model(&store.ArchiveDownload{}).Where("id = ?", "borrow-file").Update("manifest_hash", "wrong").Error; e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.ReadArchiveBorrow(context.Background(), requester, id); e == nil {
		t.Fatal("异清单包必须拒绝")
	}
	if e = db.Model(&store.ArchiveDownload{}).Where("id = ?", "borrow-file").Update("manifest_hash", archive.ManifestHash).Error; e != nil {
		t.Fatal(e)
	}
	if e = db.Model(&store.ArchiveBorrow{}).Where("id = ?", id).Update("expires_at", time.Now().Add(-time.Second)).Error; e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.ReadArchiveBorrow(context.Background(), requester, id); e == nil {
		t.Fatal("到期借阅必须拒绝")
	}
}

// TestArchiveBorrowAuditFailureRollsBackApprovalAndDownload 验证审计不可落库时不批准、不排包、不返回文件。
// 参数：t为测试上下文。
// 返回值：无。
func TestArchiveBorrowAuditFailureRollsBackApprovalAndDownload(t *testing.T) {
	s, db, requester, reviewer, archive, _ := borrowFixture(t)
	first := requestBorrow(t, s, requester, archive)
	id := first["id"].(string)
	if e := db.Callback().Create().Before("gorm:create").Register("borrow-audit-fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "exp_outbox" {
			tx.AddError(errors.New("audit unavailable"))
		}
	}); e != nil {
		t.Fatal(e)
	}
	_, e := s.DecideArchiveBorrow(context.Background(), reviewer, id, borrowJSON(t, map[string]any{"command_id": "audit-decision", "expected_version": 1, "decision": "APPROVE", "reason": "复核"}))
	if e == nil {
		t.Fatal("审计失败不得批准成功")
	}
	var row store.ArchiveBorrow
	if e = db.Where("id = ?", id).Take(&row).Error; e != nil {
		t.Fatal(e)
	}
	var jobs int64
	if e = db.Model(&store.Job{}).Where("kind = ?", "ARCHIVE_PACKAGE").Count(&jobs).Error; e != nil {
		t.Fatal(e)
	}
	if row.Status != "PENDING" || row.Version != 1 || jobs != 0 {
		t.Fatalf("批准/排包未原子回滚: %+v jobs=%d", row, jobs)
	}
	if e = db.Callback().Create().Remove("borrow-audit-fail"); e != nil {
		t.Fatal(e)
	}
	approved := decideBorrow(t, s, reviewer, id)
	seedBorrowDownload(t, db, archive, approved)
	if e = db.Callback().Create().Before("gorm:create").Register("borrow-download-audit-fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "exp_outbox" {
			tx.AddError(errors.New("audit unavailable"))
		}
	}); e != nil {
		t.Fatal(e)
	}
	defer db.Callback().Create().Remove("borrow-download-audit-fail")
	name, content, e := s.ReadArchiveBorrow(context.Background(), requester, id)
	if e == nil || name != "" || len(content) != 0 {
		t.Fatalf("下载审计失败不得返回文件: %q %d %v", name, len(content), e)
	}
}

// TestArchiveBorrowListScopeAndExpiredPackage 验证详情列表不扩范围、包TTL缺失与真实内容篡改。
// 参数：t为测试上下文。
// 返回值：无。
func TestArchiveBorrowListScopeAndExpiredPackage(t *testing.T) {
	s, db, requester, reviewer, archive, _ := borrowFixture(t)
	first := requestBorrow(t, s, requester, archive)
	id := first["id"].(string)
	approved := decideBorrow(t, s, reviewer, id)
	seedBorrowDownload(t, db, archive, approved)
	outsider := domain.Actor{TenantID: "t", SubjectID: "outsider", Permissions: requester.Permissions}
	if _, e := s.ArchiveBorrow(context.Background(), outsider, id); e == nil {
		t.Fatal("无范围不得读借阅详情")
	}
	page, e := s.ArchiveBorrows(context.Background(), outsider, nil)
	if e != nil || borrowMap(t, page)["total"] != float64(0) {
		t.Fatalf("无范围列表应为空: %v %v", page, e)
	}
	page, e = s.ArchiveBorrows(context.Background(), reviewer, map[string]string{"status": "APPROVED"})
	if e != nil || borrowMap(t, page)["total"] != float64(1) {
		t.Fatalf("独立审批范围内应可恢复借阅: %v %v", page, e)
	}
	if e = db.Model(&store.ArchiveDownload{}).Where("id = ?", "borrow-file").Update("content", []byte("tampered")).Error; e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.ReadArchiveBorrow(context.Background(), requester, id); e == nil {
		t.Fatal("真实字节SHA256不符不得下载")
	}
	if e = db.Where("id = ?", "borrow-file").Delete(&store.ArchiveDownload{}).Error; e != nil {
		t.Fatal(e)
	}
	status, e := s.ArchiveBorrow(context.Background(), requester, id)
	if e != nil || borrowMap(t, status)["package_status"] != "EXPIRED" {
		t.Fatalf("已清理生成物应明确过期: %v %v", status, e)
	}
	if _, _, e = s.ReadArchiveBorrow(context.Background(), requester, id); e == nil {
		t.Fatal("生成物TTL过期不得假成功")
	}
}

// TestArchiveBorrowApprovalRechecksLocalScope 验证实时人员能力仍在也不能复用已撤销的本地组织范围。
// 参数：t为测试上下文。
// 返回值：无。
func TestArchiveBorrowApprovalRechecksLocalScope(t *testing.T) {
	s, db, _, reviewer, archive, _ := borrowFixture(t)
	requester := domain.Actor{TenantID: "t", SubjectID: "financial-reader", Permissions: []string{"expense.archive.read", "expense.claim.read"}}
	scope, _ := encode(domain.Scope{SubjectID: requester.SubjectID, OrgIDs: []string{"org"}})
	if e := db.Create(&store.Config{ID: "borrow-scope", TenantID: "t", Kind: "scopes", Published: true, Version: 1, ContentJSON: scope}).Error; e != nil {
		t.Fatal(e)
	}
	first := requestBorrow(t, s, requester, archive)
	if e := db.Where("id = ?", "borrow-scope").Delete(&store.Config{}).Error; e != nil {
		t.Fatal(e)
	}
	_, e := s.DecideArchiveBorrow(context.Background(), reviewer, first["id"].(string), borrowJSON(t, map[string]any{"command_id": "scope-revoked", "expected_version": 1, "decision": "APPROVE", "reason": "审批"}))
	if e == nil {
		t.Fatal("审批不得把已撤销组织范围扩大为借阅授权")
	}
}

// TestArchiveBorrowRejectAndValidation 验证拒绝不排包、版本冲突与最长七日期限。
// 参数：t为测试上下文。
// 返回值：无。
func TestArchiveBorrowRejectAndValidation(t *testing.T) {
	s, db, requester, reviewer, archive, _ := borrowFixture(t)
	for _, days := range []int{-1, 8} {
		_, e := s.RequestArchiveBorrow(context.Background(), requester, archive.ID, borrowJSON(t, map[string]any{"command_id": "invalid", "expected_version": archive.PackageVersion, "requested_days": days, "reason": "查看"}))
		p, ok := e.(*domain.Problem)
		if !ok || p.Status != 422 {
			t.Fatalf("非法期限须422: %v", e)
		}
	}
	first := requestBorrow(t, s, requester, archive)
	id := first["id"].(string)
	body := map[string]any{"command_id": "reject", "expected_version": 2, "decision": "REJECT", "reason": "用途不充分"}
	if _, e := s.DecideArchiveBorrow(context.Background(), reviewer, id, borrowJSON(t, body)); e == nil {
		t.Fatal("旧版本不得审批")
	}
	body["expected_version"] = 1
	value, e := s.DecideArchiveBorrow(context.Background(), reviewer, id, borrowJSON(t, body))
	if e != nil || borrowMap(t, value)["status"] != "REJECTED" {
		t.Fatalf("明确拒绝应落库: %v %v", value, e)
	}
	var count int64
	if e = db.Model(&store.Job{}).Where("kind = ?", "ARCHIVE_PACKAGE").Count(&count).Error; e != nil {
		t.Fatal(e)
	}
	if count != 0 {
		t.Fatal("拒绝申请不得建包")
	}
}

// TestArchiveBorrowConcurrentDecisionsCAS 验证两名独立审批人竞争同版本时仅一人落库建包。
// 参数：t为测试上下文。
// 返回值：无。
func TestArchiveBorrowConcurrentDecisionsCAS(t *testing.T) {
	s, db, requester, reviewer, archive, _ := borrowFixture(t)
	first := requestBorrow(t, s, requester, archive)
	id := first["id"].(string)
	other := reviewer
	other.SubjectID = "reviewer2"
	results := make(chan error, 2)
	for i, a := range []domain.Actor{reviewer, other} {
		raw := borrowJSON(t, map[string]any{"command_id": fmt.Sprint("decision-", i), "expected_version": 1, "decision": "APPROVE", "reason": "独立核对"})
		go func(actor domain.Actor, payload json.RawMessage) {
			_, e := s.DecideArchiveBorrow(context.Background(), actor, id, payload)
			results <- e
		}(a, raw)
	}
	success := 0
	for range 2 {
		if e := <-results; e == nil {
			success++
		}
	}
	var count int64
	if e := db.Model(&store.Job{}).Where("kind = ?", "ARCHIVE_PACKAGE").Count(&count).Error; e != nil {
		t.Fatal(e)
	}
	if success != 1 || count != 1 {
		t.Fatalf("竞争审批必须仅一成功且只排一包: success=%d jobs=%d", success, count)
	}
}

// TestArchiveBorrowRequestAuditFailureAndArchiveDrift 验证申请审计失败整体回滚及封存摘要漂移拒绝下载。
// 参数：t为测试上下文。
// 返回值：无。
func TestArchiveBorrowRequestAuditFailureAndArchiveDrift(t *testing.T) {
	s, db, requester, reviewer, archive, _ := borrowFixture(t)
	if e := db.Callback().Create().Before("gorm:create").Register("borrow-request-audit-fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "exp_outbox" {
			tx.AddError(errors.New("audit unavailable"))
		}
	}); e != nil {
		t.Fatal(e)
	}
	_, e := s.RequestArchiveBorrow(context.Background(), requester, archive.ID, borrowJSON(t, map[string]any{"command_id": "borrow-request", "expected_version": archive.PackageVersion, "reason": "核对历史证据"}))
	if e == nil {
		t.Fatal("申请审计失败不得成功")
	}
	var count int64
	if e = db.Model(&store.ArchiveBorrow{}).Count(&count).Error; e != nil {
		t.Fatal(e)
	}
	if count != 0 {
		t.Fatal("申请审计失败仍落借阅事实")
	}
	if e = db.Callback().Create().Remove("borrow-request-audit-fail"); e != nil {
		t.Fatal(e)
	}
	first := requestBorrow(t, s, requester, archive)
	id := first["id"].(string)
	approved := decideBorrow(t, s, reviewer, id)
	seedBorrowDownload(t, db, archive, approved)
	if e = db.Model(&store.Archive{}).Where("id = ?", archive.ID).Update("manifest_json", "{}").Error; e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.ReadArchiveBorrow(context.Background(), requester, id); e == nil {
		t.Fatal("封存清单内容漂移不得复用已批准借阅")
	}
}

// TestArchiveBorrowCommandRecoveryReauthorizesRequest 验证原命令恢复入口不凭claim.read泄露已撤权借阅回执。
// 参数：t为测试上下文。
// 返回值：无。
func TestArchiveBorrowCommandRecoveryReauthorizesRequest(t *testing.T) {
	s, db, _, _, archive, _ := borrowFixture(t)
	requester := domain.Actor{TenantID: "t", SubjectID: "scope-reader", Permissions: []string{"expense.archive.read", "expense.claim.read"}}
	scope, _ := encode(domain.Scope{SubjectID: requester.SubjectID, OrgIDs: []string{"org"}})
	if e := db.Create(&store.Config{ID: "recovery-scope", TenantID: "t", Kind: "scopes", Published: true, Version: 1, ContentJSON: scope}).Error; e != nil {
		t.Fatal(e)
	}
	requestBorrow(t, s, requester, archive)
	for _, op := range []string{"", "archive.borrow.request"} {
		if _, e := s.Query(context.Background(), requester, "commands", "borrow-request", map[string]string{"operation": op}); e != nil {
			t.Fatalf("当前合法范围应可恢复: %v", e)
		}
	}
	revoked := requester
	revoked.Permissions = []string{"expense.claim.read"}
	for _, op := range []string{"", "archive.borrow.request"} {
		if _, e := s.Query(context.Background(), revoked, "commands", "borrow-request", map[string]string{"operation": op}); e == nil {
			t.Errorf("撤销archive.read后恢复仍成功，operation=%q", op)
		}
	}
	if e := db.Where("id = ?", "recovery-scope").Delete(&store.Config{}).Error; e != nil {
		t.Fatal(e)
	}
	for _, op := range []string{"", "archive.borrow.request"} {
		if _, e := s.Query(context.Background(), requester, "commands", "borrow-request", map[string]string{"operation": op}); e == nil {
			t.Errorf("撤销组织范围后恢复仍成功，operation=%q", op)
		}
	}
}

// TestArchiveBorrowCommandRecoveryReauthorizesDecision 验证原审批命令恢复重新要求当前export与整单组织范围。
// 参数：t为测试上下文。
// 返回值：无。
func TestArchiveBorrowCommandRecoveryReauthorizesDecision(t *testing.T) {
	s, db, requester, reviewer, archive, _ := borrowFixture(t)
	reviewer.Permissions = []string{"expense.archive.read", "expense.archive.export", "expense.claim.read"}
	scope, _ := encode(domain.Scope{SubjectID: reviewer.SubjectID, OrgIDs: []string{"org"}})
	if e := db.Create(&store.Config{ID: "review-recovery-scope", TenantID: "t", Kind: "scopes", Published: true, Version: 1, ContentJSON: scope}).Error; e != nil {
		t.Fatal(e)
	}
	first := requestBorrow(t, s, requester, archive)
	decideBorrow(t, s, reviewer, first["id"].(string))
	for _, op := range []string{"", "archive.borrow.decide"} {
		if _, e := s.Query(context.Background(), reviewer, "commands", "borrow-decide", map[string]string{"operation": op}); e != nil {
			t.Fatalf("当前合法审批范围应可恢复: %v", e)
		}
	}
	revoked := reviewer
	revoked.Permissions = []string{"expense.archive.read", "expense.claim.read"}
	for _, op := range []string{"", "archive.borrow.decide"} {
		if _, e := s.Query(context.Background(), revoked, "commands", "borrow-decide", map[string]string{"operation": op}); e == nil {
			t.Errorf("撤销export后审批恢复仍成功，operation=%q", op)
		}
	}
	if e := db.Where("id = ?", "review-recovery-scope").Delete(&store.Config{}).Error; e != nil {
		t.Fatal(e)
	}
	for _, op := range []string{"", "archive.borrow.decide"} {
		if _, e := s.Query(context.Background(), reviewer, "commands", "borrow-decide", map[string]string{"operation": op}); e == nil {
			t.Errorf("撤销审批组织范围后恢复仍成功，operation=%q", op)
		}
	}
}

// TestArchiveBorrowRejectRemainsLiveAfterRequesterRevocation 验证申请人撤权或目录故障不阻止独立拒绝关闭。
// 参数：t为测试上下文。
// 返回值：无。
func TestArchiveBorrowRejectRemainsLiveAfterRequesterRevocation(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint("directory-unavailable-", unavailable), func(t *testing.T) {
			s, db, requester, reviewer, archive, directory := borrowFixture(t)
			first := requestBorrow(t, s, requester, archive)
			id := first["id"].(string)
			directory.revoked = !unavailable
			directory.unavailable = unavailable
			approve := borrowJSON(t, map[string]any{"command_id": "must-not-approve", "expected_version": 1, "decision": "APPROVE", "reason": "批准核验"})
			if _, e := s.DecideArchiveBorrow(context.Background(), reviewer, id, approve); e == nil {
				t.Fatal("当前申请人无可确认资格不得批准")
			} else if unavailable {
				p, ok := e.(*domain.Problem)
				if !ok || p.Status != 503 {
					t.Fatalf("目录故障批准必须503失败关闭: %v", e)
				}
			}
			reject := borrowJSON(t, map[string]any{"command_id": "close-after-revocation", "expected_version": 1, "decision": "REJECT", "reason": "资格无法满足，关闭申请"})
			value, e := s.DecideArchiveBorrow(context.Background(), reviewer, id, reject)
			if e != nil || borrowMap(t, value)["status"] != "REJECTED" {
				t.Fatalf("独立拒绝不应被申请人撤权或目录故障阻断: %v %v", value, e)
			}
			var jobs int64
			if e = db.Model(&store.Job{}).Where("kind = ?", "ARCHIVE_PACKAGE").Count(&jobs).Error; e != nil {
				t.Fatal(e)
			}
			if jobs != 0 {
				t.Fatal("拒绝关闭不得建包")
			}
			var audits []store.Outbox
			if e = db.Where("topic = ? AND resource_id = ?", "audit", archive.ClaimID).Find(&audits).Error; e != nil {
				t.Fatal(e)
			}
			decisions := 0
			for _, audit := range audits {
				var fields map[string]any
				if e = json.Unmarshal([]byte(audit.PayloadJSON), &fields); e != nil {
					t.Fatal(e)
				}
				if fields["action"] == "archive.borrow.decide" {
					decisions++
				}
			}
			if decisions != 1 {
				t.Fatalf("拒绝关闭应可靠审计一次: %d", decisions)
			}
		})
	}
}
