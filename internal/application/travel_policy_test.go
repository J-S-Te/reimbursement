package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"strings"
	"testing"
)

// terminalFixtureReference 提供当前仍参与、仅项目终止的可信项目引用。
// 参数：无。
// 返回值：不能被普通eligible放行的终止引用。
func terminalFixtureReference() domain.ProjectReference {
	return domain.ProjectReference{ProjectID: "p1", ServiceItemID: "s", ServiceItemVersion: 1, ProjectStatus: "已终止", ServiceItemStatus: "进行中", ParticipantEligible: true, Eligible: false, ReferenceVersion: "terminal-reference", ProjectVersion: 2, RequiredApproverRefs: []domain.ApproverRef{{SubjectID: "lead", NodeRole: "PROJECT_EXPENSE_OWNER", SourceField: "project_manager_id", SourceVersion: 1}, {SubjectID: "technical-director", NodeRole: "TECHNICAL_DIRECTOR", SourceField: "role:technical_director", SourceDigest: strings.Repeat("a", 64), SourceOrgID: "technical-org"}}, ReasonCodes: []string{"PROJECT_TERMINATED"}}
}

// TestTailRequiresPublishedExactPolicy 验证仍参与但没有已发布政策的尾款默认关闭。
// 参数：t 为测试上下文。
// 返回值：无。
func TestTailRequiresPublishedExactPolicy(t *testing.T) {
	s, db, p := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	p.references = []domain.ProjectReference{terminalFixtureReference()}
	_, e := s.Execute(context.Background(), a, "claim.submit", id, json.RawMessage(`{"command_id":"tail-no-policy","expected_version":1}`))
	var problem *domain.Problem
	if !errors.As(e, &problem) || problem.Code != "TAIL_POLICY_MISSING" {
		t.Fatalf("未明确以缺少政策默认拒绝:%v", e)
	}
}

// TestTailPublishedPolicyCreatesFinanceDirectorTask 验证精确政策和证据只能新增额外财务审批不能自动通过。
// 参数：t 为测试上下文。
// 返回值：无。
func TestTailPublishedPolicyCreatesFinanceDirectorTask(t *testing.T) {
	s, db, p := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	p.references = []domain.ProjectReference{terminalFixtureReference()}
	seedPolicy(t, db, "historical_tail_policies", domain.HistoricalTailPolicy{ID: "tail-policy", Version: 1, OrgID: "org", ProjectID: "p1", ExpenseCutoff: "2026-11-10", ValidFrom: "2026-01-01", Enabled: true, Published: true})
	seedPolicy(t, db, "assignments", domain.Assignment{ID: "finance-director", OrgID: "org", Role: "FINANCE_DIRECTOR", SubjectID: "director", ValidFrom: "2026-01-01"})
	seedPolicy(t, db, "scopes", domain.Scope{ID: "director-scope", SubjectID: "director", OrgIDs: []string{"org"}})
	c, _, _ := loadClaim(db, "t", id, false)
	c.Items[0].HistoricalTailReason = "终止前真实发生费用迟到票据"
	c.Items[0].HistoricalTailEvidenceFileIDs = []string{"tail-proof"}
	raw, _ := json.Marshal(c)
	db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw))
	submitted := runCommand(t, s, a, "claim.submit", id, map[string]any{"command_id": "tail-policy-submit", "expected_version": 1})
	if submitted["status"] != "IN_APPROVAL" {
		t.Fatal("尾款政策不应自动批准")
	}
	var tasks []store.Task
	db.Where("claim_id = ?", id).Order("node").Find(&tasks)
	if len(tasks) != 3 || tasks[1].NodeCode != "FINANCE_DIRECTOR" || tasks[2].NodeCode != "ACCOUNTANT" {
		t.Fatalf("尾款没有财务负责人额外必审节点:%+v", tasks)
	}
	var snap store.Snapshot
	db.First(&snap, "claim_id = ?", id)
	if VerifyApprovalSnapshot(json.RawMessage(snap.ContentJSON), snap.ContentHash) != nil {
		t.Fatal("尾款冻结快照不可复算")
	}
	seedPolicy(t, db, "scopes", domain.Scope{ID: "tail-acct-scope", SubjectID: "acct", OrgIDs: []string{"org"}})
	seedPolicy(t, db, "scopes", domain.Scope{ID: "tail-cashier-scope", SubjectID: "cashier", OrgIDs: []string{"org"}})
	version := int64(2)
	for _, task := range tasks {
		db.First(&task, "id = ?", task.ID)
		approver := domain.Actor{TenantID: "t", SubjectID: task.Assignee, Permissions: []string{"expense.claim.read", "expense.approval.act", "expense.finance.review"}}
		runCommand(t, s, approver, "approval.action", task.ID, map[string]any{"command_id": "tail-approve-" + task.ID, "expected_version": version, "task_version": task.Version, "round": 1, "action": "APPROVE"})
		version++
	}
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record"}}
	runCommand(t, s, cashier, "payment.record", id, map[string]any{"command_id": "approved-tail-payment", "expected_version": version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "approved-tail-transaction", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}})
	raw, _ = json.Marshal(map[string]any{"command_id": "paid-no-supplemental-approval", "expected_version": version + 1})
	_, err := s.Execute(context.Background(), a, "claim.submit", id, raw)
	var problem *domain.Problem
	if !errors.As(err, &problem) || problem.Code != "STATE_CONFLICT" {
		t.Fatal("已付款被新政策补审批")
	}

}

// travelFixtureSnapshot 提供保留真实内容摘要、尚无权威城市码的行程事实夹具。
// 参数：site 为现场一致性结果。
// 返回值：结构化事实，不是审批结果。
func travelFixtureSnapshot(site string) *domain.TravelSnapshot {
	yes := true
	value := domain.TravelSnapshot{Status: "KNOWN", Mode: "NEW", SourceChain: []domain.TravelSource{}, ActorInTravelers: &yes, SiteMatchStatus: site, DateMatchStatus: "MATCH", CityCodeStatus: "UNKNOWN", ReasonCodes: []string{"CITY_CODE_MAPPING_UNKNOWN", "PREPARATION_IS_NOT_TRAVEL_APPROVAL"}}
	raw, _ := json.Marshal(value)
	h := sha256.Sum256(raw)
	value.ContentDigest = hex.EncodeToString(h[:])
	return &value
}

// TestTravelMismatchCannotBeClearedByClient 验证前端false或伪造MATCH不能清除服务端行程矛盾。
// 参数：t 为测试上下文。
// 返回值：无。
func TestTravelMismatchCannotBeClearedByClient(t *testing.T) {
	s, db, p := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	var standard store.Config
	db.First(&standard, "id = ?", "standard")
	var policy map[string]any
	_ = json.Unmarshal([]byte(standard.ContentJSON), &policy)
	policy["requires_travel_facts"] = true
	raw, _ := json.Marshal(policy)
	db.Model(&standard).Update("content_json", string(raw))
	ref := terminalFixtureReference()
	ref.Eligible = true
	ref.ReasonCodes = nil
	ref.TravelSnapshot = travelFixtureSnapshot("MISMATCH")
	p.references = []domain.ProjectReference{ref}
	c, _, _ := loadClaim(db, "t", id, false)
	c.Items[0].TravelException = false
	c.Items[0].TravelAssessment = &domain.TravelAssessment{Status: "MATCH"}
	raw, _ = json.Marshal(c)
	db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw))
	if _, e := s.Execute(context.Background(), a, "claim.submit", id, json.RawMessage(`{"command_id":"fake-travel-match","expected_version":1}`)); e == nil {
		t.Fatal("客户端消除了MISMATCH且没有异常说明/证据仍提交")
	}
}

// enableTravelFixture 给隔离夹具标准启用显式行程事实要求并保存输入证据。
// 参数：t/db 为临时上下文，id 为草稿，confirmation 为真实人工声明，facts 为权威输入。
// 返回值：无。
func enableTravelFixture(t *testing.T, s *Service, id string, confirmation *domain.TravelConfirmation, facts *domain.TravelSnapshot) {
	t.Helper()
	db := s.db
	var row store.Config
	if e := db.First(&row, "id = ?", "standard").Error; e != nil {
		t.Fatal(e)
	}
	var values map[string]any
	_ = json.Unmarshal([]byte(row.ContentJSON), &values)
	values["requires_travel_facts"] = true
	raw, _ := json.Marshal(values)
	if e := db.Model(&row).Update("content_json", string(raw)).Error; e != nil {
		t.Fatal(e)
	}
	c, _, _ := loadClaim(db, "t", id, false)
	c.Items[0].TravelConfirmation = confirmation
	c.Items[0].TravelException = false
	c.Items[0].TravelAssessment = &domain.TravelAssessment{Status: "MATCH"}
	raw, _ = json.Marshal(c)
	db.Model(&store.Claim{}).Where("id = ?", id).Update("content_json", string(raw))
	ref := terminalFixtureReference()
	ref.Eligible = true
	ref.ReasonCodes = nil
	ref.TravelSnapshot = facts
	s.deps.Projects.(*projectFixture).references = []domain.ProjectReference{ref}
}

// TestUnknownTravelConfirmationKeepsUnknown 验证有证据人工确认不会把未知城市或行程伪装为匹配。
// 参数：t 为测试上下文。
// 返回值：无。
func TestUnknownTravelConfirmationKeepsUnknown(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	enableTravelFixture(t, s, id, &domain.TravelConfirmation{Decision: "CONFIRMED_UNKNOWN", Reason: "城市无权威代码，已按原始票据说明实际行程", EvidenceFileIDs: []string{"travel-proof"}}, travelFixtureSnapshot("MATCH"))
	runCommand(t, s, a, "claim.submit", id, map[string]any{"command_id": "unknown-confirmed", "expected_version": 1})
	c, _, _ := loadClaim(db, "t", id, false)
	item := c.Items[0]
	if !item.RequiresTravelFacts || item.TravelAssessment == nil || item.TravelAssessment.Status != "UNKNOWN" || !item.TravelAssessment.ConfirmedUnknown || item.TravelException {
		t.Fatalf("人工确认改变了真实未知状态或误判违规:%+v", item)
	}
	var tasks int64
	db.Model(&store.Task{}).Where("claim_id = ?", id).Count(&tasks)
	if tasks != 2 {
		t.Fatal("UNKNOWN不应被自动当作违规增加总监节点")
	}
}

// TestAcknowledgedMismatchIsDerivedAndAddsDirector 验证带证据说明仍保留矛盾并触发既有异常审批。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAcknowledgedMismatchIsDerivedAndAddsDirector(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	a, id := submissionFixture(t, db, true)
	seedPolicy(t, db, "assignments", domain.Assignment{ID: "technical-director", OrgID: "org", Role: "TECHNICAL_DIRECTOR", SubjectID: "director", ValidFrom: "2026-01-01"})
	enableTravelFixture(t, s, id, &domain.TravelConfirmation{Decision: "ACKNOWLEDGED_MISMATCH", Reason: "实际现场调整，已附证据", EvidenceFileIDs: []string{"travel-proof"}}, travelFixtureSnapshot("MISMATCH"))
	runCommand(t, s, a, "claim.submit", id, map[string]any{"command_id": "mismatch-acknowledged", "expected_version": 1})
	c, _, _ := loadClaim(db, "t", id, false)
	if !c.Items[0].TravelException || c.Items[0].TravelAssessment.Status != "MISMATCH" {
		t.Fatal("前端false消除了服务端矛盾结论")
	}
	var tasks int64
	db.Model(&store.Task{}).Where("claim_id = ? AND node_code = ?", id, "TECHNICAL_DIRECTOR").Count(&tasks)
	if tasks != 1 {
		t.Fatal("实际矛盾未增加技术总监必审")
	}
}

// TestTailNeverOverridesParticipationOrCutoff 验证发布政策也不能放宽失效参与者、其他障碍或费用截止日。
// 参数：t 为测试上下文。
// 返回值：无。
func TestTailNeverOverridesParticipationOrCutoff(t *testing.T) {
	for _, scenario := range []string{"participant", "other_blocker", "cutoff", "disabled", "unpublished"} {
		t.Run(scenario, func(t *testing.T) {
			s, db, p := setupWorkflow(t)
			a, id := submissionFixture(t, db, true)
			ref := terminalFixtureReference()
			cutoff := "2026-11-10"
			enabled := true
			if scenario == "participant" {
				ref.ParticipantEligible = false
			}
			if scenario == "other_blocker" {
				ref.ReasonCodes = append(ref.ReasonCodes, "SERVICE_ITEM_UNAVAILABLE")
			}
			if scenario == "cutoff" {
				cutoff = "2026-11-09"
			}
			if scenario == "disabled" {
				enabled = false
			}
			p.references = []domain.ProjectReference{ref}
			seedPolicy(t, db, "historical_tail_policies", domain.HistoricalTailPolicy{ID: "tail-rule", OrgID: "org", ProjectID: "p1", ExpenseCutoff: cutoff, ValidFrom: "2026-01-01", Enabled: enabled})
			if scenario == "unpublished" {
				db.Model(&store.Config{}).Where("id = ?", "tail-rule").Update("published", false)
			}
			_, err := s.Execute(context.Background(), a, "claim.submit", id, json.RawMessage(`{"command_id":"tail-boundary","expected_version":1}`))
			expected := map[string]string{"participant": "PROJECT_INELIGIBLE", "other_blocker": "PROJECT_INELIGIBLE", "cutoff": "TAIL_CUTOFF_EXCEEDED", "disabled": "TAIL_POLICY_MISSING", "unpublished": "TAIL_POLICY_MISSING"}[scenario]
			var problem *domain.Problem
			if !errors.As(err, &problem) || problem.Code != expected {
				t.Fatalf("%s未按权威边界拒绝:%v", scenario, err)
			}
			var snapshots int64
			db.Model(&store.Snapshot{}).Count(&snapshots)
			if snapshots != 0 {
				t.Fatal("被拒绝的例外留下批准快照")
			}
		})
	}
}

// TestNewTerminationAtPaymentRequiresReopen 验证付款时新发布尾款政策不能补批既有正常审批。
// 参数：t 为测试上下文。
// 返回值：无。
func TestNewTerminationAtPaymentRequiresReopen(t *testing.T) {
	s, db, c, _, _ := approvedControlFixture(t)
	s.deps.Projects.(*projectFixture).references = []domain.ProjectReference{terminalFixtureReference()}
	seedPolicy(t, db, "historical_tail_policies", domain.HistoricalTailPolicy{ID: "later-tail-rule", OrgID: "org", ProjectID: "p1", ExpenseCutoff: "2026-11-10", ValidFrom: "2026-01-01", Enabled: true})
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record"}}
	raw, _ := json.Marshal(map[string]any{"command_id": "no-retroactive-tail", "expected_version": c.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "tail-payment", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}})
	_, e := s.Execute(context.Background(), cashier, "payment.record", c.ID, raw)
	var problem *domain.Problem
	if !errors.As(e, &problem) || problem.Code != "REOPEN_REQUIRED" {
		t.Fatalf("没有要求重开新审批:%v", e)
	}
	var payments int64
	db.Model(&store.Payment{}).Count(&payments)
	if payments != 0 {
		t.Fatal("新终止导致未经审批尾款付款")
	}
}

// TestClaimEvidenceUploadDoesNotCreateInvoiceParse 验证费用证据独立用途及申请人权限不会混成发票解析。
// 参数：t 为测试上下文。
// 返回值：无。
func TestClaimEvidenceUploadDoesNotCreateInvoiceParse(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	gateway := &repeatUploadGateway{}
	s.deps.Files = gateway
	a := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.update"}}
	created, e := s.CreateUpload(context.Background(), a, json.RawMessage(`{"purpose":"reimbursement.claim.evidence","idempotency_key":"claim-proof","filename":"proof.pdf"}`))
	if e != nil {
		t.Fatal(e)
	}
	var upload struct {
		ID string `json:"document_id"`
	}
	_ = json.Unmarshal(created, &upload)
	if _, e = s.CompleteUpload(context.Background(), a, upload.ID); e != nil {
		t.Fatal(e)
	}
	var jobs int64
	db.Model(&store.Job{}).Where("resource_id = ?", upload.ID).Count(&jobs)
	if jobs != 0 {
		t.Fatal("费用证据被错误排成发票解析")
	}
	a.Permissions = []string{"expense.claim.read"}
	if _, e = s.CreateUpload(context.Background(), a, json.RawMessage(`{"purpose":"reimbursement.claim.evidence","idempotency_key":"no-write-proof","filename":"proof.pdf"}`)); e == nil {
		t.Fatal("仅查看能力也可上传费用证据")
	}
}
