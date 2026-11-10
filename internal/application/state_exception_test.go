package application

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
	"testing"
)

// approvedControlFixture 使用真实提交和审批命令建立待付款单及明确财务范围。
// 参数：t 为临时测试上下文。
// 返回值：服务、隔离库、待付款单、申请人和独立财务负责人。
func approvedControlFixture(t *testing.T) (*Service, *gorm.DB, domain.Claim, domain.Actor, domain.Actor) {
	t.Helper()
	s, db, _ := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	for _, subject := range []string{"acct", "head", "cashier"} {
		seedPolicy(t, db, "scopes", domain.Scope{ID: "scope-" + subject, SubjectID: subject, OrgIDs: []string{"org"}})
	}
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "fixture-submit", "expected_version": 1})
	var tasks []store.Task
	db.Where("claim_id = ?", id).Order("node").Find(&tasks)
	version := int64(2)
	for _, task := range tasks {
		db.First(&task, "id = ?", task.ID)
		actor := domain.Actor{TenantID: "t", SubjectID: task.Assignee, Permissions: []string{"expense.claim.read", "expense.approval.act", "expense.finance.review"}}
		runCommand(t, s, actor, "approval.action", task.ID, map[string]any{"command_id": "fixture-approve-" + task.ID, "expected_version": version, "task_version": task.Version, "round": 1, "action": "APPROVE"})
		version++
	}
	c, _, e := loadClaim(db, "t", id, false)
	if e != nil {
		t.Fatal(e)
	}
	head := domain.Actor{TenantID: "t", SubjectID: "head", Permissions: []string{"expense.claim.read", "expense.archive.read", "expense.claim.reopen", "expense.claim.approved_void", "expense.payment.exception.review"}}
	return s, db, c, app, head
}

// TestApprovedReopenKeepsOldSnapshotAndRequiresIndependentAuthority 验证重开必须独立授权且不改旧批准事实。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovedReopenKeepsOldSnapshotAndRequiresIndependentAuthority(t *testing.T) {
	s, db, c, app, head := approvedControlFixture(t)
	var original store.Snapshot
	db.First(&original, "claim_id = ? AND round = ?", c.ID, 1)
	request := map[string]any{"command_id": "reopen", "expected_version": c.Version, "reason": "收款资料需申请人重新确认"}
	raw, _ := json.Marshal(request)
	if _, e := s.Execute(context.Background(), app, "claim.reopen", c.ID, raw); e == nil {
		t.Fatal("申请人无重开能力仍被放行")
	}
	result := runCommand(t, s, head, "claim.reopen", c.ID, request)
	if result["status"] != "RETURNED" {
		t.Fatal("授权重开未回到申请人确认状态")
	}
	var retained store.Snapshot
	db.First(&retained, "id = ?", original.ID)
	if retained.ContentJSON != original.ContentJSON || VerifyApprovalSnapshot(json.RawMessage(retained.ContentJSON), retained.ContentHash) != nil {
		t.Fatal("重开改写或损坏旧批准快照")
	}
	runCommand(t, s, app, "claim.submit", c.ID, map[string]any{"command_id": "resubmit", "expected_version": c.Version + 1})
	var count int64
	db.Model(&store.Snapshot{}).Where("claim_id = ?", c.ID).Count(&count)
	if count != 2 {
		t.Fatal("重新提交未新增独立轮次")
	}
}

// TestApprovedControlRejectsSelfAndAlreadyPaid 验证有角色也不能自批或重开已付款事实。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovedControlRejectsSelfAndAlreadyPaid(t *testing.T) {
	s, db, c, app, head := approvedControlFixture(t)
	seedPolicy(t, db, "scopes", domain.Scope{ID: "self-scope", SubjectID: "app", OrgIDs: []string{"org"}})
	app.Permissions = append(app.Permissions, "expense.claim.reopen")
	raw, _ := json.Marshal(map[string]any{"command_id": "self-reopen", "expected_version": c.Version, "reason": "本人请求"})
	_, e := s.Execute(context.Background(), app, "claim.reopen", c.ID, raw)
	var problem *domain.Problem
	if !errors.As(e, &problem) || problem.Code != "SELF_FINANCIAL_ACTION_FORBIDDEN" {
		t.Fatalf("没有明确拒绝自批重开:%v", e)
	}
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.record", "expense.payment.read"}}
	runCommand(t, s, cashier, "payment.record", c.ID, map[string]any{"command_id": "paid", "expected_version": c.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "tx-paid", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}})
	raw, _ = json.Marshal(map[string]any{"command_id": "paid-reopen", "expected_version": c.Version + 1, "reason": "误重开已付款"})
	if _, e = s.Execute(context.Background(), head, "claim.reopen", c.ID, raw); e == nil {
		t.Fatal("已付款仍能重开")
	}
}

// TestPaymentExceptionIsIndependentAndReviewRequiresAnotherActor 验证异常事实不会伪造付款及项目净额。
// 参数：t 为测试上下文。
// 返回值：无。
func TestPaymentExceptionIsIndependentAndReviewRequiresAnotherActor(t *testing.T) {
	s, db, c, _, head := approvedControlFixture(t)
	recorder := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record", "expense.payment.exception.record", "expense.payment.exception.review"}}
	body := map[string]any{"command_id": "external-fact", "expected_version": c.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "outside-tx", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}, "reason": "外部实际已付但登记响应异常待核实"}
	recorded := runCommand(t, s, recorder, "payment.exception.record", c.ID, body)
	exceptionID := recorded["id"].(string)
	var payments, effects int64
	db.Model(&store.Payment{}).Count(&payments)
	db.Model(&store.Effect{}).Count(&effects)
	current, _, e := loadClaim(db, "t", c.ID, false)
	if e != nil {
		t.Fatal(e)
	}
	if payments != 0 || effects != 0 || current.Version != c.Version || current.Status != "APPROVED_PENDING_PAYMENT" || current.ProjectionRevision != 0 {
		t.Fatal("异常记录直接改变了付款或项目净额")
	}
	review := map[string]any{"command_id": "self-review", "expected_version": 1, "expected_claim_version": c.Version, "decision": "CONFIRM_CONFLICT", "reason": "核实外部支付事实", "evidence_file_ids": []string{"review-evidence"}}
	raw, _ := json.Marshal(review)
	if _, e = s.Execute(context.Background(), recorder, "payment.exception.review", exceptionID, raw); e == nil {
		t.Fatal("记录人自行核实异常被放行")
	}
	review["command_id"] = "head-review"
	verified := runCommand(t, s, head, "payment.exception.review", exceptionID, review)
	if verified["status"] != "VERIFIED_CONFLICT" {
		t.Fatal("核实未形成独立未解决状态")
	}
	db.Model(&store.Payment{}).Count(&payments)
	db.Model(&store.Effect{}).Count(&effects)
	if payments != 0 || effects != 0 {
		t.Fatal("核实动作伪造了付款效果")
	}
}

// TestApprovedVoidReleasesOnlyReservedInvoices 验证批准后作废只释放保留证据且旧审批仍可复算。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovedVoidReleasesOnlyReservedInvoices(t *testing.T) {
	s, db, c, _, head := approvedControlFixture(t)
	var original store.Snapshot
	db.First(&original, "claim_id = ?", c.ID)
	runCommand(t, s, head, "claim.approved_void", c.ID, map[string]any{"command_id": "approved-void", "expected_version": c.Version, "reason": "义务未发生且尚未付款"})
	var inv store.Invoice
	db.First(&inv, "id = ?", "invoice")
	if inv.ReservationState != "AVAILABLE" || inv.ReservationClaimID != "" {
		t.Fatal("未释放未消费的保留票据")
	}
	var retained store.Snapshot
	db.First(&retained, "id = ?", original.ID)
	if retained.ContentJSON != original.ContentJSON || VerifyApprovalSnapshot(json.RawMessage(retained.ContentJSON), retained.ContentHash) != nil {
		t.Fatal("作废修改了旧审批事实")
	}
}

// TestExceptionResolutionOnlyLinksExistingMatchingPayment 验证独立核实后通过合法同交易记录解除冲突。
// 参数：t 为测试上下文。
// 返回值：无。
func TestExceptionResolutionOnlyLinksExistingMatchingPayment(t *testing.T) {
	s, db, c, _, head := approvedControlFixture(t)
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record", "expense.payment.exception.record"}}
	body := map[string]any{"command_id": "fact", "expected_version": c.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "outside-payment", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}, "reason": "实际付款存在但未登记", "status": "PAID"}
	fact := runCommand(t, s, cashier, "payment.exception.record", c.ID, body)
	exceptionID := fact["id"].(string)
	var original store.PaymentExceptionFact
	db.First(&original, "id = ?", exceptionID)
	runCommand(t, s, head, "payment.exception.review", exceptionID, map[string]any{"command_id": "verify", "expected_version": 1, "expected_claim_version": c.Version, "decision": "CONFIRM_CONFLICT", "reason": "已核验凭证待按正常流程登记", "evidence_file_ids": []string{"head-evidence"}})
	invalid, _ := json.Marshal(map[string]any{"command_id": "invalid-link", "expected_version": 2, "expected_claim_version": c.Version, "decision": "RESOLVE_WITH_PAYMENT", "reason": "错误关联不应放行", "linked_payment_id": "not-a-payment", "evidence_file_ids": []string{"head-evidence"}})
	if _, e := s.Execute(context.Background(), head, "payment.exception.review", exceptionID, invalid); e == nil {
		t.Fatal("未发生正常付款仍可直接解除冲突")
	}
	body["command_id"] = "normal-payment"
	paid := runCommand(t, s, cashier, "payment.record", c.ID, body)
	request := map[string]any{"command_id": "resolve", "expected_version": 2, "expected_claim_version": c.Version + 1, "decision": "RESOLVE_WITH_PAYMENT", "reason": "核对同交易已正常登记", "linked_payment_id": paid["id"], "evidence_file_ids": []string{"head-evidence"}}
	resolved := runCommand(t, s, head, "payment.exception.review", exceptionID, request)
	if resolved["status"] != "RESOLVED" {
		t.Fatal("合法同交易未解除冲突")
	}
	s.deps.Files = nil
	runCommand(t, s, head, "payment.exception.review", exceptionID, request)
	var retained store.PaymentExceptionFact
	db.First(&retained, "id = ?", exceptionID)
	if original.ContentJSON != retained.ContentJSON || original.ContentHash != retained.ContentHash {
		t.Fatal("独立核实改写了原始异常事实")
	}
	var paymentCount, effectCount, reviewCount, projectEvents int64
	db.Model(&store.Payment{}).Count(&paymentCount)
	db.Model(&store.Effect{}).Count(&effectCount)
	db.Model(&store.PaymentExceptionReview{}).Count(&reviewCount)
	db.Model(&store.Outbox{}).Where("topic = ?", "project").Count(&projectEvents)
	if paymentCount != 1 || effectCount != 1 || projectEvents != 1 || reviewCount != 2 {
		t.Fatal("异常核实重复产生了财务或项目事件")
	}
	detail, e := s.Query(context.Background(), head, "payment-exception", exceptionID, nil)
	if e != nil {
		t.Fatal(e)
	}
	var view struct {
		Reviews []domain.PaymentExceptionReview `json:"reviews"`
	}
	if e = json.Unmarshal(detail, &view); e != nil || len(view.Reviews) != 2 {
		t.Fatal("核实详情缺少不可变历史")
	}
}

// TestReopenedReversedClaimRetainsConsumedEvidence 验证已全额冲正的同单可重审且消费证据不跨单释放。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReopenedReversedClaimRetainsConsumedEvidence(t *testing.T) {
	s, db, c, app, head := approvedControlFixture(t)
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record", "expense.payment.reversal.request"}}
	head.Permissions = append(head.Permissions, "expense.payment.reversal.approve")
	payment := runCommand(t, s, cashier, "payment.record", c.ID, map[string]any{"command_id": "payment-before-reopen", "expected_version": c.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "original-tx", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}})
	reversal := runCommand(t, s, cashier, "reversal.request", payment["id"].(string), map[string]any{"command_id": "refund-request", "expected_version": c.Version + 1, "reason": "已全额退回需重新确认内容", "fact_kind": "FULL_REFUND", "evidence_file_ids": []string{"refund"}})
	runCommand(t, s, head, "reversal.decide", reversal["id"].(string), map[string]any{"command_id": "refund-approved", "expected_version": 1, "approve": true, "reason": "独立核实退款事实"})
	runCommand(t, s, head, "claim.reopen", c.ID, map[string]any{"command_id": "reopen-refunded", "expected_version": c.Version + 2, "reason": "退款后重审费用分摊"})
	runCommand(t, s, app, "claim.submit", c.ID, map[string]any{"command_id": "resubmit-refunded", "expected_version": c.Version + 3})
	var inv store.Invoice
	db.First(&inv, "id = ?", "invoice")
	if inv.ReservationState != "CONSUMED" || inv.ReservationClaimID != c.ID {
		t.Fatal("重审错误地释放或降级了已消费证据")
	}
}

// TestExceptionResolutionRejectsDifferentTransaction 验证同金额不同流水不能假关联解除冲突。
// 参数：t 为测试上下文。
// 返回值：无。
func TestExceptionResolutionRejectsDifferentTransaction(t *testing.T) {
	s, db, c, _, head := approvedControlFixture(t)
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record", "expense.payment.exception.record"}}
	body := map[string]any{"command_id": "fact-different-tx", "expected_version": c.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "outside-one", "voucher_file_ids": []string{"voucher"}, "reason": "存在待核实原交易"}
	fact := runCommand(t, s, cashier, "payment.exception.record", c.ID, body)
	body["command_id"] = "another-normal-payment"
	body["transaction_no"] = "outside-two"
	body["payee_profile_id"] = "payee"
	paid := runCommand(t, s, cashier, "payment.record", c.ID, body)
	raw, _ := json.Marshal(map[string]any{"command_id": "wrong-link", "expected_version": 1, "expected_claim_version": c.Version + 1, "decision": "RESOLVE_WITH_PAYMENT", "reason": "不能只看金额相同", "linked_payment_id": paid["id"], "evidence_file_ids": []string{"proof"}})
	_, e := s.Execute(context.Background(), head, "payment.exception.review", fact["id"].(string), raw)
	var problem *domain.Problem
	if !errors.As(e, &problem) || problem.Code != "EXCEPTION_LINK_MISMATCH" {
		t.Fatalf("没有拒绝不同交易:%v", e)
	}
	var cursor store.PaymentExceptionCursor
	db.First(&cursor, "id = ?", fact["id"])
	if cursor.Version != 1 || cursor.Status != "PENDING" {
		t.Fatal("错误关联仍改变核实状态")
	}
}

// TestExceptionReviewAuditFailureRollsBackDecision 验证核实审计失败不会留下决定或游标推进。
// 参数：t 为测试上下文。
// 返回值：无。
func TestExceptionReviewAuditFailureRollsBackDecision(t *testing.T) {
	s, db, c, _, head := approvedControlFixture(t)
	recorder := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.exception.record"}}
	fact := runCommand(t, s, recorder, "payment.exception.record", c.ID, map[string]any{"command_id": "fact-before-audit-failure", "expected_version": c.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "CASH", "source_account_ref": "cash-ledger", "transaction_no": "cash-proof", "voucher_file_ids": []string{"voucher"}, "reason": "现金原始记录待核实"})
	if e := db.Exec(`CREATE TRIGGER reject_exception_audit BEFORE INSERT ON exp_outbox WHEN NEW.topic = 'audit' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]any{"command_id": "review-audit-fail", "expected_version": 1, "expected_claim_version": c.Version, "decision": "CONFIRM_CONFLICT", "reason": "核实不能缺审计", "evidence_file_ids": []string{"proof"}})
	if _, e := s.Execute(context.Background(), head, "payment.exception.review", fact["id"].(string), raw); e == nil {
		t.Fatal("审计失败仍报告核实成功")
	}
	var cursor store.PaymentExceptionCursor
	db.First(&cursor, "id = ?", fact["id"])
	var reviews int64
	db.Model(&store.PaymentExceptionReview{}).Count(&reviews)
	if cursor.Version != 1 || cursor.Status != "PENDING" || reviews != 0 {
		t.Fatal("失败核实留下了半条决定或状态推进")
	}
}

// TestExceptionResolvesViaApprovedReversalAndVoidRetainsConsumed 验证完整退款链可关联且作废不释放消费证据。
// 参数：t 为测试上下文。
// 返回值：无。
func TestExceptionResolvesViaApprovedReversalAndVoidRetainsConsumed(t *testing.T) {
	s, db, c, _, head := approvedControlFixture(t)
	head.Permissions = append(head.Permissions, "expense.payment.reversal.approve")
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record", "expense.payment.reversal.request", "expense.payment.exception.record"}}
	body := map[string]any{"command_id": "refunded-external-fact", "expected_version": c.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "refunded-original", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}, "reason": "付款及全额退款需关联核实"}
	fact := runCommand(t, s, cashier, "payment.exception.record", c.ID, body)
	body["command_id"] = "register-original"
	payment := runCommand(t, s, cashier, "payment.record", c.ID, body)
	request := runCommand(t, s, cashier, "reversal.request", payment["id"].(string), map[string]any{"command_id": "refund", "expected_version": c.Version + 1, "reason": "已确认全额退回", "fact_kind": "FULL_REFUND", "evidence_file_ids": []string{"refund"}})
	runCommand(t, s, head, "reversal.decide", request["id"].(string), map[string]any{"command_id": "refund-decision", "expected_version": 1, "approve": true, "reason": "独立确认全额退款"})
	var reversal store.Reversal
	db.Where("payment_id = ?", payment["id"]).First(&reversal)
	resolved := runCommand(t, s, head, "payment.exception.review", fact["id"].(string), map[string]any{"command_id": "resolve-with-refund", "expected_version": 1, "expected_claim_version": c.Version + 2, "decision": "RESOLVE_WITH_REVERSAL", "reason": "核对原交易和独立批准全额退款", "linked_payment_id": payment["id"], "linked_reversal_id": reversal.ID, "evidence_file_ids": []string{"review-proof"}})
	if resolved["status"] != "RESOLVED" {
		t.Fatal("合法全额退款链没有解除异常")
	}
	runCommand(t, s, head, "claim.approved_void", c.ID, map[string]any{"command_id": "void-after-refund", "expected_version": c.Version + 2, "reason": "原报销义务已终止"})
	var inv store.Invoice
	db.First(&inv, "id = ?", "invoice")
	if inv.ReservationState != "CONSUMED" || inv.ReservationClaimID != c.ID {
		t.Fatal("退款或作废把消费证据释放给了其他单据")
	}
	var effects, projectEvents int64
	db.Model(&store.Effect{}).Count(&effects)
	db.Model(&store.Outbox{}).Where("topic = ?", "project").Count(&projectEvents)
	if effects != 2 || projectEvents != 2 {
		t.Fatal("核实或作废改变了既有资金效果")
	}
}

// TestPaidExceptionCreatesIndependentArchiveVersions 验证异常追加档案不冒充新的财务投影版本。
// 参数：t 为测试上下文。
// 返回值：无。
func TestPaidExceptionCreatesIndependentArchiveVersions(t *testing.T) {
	s, db, c, _, head := approvedControlFixture(t)
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.record", "expense.payment.exception.record"}}
	body := map[string]any{"command_id": "normal-paid", "expected_version": c.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "paid-before-exception", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}}
	runCommand(t, s, cashier, "payment.record", c.ID, body)
	body["command_id"] = "paid-later-fact"
	body["expected_version"] = c.Version + 1
	body["reason"] = "付款后追加独立对账核实事实"
	fact := runCommand(t, s, cashier, "payment.exception.record", c.ID, body)
	runCommand(t, s, head, "payment.exception.review", fact["id"].(string), map[string]any{"command_id": "paid-later-review", "expected_version": 1, "expected_claim_version": c.Version + 1, "decision": "RESOLVE_WITH_PAYMENT", "reason": "核实同交易已有正常付款", "linked_payment_id": func() string { var row store.Payment; db.First(&row, "claim_id = ?", c.ID); return row.ID }(), "evidence_file_ids": []string{"proof"}})
	var archives []store.Archive
	db.Where("claim_id = ?", c.ID).Order("package_version").Find(&archives)
	if len(archives) != 3 {
		t.Fatalf("付款、事后异常和核实应有3版档案，实际%d", len(archives))
	}
	var jobs []store.Job
	db.Where("kind = ?", "ARCHIVE").Order("created_at,id").Find(&jobs)
	for index, archive := range archives {
		if archive.PackageVersion != int64(index+1) {
			t.Fatal("档案独立版本没有连续递增")
		}
	}
	for _, job := range jobs {
		var payload struct {
			Projection int64        `json:"projection_revision"`
			Document   domain.Claim `json:"document_snapshot"`
		}
		if e := json.Unmarshal([]byte(job.PayloadJSON), &payload); e != nil {
			t.Fatal(e)
		}
		if payload.Projection != 1 || payload.Document.ID != c.ID {
			t.Fatal("档案版本被当成财务revision或没有冻结当时文档")
		}
	}
	current, _, e := loadClaim(db, "t", c.ID, false)
	if e != nil {
		t.Fatal(e)
	}
	if current.ProjectionRevision != 1 {
		t.Fatal("异常档案递增了财务净额版本")
	}
}
