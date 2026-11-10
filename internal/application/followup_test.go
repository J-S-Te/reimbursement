package application

import (
	"context"
	"encoding/json"
	"errors"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStandardChoosesUniqueMaximumBeforeDetectingTies 防止低优先级重复误阻断唯一最高标准。
// 参数：t 为测试上下文。
// 返回值：无。
func TestStandardChoosesUniqueMaximumBeforeDetectingTies(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	_ = s
	seedPolicy(t, db, "employee_profiles", domain.EmployeeProfile{ID: "profile", SubjectID: "app", OrgID: "org", Grade: "G", RouteType: "TECHNICAL", Active: true})
	for i, priority := range []int{1, 1, 2} {
		seedPolicy(t, db, "standards", domain.Standard{ID: []string{"low-a", "low-b", "highest"}[i], OrgID: "org", FeeType: "TRAVEL", Grade: "G", Limit: 100, Unit: "PER_OCCURRENCE", Priority: priority, ValidFrom: "2026-01-01"})
	}
	if e := db.Model(&store.Config{}).Where("id = ?", "low-a").Update("created_at", time.Now().Add(3*time.Hour)).Error; e != nil {
		t.Fatal(e)
	}
	if e := db.Model(&store.Config{}).Where("id = ?", "low-b").Update("created_at", time.Now().Add(2*time.Hour)).Error; e != nil {
		t.Fatal(e)
	}
	claim := domain.Claim{TenantID: "t", Applicant: "app", OrgID: "org", RouteType: "TECHNICAL", Items: []domain.Item{{FeeType: "TRAVEL", OccurredDate: "2026-11-10", Requested: 90, Units: 1, InvoiceUsages: []domain.InvoiceUsage{{InvoiceID: "invoice", Used: 90}}}}}
	if e := applyStandards(db, &claim); e != nil {
		t.Fatalf("唯一最高优先级不能因较低优先级重复被误拒:%v", e)
	}
	if claim.Items[0].StandardVersionID != "highest" {
		t.Fatal("未选最高标准")
	}
}

type repeatUploadGateway struct {
	filesFixture
	calls int
}

// CreateUpload 返回重复签发的网关会话以验证生命周期不会被覆盖。
// 参数：上下文、身份、文档标识及元数据。
// 返回值：真实边界形状的测试会话。
func (g *repeatUploadGateway) CreateUpload(context.Context, domain.Actor, string, json.RawMessage) (json.RawMessage, error) {
	g.calls++
	return json.RawMessage(`{"file_id":"file","upload_id":"upload","ticket":"one-use","upload_url":"/upload"}`), nil
}

// CompleteUpload 提供测试未调用的网关完成边界。
// 参数：上下文、身份及文档标识。
// 返回值：就绪测试响应。
func (*repeatUploadGateway) CompleteUpload(context.Context, domain.Actor, string) (json.RawMessage, error) {
	return json.RawMessage(`{"file_id":"file","status":"READY"}`), nil
}

// ReadDocument 提供测试未调用的原件读取边界。
// 参数：上下文、租户及文档标识。
// 返回值：空测试文件。
func (*repeatUploadGateway) ReadDocument(context.Context, string, string) ([]byte, error) {
	return nil, nil
}

// TestRepeatedUploadPreservesCompletedParse 防止同上传key重试清掉既有解析任务和来源。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRepeatedUploadPreservesCompletedParse(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	gateway := &repeatUploadGateway{}
	s.deps.Files = gateway
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload"}}
	payload := json.RawMessage(`{"purpose":"reimbursement.invoice.original","idempotency_key":"same","filename":"a.pdf"}`)
	raw, e := s.CreateUpload(context.Background(), a, payload)
	if e != nil {
		t.Fatal(e)
	}
	var result struct {
		ID string `json:"document_id"`
	}
	_ = json.Unmarshal(raw, &result)
	var doc store.Document
	db.First(&doc, "id = ?", result.ID)
	var metadata map[string]any
	_ = json.Unmarshal([]byte(doc.ContentJSON), &metadata)
	metadata["parse_job_id"] = "job"
	metadata["parse_complete"] = true
	metadata["parse_result"] = map[string]any{"parser_version": "frozen"}
	content, _ := json.Marshal(metadata)
	db.Model(&doc).Updates(map[string]any{"status": "PARSED", "content_json": string(content)})
	if _, e = s.CreateUpload(context.Background(), a, payload); e != nil {
		t.Fatal(e)
	}
	db.First(&doc, "id = ?", result.ID)
	metadata = nil
	_ = json.Unmarshal([]byte(doc.ContentJSON), &metadata)
	if metadata["parse_job_id"] != "job" || metadata["parse_complete"] != true {
		t.Fatal("重复上传覆盖已完成解析生命周期")
	}
	if gateway.calls != 1 {
		t.Fatal("已完成上传不应再次签发上传ticket")
	}
}

// TestConfigurationReplaceIsAtomicAndPreservesPublishedContent 验证退休与新版本发布原子切换且不改历史。
// 参数：t 为测试上下文。
// 返回值：无。
func TestConfigurationReplaceIsAtomicAndPreservesPublishedContent(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	old := domain.EmployeeProfile{ID: "old-profile", SubjectID: "app", OrgID: "org", Grade: "G1", RouteType: "TECHNICAL", Version: 1, Active: true}
	seedPolicy(t, db, "employee_profiles", old)
	var before store.Config
	db.First(&before, "id = ?", old.ID)
	admin := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.config.manage"}}
	draft := runCommand(t, s, admin, "config.save", "employee_profiles", map[string]any{"command_id": "new-profile", "kind": "employee_profiles", "subject_id": "app", "org_id": "org", "grade": "G2", "route_type": "TECHNICAL", "active": true})
	head := domain.Actor{TenantID: "t", SubjectID: "head", Permissions: []string{"expense.policy.publish"}}
	request := map[string]any{"command_id": "replace-profile", "expected_version": 1, "kind": "employee_profiles", "replacement_id": draft["id"], "replacement_expected_version": draft["version"], "reason": "已审核晋级"}
	runCommand(t, s, head, "config.replace", old.ID, request)
	var after store.Config
	db.First(&after, "id = ?", old.ID)
	if after.ContentJSON != before.ContentJSON {
		t.Fatal("退休改写了已发布政策内容")
	}
	profile, e := employeeProfile(db, domain.Claim{TenantID: "t", Applicant: "app", OrgID: "org"})
	if e != nil {
		t.Fatal(e)
	}
	if profile.Grade != "G2" {
		t.Fatal("新业务没有切换替换资料")
	}
	runCommand(t, s, head, "config.replace", old.ID, request)
	var count int64
	db.Table("exp_policy_lifecycle").Count(&count)
	if count != 1 {
		t.Fatal("同命令重放重复写退休账本")
	}
}

// TestConfigurationPublishRejectsOverlappingEffectiveVersions 防止重叠政策发布后业务因冲突长期不可用。
// 参数：t 为测试上下文。
// 返回值：无。
func TestConfigurationPublishRejectsOverlappingEffectiveVersions(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "employee_profiles", domain.EmployeeProfile{ID: "old", SubjectID: "app", OrgID: "org", Grade: "G1", RouteType: "TECHNICAL", Active: true})
	admin := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.config.manage"}}
	draft := runCommand(t, s, admin, "config.save", "employee_profiles", map[string]any{"command_id": "draft", "kind": "employee_profiles", "subject_id": "app", "org_id": "org", "grade": "G2", "route_type": "TECHNICAL", "active": true})
	head := domain.Actor{TenantID: "t", SubjectID: "head", Permissions: []string{"expense.policy.publish"}}
	raw, _ := json.Marshal(map[string]any{"command_id": "publish", "expected_version": draft["version"], "kind": "employee_profiles"})
	if _, e := s.Execute(context.Background(), head, "config.publish", draft["id"].(string), raw); e == nil {
		t.Fatal("同主体组织的重叠已发布资料未阻断")
	}
}

type personnelFixture struct {
	allowed map[string]bool
	calls   []string
}

// VerifyApprover 验证测试授权目录只为指定受理人和精确任务权限返回成功。
// 参数：上下文、可信操作者、目标主体和所需任务权限。
// 返回值：未在职或未授权目标的拒绝错误。
func (p *personnelFixture) VerifyApprover(_ context.Context, a domain.Actor, target, permission string) error {
	p.calls = append(p.calls, target+"/"+permission)
	if a.TenantID != "t" || permission != "expense.approval.act" || !p.allowed[target] {
		return domain.Fail("APPROVER_INELIGIBLE", "新受理人不在职或没有当前审批能力", 403)
	}
	return nil
}

// TestTransferFailsClosedAndKeepsEveryHop 验证目录不可用关闭及每次转交责任链持久保存。
// 参数：t 为测试上下文。
// 返回值：无。
func TestTransferFailsClosedAndKeepsEveryHop(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	runCommand(t, s, a, "claim.submit", id, map[string]any{"command_id": "submit-transfer", "expected_version": 1})
	for _, subject := range []string{"lead2", "lead3", "inactive"} {
		seedPolicy(t, db, "assignments", domain.Assignment{ID: "alternate-" + subject, SubjectID: subject, OrgID: "org", ProjectID: "p1", Role: "BUSINESS_REVIEW", ValidFrom: "2026-01-01"})
	}
	var task store.Task
	db.Where("claim_id = ? AND node = ?", id, 1).First(&task)
	source := domain.Actor{TenantID: "t", SubjectID: "lead", Permissions: []string{"expense.claim.read", "expense.approval.act", "expense.approval.transfer"}}
	s.deps.Personnel = nil
	body := map[string]any{"command_id": "missing-personnel", "expected_version": 2, "task_version": 1, "round": 1, "action": "TRANSFER", "target_subject_id": "lead2", "reason": "现场职责移交"}
	raw, _ := json.Marshal(body)
	if _, e := s.Execute(context.Background(), source, "approval.action", task.ID, raw); e == nil {
		t.Fatal("缺少当前人员资格目录时仍允许转交")
	}
	directory := &personnelFixture{allowed: map[string]bool{"lead2": true, "lead3": true}}
	s.deps.Personnel = directory
	body["command_id"] = "hop1"
	runCommand(t, s, source, "approval.action", task.ID, body)
	source.SubjectID = "lead2"
	body["command_id"] = "hop2"
	body["expected_version"] = 3
	body["task_version"] = 2
	body["target_subject_id"] = "lead3"
	runCommand(t, s, source, "approval.action", task.ID, body)
	var details []store.ActionDetail
	db.Order("created_at,id").Find(&details)
	if len(details) != 2 || details[0].FromSubjectID != "lead" || details[0].ToSubjectID != "lead2" || details[1].FromSubjectID != "lead2" || details[1].ToSubjectID != "lead3" {
		t.Fatalf("多次转交未保留完整逐跳目标:%+v", details)
	}
	if len(directory.calls) != 2 || directory.calls[0] != "lead2/expense.approval.act" || directory.calls[1] != "lead3/expense.approval.act" {
		t.Fatal("未按每跳目标与精确节点能力查询目录")
	}
	source.SubjectID = "lead3"
	body["command_id"] = "inactive-target"
	body["expected_version"] = 4
	body["task_version"] = 3
	body["target_subject_id"] = "inactive"
	raw, _ = json.Marshal(body)
	if _, e := s.Execute(context.Background(), source, "approval.action", task.ID, raw); e == nil {
		t.Fatal("未授权新受理人被放行")
	}
	db.First(&task, "id = ?", task.ID)
	if task.Assignee != "lead3" || task.Version != 3 {
		t.Fatal("目录拒绝仍改变任务")
	}
	// 替代人依当前有效映射和不可变转交链办理，不冒充原项目经理。
	body["command_id"] = "substitute-approve"
	body["action"] = "APPROVE"
	body["target_subject_id"] = ""
	runCommand(t, s, source, "approval.action", task.ID, body)
	db.First(&task, "id = ?", task.ID)
	if task.Status != "APPROVED" {
		t.Fatal("受控替代人无法依合法责任链完成审批")
	}
}

// TestPolicyPublicationNormalizesDefaultsAndUnit 防止默认维度和不同计量单位绕过相同匹配范围冲突。
// 参数：t 为测试上下文。
// 返回值：无。
func TestPolicyPublicationNormalizesDefaultsAndUnit(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "standards", domain.Standard{ID: "standard-old", OrgID: "org", FeeType: "TRAVEL", Grade: "G", Limit: 100, Unit: "PER_NIGHT", ValidFrom: "2026-01-01", ValidUntil: "2026-12-31"})
	admin := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.config.manage"}}
	draft := runCommand(t, s, admin, "config.save", "standards", map[string]any{"command_id": "standard-new", "kind": "standards", "org_id": "org", "fee_type": "TRAVEL", "grade": "G", "limit_minor": "200", "unit": "PER_OCCURRENCE", "valid_from": "2026-01-01"})
	head := domain.Actor{TenantID: "t", SubjectID: "head", Permissions: []string{"expense.policy.publish"}}
	raw, _ := json.Marshal(map[string]any{"command_id": "standard-publish", "kind": "standards", "expected_version": draft["version"]})
	if _, e := s.Execute(context.Background(), head, "config.publish", draft["id"].(string), raw); e == nil {
		t.Fatal("相同实际匹配范围因省略默认维度或改unit被重复发布")
	}
}

// TestReplacementAuditFailureRollsBackBothVersions 验证替换发布和退休审计失败时不留配置窗口或半成功。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReplacementAuditFailureRollsBackBothVersions(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "employee_profiles", domain.EmployeeProfile{ID: "old", SubjectID: "app", OrgID: "org", Grade: "G1", RouteType: "TECHNICAL", Active: true})
	admin := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.config.manage"}}
	draft := runCommand(t, s, admin, "config.save", "employee_profiles", map[string]any{"command_id": "replace-draft", "kind": "employee_profiles", "subject_id": "app", "org_id": "org", "grade": "G2", "route_type": "TECHNICAL", "active": true})
	if e := db.Exec(`CREATE TRIGGER reject_lifecycle_audit BEFORE INSERT ON exp_outbox WHEN NEW.topic = 'audit' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; e != nil {
		t.Fatal(e)
	}
	head := domain.Actor{TenantID: "t", SubjectID: "head", Permissions: []string{"expense.policy.publish"}}
	raw, _ := json.Marshal(map[string]any{"command_id": "replace-audit-fail", "expected_version": 1, "kind": "employee_profiles", "replacement_id": draft["id"], "replacement_expected_version": draft["version"], "reason": "调整职级"})
	if _, e := s.Execute(context.Background(), head, "config.replace", "old", raw); e == nil {
		t.Fatal("审计失败仍完成替换")
	}
	var old, next store.Config
	db.First(&old, "id = ?", "old")
	db.First(&next, "id = ?", draft["id"])
	var events int64
	db.Model(&store.PolicyLifecycle{}).Count(&events)
	if old.Version != 1 || next.Published || events != 0 {
		t.Fatal("原子替换失败留下退休或已发布新版本")
	}
}

// TestRetirementRequiresCurrentPublisherAndKeepsHistoricalContent 验证独立退休权限、版本与不可变历史。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRetirementRequiresCurrentPublisherAndKeepsHistoricalContent(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	seedPolicy(t, db, "employee_profiles", domain.EmployeeProfile{ID: "old", SubjectID: "app", OrgID: "org", Grade: "G", RouteType: "TECHNICAL", Active: true})
	var before store.Config
	db.First(&before, "id = ?", "old")
	raw := json.RawMessage(`{"command_id":"retire","kind":"employee_profiles","expected_version":1,"reason":"人员已离岗"}`)
	admin := domain.Actor{TenantID: "t", SubjectID: "admin", Permissions: []string{"expense.config.manage"}}
	if _, e := s.Execute(context.Background(), admin, "config.retire", "old", raw); e == nil {
		t.Fatal("仅配置维护权不能退休发布政策")
	}
	head := domain.Actor{TenantID: "t", SubjectID: "head", Permissions: []string{"expense.policy.publish"}}
	if _, e := s.Execute(context.Background(), head, "config.retire", "old", raw); e != nil {
		t.Fatal(e)
	}
	if _, e := employeeProfile(db, domain.Claim{TenantID: "t", Applicant: "app", OrgID: "org"}); e == nil {
		t.Fatal("已退休资料仍被新业务选中")
	}
	var after store.Config
	db.First(&after, "id = ?", "old")
	if before.ContentJSON != after.ContentJSON || after.Version != 2 {
		t.Fatal("退休没有保持已发布内容与独立行版本")
	}
	head.Permissions = nil
	if _, e := s.Execute(context.Background(), head, "config.retire", "old", raw); e == nil {
		t.Fatal("权限已撤销仍能读取冻结退休结果")
	}
}

// TestArchiveRetryUsesExistingLiveJob 防止归档重试重复排队同一权威清单。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchiveRetryUsesExistingLiveJob(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	_ = a
	seedPolicy(t, db, "scopes", domain.Scope{ID: "archive-scope", SubjectID: "archiver", OrgIDs: []string{"org"}})
	archive := store.Archive{ID: "archive", TenantID: "t", ClaimID: id, PackageVersion: 1, Status: "FAILED", ManifestJSON: `{"claim_id":"claim"}`}
	if e := db.Create(&archive).Error; e != nil {
		t.Fatal(e)
	}
	job := store.Job{ID: "existing-job", TenantID: "t", Kind: "ARCHIVE", ResourceID: archive.ID, PayloadJSON: archive.ManifestJSON, Status: "PROCESSING", Version: 1, LeaseOwner: "worker", NextAttemptAt: time.Now().UTC()}
	if e := db.Create(&job).Error; e != nil {
		t.Fatal(e)
	}
	actor := domain.Actor{TenantID: "t", SubjectID: "archiver", Permissions: []string{"expense.claim.read", "expense.archive.read", "expense.archive.retry"}}
	result := runCommand(t, s, actor, "archive.retry", id, map[string]any{"command_id": "retry"})
	if result["job_id"] != job.ID || result["status"] != "PROCESSING" {
		t.Fatal("已有处理中任务仍创建了新任务")
	}
	var count int64
	db.Model(&store.Job{}).Where("resource_id = ?", archive.ID).Count(&count)
	if count != 1 {
		t.Fatal("重复归档排队")
	}
}

// TestDeadlockRetriesOnlyLocalTransactionWithRollback 验证瞬时MySQL错误的有限重试和失败尝试整体回滚。
// 参数：t 为测试上下文。
// 返回值：无。
func TestDeadlockRetriesOnlyLocalTransactionWithRollback(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	attempts := 0
	e := localTransaction(context.Background(), db, func(tx *gorm.DB) error {
		attempts++
		if err := tx.Create(&store.Sequence{ID: "same-command-local-fact", Value: int64(attempts)}).Error; err != nil {
			return err
		}
		if attempts < 3 {
			return &mysqldriver.MySQLError{Number: 1213, Message: "simulated deadlock"}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if attempts != 3 {
		t.Fatal("死锁未按上限三次重试")
	}
	var row store.Sequence
	db.First(&row, "id = ?", "same-command-local-fact")
	if row.Value != 3 {
		t.Fatal("失败尝试的事务事实没有回滚")
	}
}

// TestTransactionRetryStopsAtCancellation 验证退避期间取消会立即终止且不继续命令。
// 参数：t 为测试上下文。
// 返回值：无。
func TestTransactionRetryStopsAtCancellation(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	e := localTransaction(ctx, db, func(*gorm.DB) error {
		attempts++
		cancel()
		return &mysqldriver.MySQLError{Number: 1205, Message: "simulated lock timeout"}
	})
	if !errors.Is(e, context.Canceled) || attempts != 1 {
		t.Fatalf("取消后仍重试或没有返回取消:%v attempts=%d", e, attempts)
	}
}

// TestDeadlockRetryIsBoundedAndBusinessErrorsAreFinal 验证持续死锁只试三次且业务错误绝不重试。
// 参数：t 为测试上下文。
// 返回值：无。
func TestDeadlockRetryIsBoundedAndBusinessErrorsAreFinal(t *testing.T) {
	_, db, _ := setupWorkflow(t)
	attempts := 0
	e := localTransaction(context.Background(), db, func(*gorm.DB) error {
		attempts++
		return &mysqldriver.MySQLError{Number: 1213, Message: "simulated persistent deadlock"}
	})
	if e == nil || attempts != 3 {
		t.Fatalf("持续死锁没有在三次后终止:%v attempts=%d", e, attempts)
	}
	attempts = 0
	business := domain.Fail("STATE_CONFLICT", "当前状态不允许", 409)
	e = localTransaction(context.Background(), db, func(*gorm.DB) error { attempts++; return business })
	if e != business || attempts != 1 {
		t.Fatal("业务冲突被无意义重试")
	}
}

// TestSubmitRejectsInactiveConfiguredApprover 防止本地配置离职审批人仍形成无法办理的待办。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSubmitRejectsInactiveConfiguredApprover(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	s.deps.Personnel = &personnelFixture{allowed: map[string]bool{}}
	if _, e := s.Execute(context.Background(), a, "claim.submit", id, json.RawMessage(`{"command_id":"inactive-submit","expected_version":1}`)); e == nil {
		t.Fatal("配置了当前无资格的审批人仍提交成功")
	}
	var tasks int64
	db.Model(&store.Task{}).Count(&tasks)
	if tasks != 0 {
		t.Fatal("资格拒绝仍创建待办")
	}
}

// TestManualReparseCreatesIndependentJobAndReplaysCommand 验证成功原件可重解析且历史运行和来源不被覆盖。
// 参数：t 为测试上下文。
// 返回值：无。
func TestManualReparseCreatesIndependentJobAndReplaysCommand(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	doc := store.Document{ID: "document", TenantID: "t", Owner: "app", FileID: "file", Status: "PARSED", Version: 2, ContentJSON: `{"purpose":"reimbursement.invoice.original","parse_job_id":"old-job","parse_complete":true,"parse_result":{"parser_version":"old-version"}}`}
	if e := db.Create(&doc).Error; e != nil {
		t.Fatal(e)
	}
	oldJob := store.Job{ID: "old-job", TenantID: "t", Kind: "INVOICE_PARSE", ResourceID: doc.ID, Status: "DONE", Version: 3, Attempt: 1, PayloadJSON: `{"status":"SUCCESS"}`}
	if e := db.Create(&oldJob).Error; e != nil {
		t.Fatal(e)
	}
	oldRun := store.InvoiceParseRun{ID: "old-job-1", TenantID: "t", DocumentID: doc.ID, JobID: oldJob.ID, Attempt: 1, Status: "SUCCESS", ParserVersion: "old-version", ResultJSON: `{"documents":[{"invoice_number":"old"}]}`}
	if e := db.Create(&oldRun).Error; e != nil {
		t.Fatal(e)
	}
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload", "expense.invoice.read"}}
	request := map[string]any{"command_id": "reparse", "expected_version": 2}
	result := runCommand(t, s, actor, "invoice.reparse", doc.ID, request)
	if result["id"] == oldJob.ID || result["status"] != "PENDING" {
		t.Fatal("没有为成功文档创建独立新任务")
	}
	replay := runCommand(t, s, actor, "invoice.reparse", doc.ID, request)
	if replay["id"] != result["id"] {
		t.Fatal("同命令重放重复创建解析任务")
	}
	var count int64
	db.Model(&store.Job{}).Where("resource_id = ?", doc.ID).Count(&count)
	if count != 2 {
		t.Fatal("任务历史被覆盖或重复")
	}
	var retained store.InvoiceParseRun
	db.First(&retained, "id = ?", oldRun.ID)
	if retained.ResultJSON != oldRun.ResultJSON || retained.ParserVersion != "old-version" {
		t.Fatal("旧不可变运行被改写")
	}
	db.First(&doc, "id = ?", doc.ID)
	var metadata map[string]any
	_ = json.Unmarshal([]byte(doc.ContentJSON), &metadata)
	if metadata["parse_complete"] != false || metadata["parse_job_id"] != result["id"] {
		t.Fatal("新解析任务未关联到当前文档投影")
	}
}

// TestInvoiceSourcesUseFrozenRunAndDoNotLeakOtherCandidates 验证来源读取不可变run而非最新文档投影。
// 参数：t 为测试上下文。
// 返回值：无。
func TestInvoiceSourcesUseFrozenRunAndDoNotLeakOtherCandidates(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	invoice := domain.Invoice{ID: "history-invoice", TenantID: "t", Owner: "app", Version: 1, FileIDs: []string{"history-file"}}
	raw, _ := json.Marshal(invoice)
	db.Create(&store.Invoice{ID: invoice.ID, TenantID: "t", Owner: "app", Version: 1, ReservationState: "AVAILABLE", ContentJSON: string(raw)})
	db.Create(&store.Document{ID: "history-doc", TenantID: "t", Owner: "app", FileID: "history-file", Status: "PARSED", Version: 2, ContentJSON: `{"filename":"history.pdf","media_type":"application/pdf","parse_result":{"parser_version":"latest-projection"}}`})
	db.Create(&store.InvoiceParseRun{ID: "frozen-run", TenantID: "t", DocumentID: "history-doc", JobID: "job", Attempt: 1, ParserVersion: "frozen-parser", RulesVersion: "r1", Status: "PARTIAL", ResultJSON: `{"documents":[{"issues":["DATE_REVIEW"]},{"seller_tax_id":"SECRET_OTHER_CANDIDATE","raw_fields":"PRIVATE"}]}`})
	db.Create(&store.InvoiceSource{ID: "source", TenantID: "t", InvoiceID: invoice.ID, DocumentID: "history-doc", ParseRunID: "frozen-run", CandidateIndex: 0, SourcesJSON: `[{"page":1,"x":3,"text":"source evidence"}]`})
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.read"}}
	result, e := s.InvoiceSources(context.Background(), a, invoice.ID)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(result), "frozen-parser") || strings.Contains(string(result), "latest-projection") || strings.Contains(string(result), "SECRET_OTHER_CANDIDATE") || strings.Contains(string(result), "PRIVATE") {
		t.Fatalf("来源没有取固定运行或泄露其他候选:%s", result)
	}
}

type boundedFileFixture struct {
	filesFixture
	calls int
}

// Validate 统计远端验证边界，测试超限请求必须在本地提前终止。
// 参数：上下文、身份、文件集合及用途。
// 返回值：夹具文件均合法。
func (f *boundedFileFixture) Validate(context.Context, domain.Actor, []string, string) error {
	f.calls++
	return nil
}

// TestFileReferenceLimitsBeforeRemoteCalls 防止超限票据原件或付款证据造成远端串行放大。
// 参数：t 为测试上下文。
// 返回值：无。
func TestFileReferenceLimitsBeforeRemoteCalls(t *testing.T) {
	s, _, _ := setupWorkflow(t)
	files := &boundedFileFixture{}
	s.deps.Files = files
	ids := make([]string, 21)
	for i := range ids {
		ids[i] = "file-" + strconv.Itoa(i)
	}
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.invoice.upload", "expense.payment.reversal.request"}}
	for _, test := range []struct{ op, field string }{{"invoice.create", "file_ids"}, {"reversal.request", "evidence_file_ids"}} {
		raw, _ := json.Marshal(map[string]any{"command_id": "bounded-" + test.op, test.field: ids})
		_, e := s.Execute(context.Background(), a, test.op, "unknown", raw)
		var problem *domain.Problem
		if !errors.As(e, &problem) || problem.Status != 422 {
			t.Fatalf("超限未本地返回422:%v", e)
		}
	}
	if files.calls != 0 {
		t.Fatal("超限请求已调用远端文件服务")
	}
}

// TestAuditKeepsFirstTrustedRequestIDAcrossReplay 验证审计只记录可信首次请求追踪并忽略body伪造值。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAuditKeepsFirstTrustedRequestIDAcrossReplay(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.create", "expense.claim.read"}, RequestID: "trusted-trace"}
	body := map[string]any{"command_id": "trace-command", "request_id": "forged-trace"}
	runCommand(t, s, a, "claim.create", "", body)
	a.RequestID = "retry-trace"
	runCommand(t, s, a, "claim.create", "", body)
	var rows []store.Outbox
	db.Where("topic = ?", "audit").Find(&rows)
	if len(rows) != 1 {
		t.Fatal("成功命令重放重复生成审计")
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(rows[0].PayloadJSON), &payload)
	if payload["request_id"] != "trusted-trace" {
		t.Fatal("审计没有使用首次可信请求ID")
	}
}
