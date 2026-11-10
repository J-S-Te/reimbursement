package application

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// independentFinance 返回非风险记录人、非票据所有人的独立财务身份，仅凭双重财务能力关闭风险。
// 参数：无。
// 返回值：持有票据更正与财务复核能力的独立财务身份。
func independentFinance() domain.Actor {
	return domain.Actor{TenantID: "t", SubjectID: "finance-reviewer", Permissions: []string{"expense.claim.read", "expense.invoice.read", "expense.invoice.correct", "expense.finance.review"}}
}

// unclaimedRiskFixture 建立未被任何单据占用的票据风险夹具：草稿单只在内容中引用票据。
// 参数：t 为隔离应用测试上下文。
// 返回值：服务、隔离库、票据所有人及草稿单标识。
func unclaimedRiskFixture(t *testing.T) (*Service, *gorm.DB, domain.Actor, string) {
	t.Helper()
	s, db, _ := setupWorkflow(t)
	app, claimID := submissionFixture(t, db, true)
	app.Permissions = append(app.Permissions, "expense.invoice.review", "expense.invoice.read")
	var invoice store.Invoice
	if e := db.First(&invoice, "tenant_id = ? AND id = ?", "t", "invoice").Error; e != nil {
		t.Fatal(e)
	}
	if invoice.ReservationClaimID != "" || invoice.ReservationState != "AVAILABLE" {
		t.Fatalf("夹具票据已被单据占用：%+v", invoice)
	}
	return s, db, app, claimID
}

// claimedRiskFixture 提交草稿单占用票据后由票主登记风险，保持已关联单据的OPEN案件。
// 参数：t 为隔离应用测试上下文。
// 返回值：服务、隔离库、票据所有人、占用票据的单据及风险案件标识。
func claimedRiskFixture(t *testing.T) (*Service, *gorm.DB, domain.Actor, string, string) {
	t.Helper()
	s, db, app, claimID := unclaimedRiskFixture(t)
	runCommand(t, s, app, "claim.submit", claimID, map[string]any{"command_id": "claimed-submit", "expected_version": 1})
	var invoice store.Invoice
	if e := db.First(&invoice, "tenant_id = ? AND id = ?", "t", "invoice").Error; e != nil {
		t.Fatal(e)
	}
	if invoice.ReservationClaimID != claimID {
		t.Fatalf("夹具票据未由单据占用：%+v", invoice)
	}
	return s, db, app, claimID, recordOpenRisk(t, s, app, "claimed-risk", invoice.Version)
}

// recordOpenRisk 由票据所有人登记真实红字风险并断言案件初始为OPEN。
// 参数：t 为测试上下文，s 为服务，a 为票主，commandID 为命令标识，invoiceVersion 为票据版本。
// 返回值：风险案件标识。
func recordOpenRisk(t *testing.T, s *Service, a domain.Actor, commandID string, invoiceVersion int64) string {
	t.Helper()
	risk := runCommand(t, s, a, "invoice.risk.record", "invoice", map[string]any{
		"command_id": commandID, "expected_version": invoiceVersion, "risk_kind": "RED_LETTER",
		"original_invoice_id": "invoice", "evidence_file_ids": []string{"file"}, "reason": "收到供应商红字通知",
	})
	id, _ := risk["id"].(string)
	if id == "" || risk["status"] != "OPEN" {
		t.Fatalf("未返回OPEN风险案件：%v", risk)
	}
	return id
}

// invoiceRiskRow 读取票据风险案件的当前持久化事实。
// 参数：t 为测试上下文，db 为隔离库，id 为案件标识。
// 返回值：风险案件行。
func invoiceRiskRow(t *testing.T, db *gorm.DB, id string) store.InvoiceRisk {
	t.Helper()
	var row store.InvoiceRisk
	if e := db.First(&row, "tenant_id = ? AND id = ?", "t", id).Error; e != nil {
		t.Fatal(e)
	}
	return row
}

// TestUnclaimedInvoiceRiskClosedByIndependentFinance 验证未被单据占用的票据风险可由独立财务闭环为排除或确认。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestUnclaimedInvoiceRiskClosedByIndependentFinance(t *testing.T) {
	for _, resolution := range []string{"DISMISSED", "CONFIRMED_RISK"} {
		t.Run(resolution, func(t *testing.T) {
			s, db, app, claimID := unclaimedRiskFixture(t)
			riskID := recordOpenRisk(t, s, app, "unclaimed-risk-"+resolution, 1)
			var impacts int64
			if e := db.Model(&store.InvoiceControlImpact{}).Where("tenant_id = ? AND control_id = ?", "t", riskID).Count(&impacts).Error; e != nil || impacts != 0 {
				t.Fatalf("无单据引用的风险不应建立阻断影响：%d %v", impacts, e)
			}
			closed := runCommand(t, s, independentFinance(), "invoice.risk.close", riskID, map[string]any{"command_id": "unclaimed-close-" + resolution, "expected_version": 1, "resolution": resolution, "reason": "与供应商核实真实红字事实"})
			if closed["status"] != "CLOSED" || closed["resolution"] != resolution {
				t.Fatalf("独立财务未闭环风险案件：%v", closed)
			}
			row := invoiceRiskRow(t, db, riskID)
			if row.Status != "CLOSED" || row.Resolution != resolution || row.Reviewer != "finance-reviewer" || row.Version != 2 {
				t.Fatalf("风险案件持久化事实不符：%+v", row)
			}
			var impactsAfter int64
			db.Model(&store.InvoiceControlImpact{}).Where("tenant_id = ? AND control_id = ?", "t", riskID).Count(&impactsAfter)
			if impactsAfter != 0 {
				t.Fatalf("无单据风险关闭不应写入阻断影响：%d", impactsAfter)
			}
			if resolution != "CONFIRMED_RISK" {
				return
			}
			// 确认真实风险的闭环不等于解除阻断，引用该票据的单据仍被拒绝。
			claim, _, e := loadClaim(db, "t", claimID, false)
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(map[string]any{"command_id": "confirmed-risk-submit", "expected_version": claim.Version})
			_, e = s.Execute(context.Background(), app, "claim.submit", claimID, raw)
			sourceErrorCode(t, e, "INVOICE_RISK_REVIEW_REQUIRED", 409)
		})
	}
}

// TestInvoiceRiskRecorderCannotCloseOwnCase 验证风险记录人不能自行关闭，未占用与已占用票据均保持自审拒绝。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestInvoiceRiskRecorderCannotCloseOwnCase(t *testing.T) {
	// 无单据引用：记录人本人尝试关闭被票据所有人自审边界拒绝。
	s, db, app, _ := unclaimedRiskFixture(t)
	riskID := recordOpenRisk(t, s, app, "recorder-unclaimed-risk", 1)
	self := app
	self.Permissions = append(self.Permissions, "expense.invoice.correct", "expense.finance.review", "expense.scope.tenant_all")
	raw, _ := json.Marshal(map[string]any{"command_id": "recorder-unclaimed-close", "expected_version": 1, "resolution": "DISMISSED", "reason": "自行排除风险"})
	_, e := s.Execute(context.Background(), self, "invoice.risk.close", riskID, raw)
	sourceErrorCode(t, e, "SELF_FINANCIAL_ACTION_FORBIDDEN", 403)
	if row := invoiceRiskRow(t, db, riskID); row.Status != "OPEN" {
		t.Fatalf("自审拒绝仍关闭了风险：%+v", row)
	}
	// 已关联单据：既有自审拒绝保持不变，记录人具备全部财务能力也不能关闭。
	s2, db2, app2, _, claimedRiskID := claimedRiskFixture(t)
	self2 := app2
	self2.Permissions = append(self2.Permissions, "expense.invoice.correct", "expense.finance.review", "expense.scope.tenant_all")
	raw, _ = json.Marshal(map[string]any{"command_id": "recorder-claimed-close", "expected_version": 1, "resolution": "DISMISSED", "reason": "自行排除风险"})
	_, e = s2.Execute(context.Background(), self2, "invoice.risk.close", claimedRiskID, raw)
	sourceErrorCode(t, e, "SELF_FINANCIAL_ACTION_FORBIDDEN", 403)
	if row := invoiceRiskRow(t, db2, claimedRiskID); row.Status != "OPEN" {
		t.Fatalf("已关联单据的自审拒绝仍关闭了风险：%+v", row)
	}
}

// TestInvoiceOwnerCannotCloseInvoiceRisk 验证票据所有人即使具备双重财务能力也不能关闭他人登记的本人票据风险。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestInvoiceOwnerCannotCloseInvoiceRisk(t *testing.T) {
	s, db, app, _ := unclaimedRiskFixture(t)
	const riskID = "imported-owner-risk"
	if e := db.Create(&store.InvoiceRisk{ID: riskID, TenantID: "t", InvoiceID: "invoice", OriginalInvoiceID: "invoice", Recorder: "external-auditor", Kind: "RED_LETTER", Status: "OPEN", Version: 1, ContentJSON: `{"reason":"历史导入风险","evidence_file_ids":["file"],"invoice_version":1}`}).Error; e != nil {
		t.Fatal(e)
	}
	owner := app
	owner.Permissions = append(owner.Permissions, "expense.invoice.correct", "expense.finance.review", "expense.scope.tenant_all")
	raw, _ := json.Marshal(map[string]any{"command_id": "owner-close-risk", "expected_version": 1, "resolution": "DISMISSED", "reason": "票主自行排除"})
	_, e := s.Execute(context.Background(), owner, "invoice.risk.close", riskID, raw)
	sourceErrorCode(t, e, "SELF_FINANCIAL_ACTION_FORBIDDEN", 403)
	if row := invoiceRiskRow(t, db, riskID); row.Status != "OPEN" {
		t.Fatalf("票主自审拒绝仍关闭了风险：%+v", row)
	}
	// 非记录人、非票主的独立财务仍可关闭同一案件，证明拒绝原因是票主身份而不是权限或案件状态。
	runCommand(t, s, independentFinance(), "invoice.risk.close", riskID, map[string]any{"command_id": "owner-close-risk-finance", "expected_version": 1, "resolution": "DISMISSED", "reason": "独立财务核实后排除"})
	if row := invoiceRiskRow(t, db, riskID); row.Status != "CLOSED" || row.Reviewer != "finance-reviewer" {
		t.Fatalf("独立财务未关闭同一案件：%+v", row)
	}
}

// TestUnclaimedInvoiceRiskClosureUnblocksReferencingClaim 验证无单据引用的风险闭环后，引用该票据的单据不再被死锁阻断。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestUnclaimedInvoiceRiskClosureUnblocksReferencingClaim(t *testing.T) {
	s, db, app, claimID := unclaimedRiskFixture(t)
	riskID := recordOpenRisk(t, s, app, "deadlock-risk", 1)
	claim, _, e := loadClaim(db, "t", claimID, false)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]any{"command_id": "deadlock-submit-blocked", "expected_version": claim.Version})
	_, e = s.Execute(context.Background(), app, "claim.submit", claimID, raw)
	sourceErrorCode(t, e, "INVOICE_RISK_REVIEW_REQUIRED", 409)
	blocked, _, e := loadClaim(db, "t", claimID, false)
	if e != nil || blocked.Version != claim.Version || blocked.Status != claim.Status || blocked.CurrentRound != claim.CurrentRound {
		t.Fatalf("被阻断的提交改变单据事实：%s v%d r%d %v", blocked.Status, blocked.Version, blocked.CurrentRound, e)
	}
	runCommand(t, s, independentFinance(), "invoice.risk.close", riskID, map[string]any{"command_id": "deadlock-risk-close", "expected_version": 1, "resolution": "DISMISSED", "reason": "与供应商核实未开具红字"})
	runCommand(t, s, app, "claim.submit", claimID, map[string]any{"command_id": "deadlock-submit-passed", "expected_version": claim.Version})
	final, _, e := loadClaim(db, "t", claimID, false)
	if e != nil || final.Status != "IN_APPROVAL" || final.CurrentRound != claim.CurrentRound+1 {
		t.Fatalf("风险闭环后提交未进入新轮次：%s r%d %v", final.Status, final.CurrentRound, e)
	}
	var invoice store.Invoice
	if e = db.First(&invoice, "tenant_id = ? AND id = ?", "t", "invoice").Error; e != nil {
		t.Fatal(e)
	}
	if invoice.ReservationClaimID != claimID || invoice.ReservationState != "RESERVED" {
		t.Fatalf("风险闭环后票据未正常占用：%+v", invoice)
	}
}

// TestClaimLinkedInvoiceRiskClosureKeepsScopeAndBlockEnforcement 验证有单据关联时的阻断与财务范围校验不变。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestClaimLinkedInvoiceRiskClosureKeepsScopeAndBlockEnforcement(t *testing.T) {
	s, db, claim, app, _ := approvedControlFixture(t)
	app.Permissions = append(app.Permissions, "expense.invoice.review", "expense.invoice.read")
	var invoice store.Invoice
	if e := db.First(&invoice, "tenant_id = ? AND id = ?", "t", "invoice").Error; e != nil {
		t.Fatal(e)
	}
	riskID := recordOpenRisk(t, s, app, "claimed-block-risk", invoice.Version)
	var impact store.InvoiceControlImpact
	if e := db.First(&impact, "tenant_id = ? AND control_id = ? AND kind = ?", "t", riskID, "RISK").Error; e != nil || impact.Status != "FINANCE_REVIEW_REQUIRED" || impact.ClaimID != claim.ID {
		t.Fatalf("已关联单据的风险未建立阻断影响：%+v %v", impact, e)
	}
	cashier := domain.Actor{TenantID: "t", SubjectID: "cashier", Permissions: []string{"expense.claim.read", "expense.payment.read", "expense.payment.record"}}
	payment := map[string]any{"command_id": "claimed-block-payment", "expected_version": claim.Version, "amount_minor": "100", "currency": "CNY", "paid_date": "2026-11-10", "method": "BANK", "source_account_ref": "source", "transaction_no": "txn-risk", "payee_profile_id": "payee", "voucher_file_ids": []string{"voucher"}}
	raw, _ := json.Marshal(payment)
	_, e := s.Execute(context.Background(), cashier, "payment.record", claim.ID, raw)
	sourceErrorCode(t, e, "INVOICE_RISK_REVIEW_REQUIRED", 409)
	var payments int64
	if e = db.Model(&store.Payment{}).Count(&payments).Error; e != nil || payments != 0 {
		t.Fatalf("风险阻断仍写入付款事实：%d %v", payments, e)
	}
	// 未配置本地财务范围的双重能力人仍不能关闭已关联单据的风险。
	unscoped := domain.Actor{TenantID: "t", SubjectID: "unscoped-finance", Permissions: []string{"expense.claim.read", "expense.invoice.read", "expense.invoice.correct", "expense.finance.review"}}
	raw, _ = json.Marshal(map[string]any{"command_id": "claimed-unscoped-close", "expected_version": 1, "resolution": "DISMISSED", "reason": "缺少本地财务范围"})
	_, e = s.Execute(context.Background(), unscoped, "invoice.risk.close", riskID, raw)
	sourceErrorCode(t, e, "FORBIDDEN", 403)
	if row := invoiceRiskRow(t, db, riskID); row.Status != "OPEN" {
		t.Fatalf("无财务范围仍关闭了已关联单据的风险：%+v", row)
	}
	// 已配置财务范围的独立财务关闭后，同一付款才可放行。
	scoped := domain.Actor{TenantID: "t", SubjectID: "acct", Permissions: []string{"expense.claim.read", "expense.invoice.read", "expense.invoice.correct", "expense.finance.review"}}
	runCommand(t, s, scoped, "invoice.risk.close", riskID, map[string]any{"command_id": "claimed-scoped-close", "expected_version": 1, "resolution": "DISMISSED", "reason": "与供应商核实后排除"})
	payment["command_id"] = "claimed-payment-after-close"
	paid := runCommand(t, s, cashier, "payment.record", claim.ID, payment)
	if paid["id"] == nil {
		t.Fatalf("风险闭环后付款未放行：%v", paid)
	}
	if e = db.Model(&store.Payment{}).Count(&payments).Error; e != nil || payments != 1 {
		t.Fatalf("风险闭环后未写入唯一付款事实：%d %v", payments, e)
	}
}
