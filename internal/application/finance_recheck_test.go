package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// financeFixture 建立真实提交并完成业务节点的独立会计复检夹具。
// 参数：t 为隔离测试上下文。
// 返回值：应用服务、临时库、会计身份和当前会计任务。
func financeFixture(t *testing.T) (*Service, *gorm.DB, domain.Actor, store.Task) {
	t.Helper()
	return financeVersionFixture(t, 1)
}

// financeVersionFixture 以指定原标准版本建立真实提交及会计待办。
// 参数：t 为测试上下文，version 为标准在提交前的真实版本，零用于遗留字段兼容。
// 返回值：应用服务、临时库、会计身份和会计任务。
func financeVersionFixture(t *testing.T, version int64) (*Service, *gorm.DB, domain.Actor, store.Task) {
	t.Helper()
	s, db, _ := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	var config store.Config
	if err := db.First(&config, "id = ?", "standard").Error; err != nil {
		t.Fatal(err)
	}
	var standard domain.Standard
	if err := json.Unmarshal([]byte(config.ContentJSON), &standard); err != nil {
		t.Fatal(err)
	}
	standard.Version = version
	content, _ := json.Marshal(standard)
	if err := db.Model(&config).Update("content_json", string(content)).Error; err != nil {
		t.Fatal(err)
	}
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "finance-submit", "expected_version": 1})
	var lead store.Task
	if err := db.Where("claim_id = ? AND status = ?", id, "PENDING").First(&lead).Error; err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{TenantID: "t", SubjectID: lead.Assignee, Permissions: []string{"expense.approval.act", "expense.claim.read"}}
	runCommand(t, s, actor, "approval.action", lead.ID, map[string]any{"command_id": "finance-business", "expected_version": 2, "task_version": lead.Version, "round": 1, "action": "APPROVE"})
	var task store.Task
	if err := db.Where("claim_id = ? AND node_code = ?", id, "ACCOUNTANT").First(&task).Error; err != nil {
		t.Fatal(err)
	}
	actor.SubjectID = task.Assignee
	actor.Permissions = append(actor.Permissions, "expense.finance.review")
	return s, db, actor, task
}

// TestFinanceRecheckLegacyStandardVersionUsesFrozenID 防止旧快照缺少新版本字段就被永久阻断。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckLegacyStandardVersionUsesFrozenID(t *testing.T) {
	s, db, actor, task := financeVersionFixture(t, 0)
	// 旧提交仅冻结不可变标准ID与金额，新字段缺省为零；原标准行本身已有版本1。
	var config store.Config
	if err := db.First(&config, "id = ?", "standard").Error; err != nil {
		t.Fatal(err)
	}
	var original domain.Standard
	if err := json.Unmarshal([]byte(config.ContentJSON), &original); err != nil {
		t.Fatal(err)
	}
	original.Version = 1
	content, _ := json.Marshal(original)
	if err := db.Model(&config).Update("content_json", string(content)).Error; err != nil {
		t.Fatal(err)
	}
	seedPolicy(t, db, "standards", domain.Standard{ID: "legacy-new-policy", Version: 2, OrgID: "org", FeeType: "TRAVEL", Grade: "G", Limit: 1, Unit: "PER_OCCURRENCE", Priority: 99, ValidFrom: "2026-01-01"})
	r := runCommand(t, s, actor, "approval.action", task.ID, financeBody(task))
	if r["action"] != "APPROVE" || r["status"] != "APPROVED_PENDING_PAYMENT" {
		t.Fatalf("遗留快照未按冻结ID与金额兼容: %+v", r)
	}
}

// financeBody 返回绑定当前夹具版本的会计通过请求。
// 参数：task 为当前会计任务。
// 返回值：原始通过命令，不随复检结论修改。
func financeBody(task store.Task) map[string]any {
	return map[string]any{"command_id": "finance-approve", "expected_version": 3, "task_version": task.Version, "round": 1, "action": "APPROVE"}
}

// TestFinanceRecheckReturnsRealActionAndReplays 防止不合规仍通过、假退回和幂等丢失实际结论。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestFinanceRecheckReturnsRealActionAndReplays(t *testing.T) {
	for _, problem := range []string{"MISMATCH", "OPEN_RISK", "WRONG_RESERVATION"} {
		t.Run(problem, func(t *testing.T) {
			s, db, actor, task := financeFixture(t)
			var invoice store.Invoice
			if err := db.First(&invoice, "id = ?", "invoice").Error; err != nil {
				t.Fatal(err)
			}
			switch problem {
			case "MISMATCH":
				var inv domain.Invoice
				if err := json.Unmarshal([]byte(invoice.ContentJSON), &inv); err != nil {
					t.Fatal(err)
				}
				inv.VerificationStatus = "MISMATCH"
				raw, _ := json.Marshal(inv)
				if err := db.Model(&invoice).Update("content_json", string(raw)).Error; err != nil {
					t.Fatal(err)
				}
			case "OPEN_RISK":
				if err := db.Create(&store.InvoiceRisk{ID: "risk", TenantID: "t", InvoiceID: "invoice", OriginalInvoiceID: "invoice", Status: "OPEN", Version: 1, ContentJSON: `{}`}).Error; err != nil {
					t.Fatal(err)
				}
			case "WRONG_RESERVATION":
				if err := db.Model(&invoice).Update("reservation_claim_id", "another-claim").Error; err != nil {
					t.Fatal(err)
				}
			}
			other := task
			other.ID, other.Assignee = "second-accountant", "acct2"
			if err := db.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			body := financeBody(task)
			result := runCommand(t, s, actor, "approval.action", task.ID, body)
			if result["action"] != "RETURN" || result["auto_returned"] != true || result["status"] != "RETURNED" {
				t.Fatalf("不合规未真实退回: %+v", result)
			}
			if len(result["compliance_codes"].([]any)) == 0 || result["reason"] == "" {
				t.Fatalf("缺少复检结论: %+v", result)
			}
			var action store.Action
			if err := db.Where("task_id = ?", task.ID).First(&action).Error; err != nil || action.Action != "RETURN" {
				t.Fatalf("未写真实退回动作: %+v %v", action, err)
			}
			var detail store.ActionDetail
			if err := db.First(&detail, "action_id = ?", action.ID).Error; err != nil {
				t.Fatal(err)
			}
			if problem == "OPEN_RISK" && !strings.Contains(detail.ContentJSON, `"id":"risk"`) {
				t.Fatalf("退回证据没有绑定真实风险案件: %s", detail.ContentJSON)
			}
			if err := db.First(&other, "id = ?", other.ID).Error; err != nil || other.Status != "CANCELLED" {
				t.Fatalf("同轮会计未取消: %+v %v", other, err)
			}
			if err := db.First(&invoice, "id = ?", "invoice").Error; err != nil {
				t.Fatal(err)
			}
			if problem != "WRONG_RESERVATION" && (invoice.ReservationState != "AVAILABLE" || invoice.ReservationClaimID != "") {
				t.Fatalf("退回未释放票据: %+v", invoice)
			}
			s.deps.Files = nil
			replayed := runCommand(t, s, actor, "approval.action", task.ID, body)
			if replayed["action"] != "RETURN" || replayed["claim_version"] != result["claim_version"] {
				t.Fatalf("重放改变真实结论: %+v", replayed)
			}
		})
	}
}

type financeUnavailableFiles struct{ filesFixture }

// Validate 模拟文件依赖故障，禁止将服务不可用伪装为业务退回。
// 参数：上下文、文件所有人、文件标识及用途为已授权复核范围。
// 返回值：明确的文件网关不可用错误。
func (financeUnavailableFiles) Validate(context.Context, domain.Actor, []string, string) error {
	return domain.Fail("FILE_GATEWAY_UNAVAILABLE", "文件服务不可用", 503)
}

// TestFinanceRecheckDependencyFailureHasNoWrites 防止依赖故障伪造退回或提前写审批。
// 参数：t 为隔离应用测试上下文。
// 返回值：无。
func TestFinanceRecheckDependencyFailureHasNoWrites(t *testing.T) {
	s, db, actor, task := financeFixture(t)
	s.deps.Files = financeUnavailableFiles{}
	raw, _ := json.Marshal(financeBody(task))
	_, err := s.Execute(context.Background(), actor, "approval.action", task.ID, raw)
	var p *domain.Problem
	if !errors.As(err, &p) || p.Status != 503 {
		t.Fatalf("依赖故障未保持503: %v", err)
	}
	var count int64
	if err := db.Model(&store.Action{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("依赖失败留下动作: %d %v", count, err)
	}
	c, _, err := loadClaim(db, "t", task.ClaimID, false)
	if err != nil || c.Version != 3 || c.Status != "IN_APPROVAL" {
		t.Fatalf("依赖失败改写单据: %+v %v", c, err)
	}
}

// TestFinanceRecheckAllowsNewManualVerification 防止真实人工核验仅递增版本就被错判重大更正。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckAllowsNewManualVerification(t *testing.T) {
	s, db, actor, task := financeFixture(t)
	actor.Permissions = append(actor.Permissions, "expense.invoice.verify", "expense.invoice.read")
	runCommand(t, s, actor, "invoice.verify", "invoice", map[string]any{"command_id": "verify-before-finance", "expected_version": 2, "verification_method": "MANUAL", "verification_result": "PASS", "reason": "独立核对原件", "evidence_file_ids": []string{"file"}})
	r := runCommand(t, s, actor, "approval.action", task.ID, financeBody(task))
	if r["action"] != "APPROVE" || r["status"] != "APPROVED_PENDING_PAYMENT" {
		t.Fatalf("合法人工核验不应被退回: %+v", r)
	}
	var count int64
	if err := db.Model(&store.Action{}).Where("action = ?", "RETURN").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("合法核验产生退回: %d %v", count, err)
	}
}

// TestFinanceRecheckFrozenStandardSurvivesReplacement 防止新发布或停用标准追溯否决已提交单据。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckFrozenStandardSurvivesReplacement(t *testing.T) {
	s, db, actor, task := financeFixture(t)
	seedPolicy(t, db, "standards", domain.Standard{ID: "new-strict-standard", Version: 2, OrgID: "org", FeeType: "TRAVEL", Grade: "G", Unit: "PER_OCCURRENCE", Limit: 1, ValidFrom: "2026-01-01", Priority: 99})
	if err := db.Create(&store.PolicyLifecycle{ID: "retired-standard", TenantID: "t", ConfigID: "standard"}).Error; err != nil {
		t.Fatal(err)
	}
	r := runCommand(t, s, actor, "approval.action", task.ID, financeBody(task))
	if r["action"] != "APPROVE" || r["status"] != "APPROVED_PENDING_PAYMENT" {
		t.Fatalf("新标准不应追溯否决旧单: %+v", r)
	}
}

// assertFinanceUntouched 验证失败没有创建会计事实、命令回执或释放占用。
// 参数：t 为测试上下文，db 为临时库，task 为失败前会计任务。
// 返回值：无。
func assertFinanceUntouched(t *testing.T, db *gorm.DB, task store.Task) {
	t.Helper()
	c, _, err := loadClaim(db, "t", task.ClaimID, false)
	if err != nil || c.Version != 3 || c.Status != "IN_APPROVAL" {
		t.Fatalf("失败改写单据: %+v %v", c, err)
	}
	var current store.Task
	if err := db.First(&current, "id = ?", task.ID).Error; err != nil || current.Status != "PENDING" || current.Version != task.Version {
		t.Fatalf("失败改写任务: %+v %v", current, err)
	}
	var count int64
	if err := db.Model(&store.Action{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("失败留下审批动作: %d %v", count, err)
	}
	if err := db.Model(&store.Command{}).Where("command_id = ?", "finance-approve").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("失败冻结成功回执: %d %v", count, err)
	}
	var inv store.Invoice
	if err := db.First(&inv, "id = ?", "invoice").Error; err != nil || inv.ReservationClaimID != task.ClaimID || inv.ReservationState != "RESERVED" {
		t.Fatalf("失败释放了票据: %+v %v", inv, err)
	}
}

// TestFinanceRecheckRejectsAuthorizationStaleAndCorruption 防止权限、过期及账本异常被伪装自动退回。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckRejectsAuthorizationStaleAndCorruption(t *testing.T) {
	for _, condition := range []string{"PERMISSION", "SCOPE", "STALE", "ROUND", "SNAPSHOT", "INVOICE_JSON", "STANDARD_VERSION", "STANDARD_LIMIT", "TASK_NODE"} {
		t.Run(condition, func(t *testing.T) {
			s, db, actor, task := financeFixture(t)
			body := financeBody(task)
			status := 409
			switch condition {
			case "PERMISSION":
				actor.Permissions = []string{"expense.claim.read"}
				status = 403
			case "SCOPE":
				if err := db.Where("kind = ?", "scopes").Delete(&store.Config{}).Error; err != nil {
					t.Fatal(err)
				}
				status = 403
			case "STALE":
				body["expected_version"] = 2
			case "ROUND":
				body["round"] = 2
			case "SNAPSHOT":
				if err := db.Model(&store.Snapshot{}).Where("claim_id = ?", task.ClaimID).Update("content_hash", "corrupt").Error; err != nil {
					t.Fatal(err)
				}
			case "INVOICE_JSON":
				if err := db.Model(&store.Invoice{}).Where("id = ?", "invoice").Update("content_json", "{").Error; err != nil {
					t.Fatal(err)
				}
				status = 0
			case "STANDARD_VERSION":
				if err := db.Model(&store.Config{}).Where("id = ?", "standard").Update("content_json", `{"id":"standard","version":99}`).Error; err != nil {
					t.Fatal(err)
				}
			case "STANDARD_LIMIT":
				if err := db.Model(&store.Config{}).Where("id = ?", "standard").Update("content_json", `{"id":"standard","version":1,"limit_minor":"0"}`).Error; err != nil {
					t.Fatal(err)
				}
			case "TASK_NODE":
				var dto domain.Task
				if err := json.Unmarshal([]byte(task.ContentJSON), &dto); err != nil {
					t.Fatal(err)
				}
				dto.NodeCode = "BUSINESS_REVIEW"
				content, _ := json.Marshal(dto)
				if err := db.Model(&store.Task{}).Where("id = ?", task.ID).Update("content_json", string(content)).Error; err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(body)
			_, err := s.Execute(context.Background(), actor, "approval.action", task.ID, raw)
			if err == nil {
				t.Fatal("非法命令被受理")
			}
			var p *domain.Problem
			if status > 0 && (!errors.As(err, &p) || p.Status != status) {
				t.Fatalf("错误被转换或丢失: %v", err)
			}
			assertFinanceUntouched(t, db, task)
		})
	}
}

// TestFinanceRecheckAuditFailureRollsBackReturn 防止审计失败后退回、取消、释放与通知部分提交。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckAuditFailureRollsBackReturn(t *testing.T) {
	s, db, actor, task := financeFixture(t)
	if err := db.Create(&store.InvoiceRisk{ID: "audit-risk", TenantID: "t", InvoiceID: "invoice", Status: "OPEN", Version: 1, ContentJSON: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER reject_finance_audit BEFORE INSERT ON exp_outbox WHEN NEW.topic = 'audit' AND instr(NEW.payload_json, 'approval.finance_auto_return') > 0 BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := db.Model(&store.Outbox{}).Count(&before).Error; err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(financeBody(task))
	if _, err := s.Execute(context.Background(), actor, "approval.action", task.ID, raw); err == nil {
		t.Fatal("审计失败应使整个命令失败")
	}
	assertFinanceUntouched(t, db, task)
	var after int64
	if err := db.Model(&store.Outbox{}).Count(&after).Error; err != nil || after != before {
		t.Fatalf("回滚留下通知或审计: %d -> %d %v", before, after, err)
	}
	if err := db.Model(&store.ActionDetail{}).Where("task_id = ?", task.ID).Count(&after).Error; err != nil || after != 0 {
		t.Fatalf("回滚留下证据: %d %v", after, err)
	}
}

// TestFinanceRecheckManualReturnKeepsExistingBehavior 防止手动退回被外部复检或自动退回协议改变。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckManualReturnKeepsExistingBehavior(t *testing.T) {
	s, db, actor, task := financeFixture(t)
	s.deps.Files, s.deps.Personnel = nil, nil
	body := financeBody(task)
	body["action"], body["reason"] = "RETURN", "人工复核退回补充说明"
	r := runCommand(t, s, actor, "approval.action", task.ID, body)
	if r["action"] != "RETURN" || r["auto_returned"] == true {
		t.Fatalf("手动退回语义改变: %+v", r)
	}
	var inv store.Invoice
	if err := db.First(&inv, "id = ?", "invoice").Error; err != nil || inv.ReservationState != "RESERVED" {
		t.Fatalf("手动退回原占用语义改变: %+v %v", inv, err)
	}
}

// TestFinanceRecheckConcurrentReplayProducesOneReturn 防止两个相同命令并发产生多次真实退回。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckConcurrentReplayProducesOneReturn(t *testing.T) {
	s, db, actor, task := financeFixture(t)
	if err := db.Create(&store.InvoiceRisk{ID: "parallel-risk", TenantID: "t", InvoiceID: "invoice", Status: "OPEN", Version: 1, ContentJSON: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(financeBody(task))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.Execute(context.Background(), actor, "approval.action", task.ID, raw)
			if err == nil {
				var value map[string]any
				err = json.Unmarshal(result, &value)
				if err == nil && value["action"] != "RETURN" {
					err = errors.New("并发回执不是实际退回")
				}
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := db.Model(&store.Action{}).Where("task_id = ? AND action = ?", task.ID, "RETURN").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("并发产生多次退回: %d %v", count, err)
	}
}

// TestFinanceRecheckChangedTravelFaceRequiresNewApproval 防止当前行程票面已变但旧批准继续放行。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckChangedTravelFaceRequiresNewApproval(t *testing.T) {
	s, db, actor, task := financeFixture(t)
	var row store.Invoice
	if err := db.First(&row, "id = ?", "invoice").Error; err != nil {
		t.Fatal(err)
	}
	var inv domain.Invoice
	if err := json.Unmarshal([]byte(row.ContentJSON), &inv); err != nil {
		t.Fatal(err)
	}
	inv.TravelFacts = &domain.InvoiceTravelFacts{Kind: "RAIL", OriginCityCode: "110000", DestinationCityCode: "310000", StartDate: "2026-11-10", EndDate: "2026-11-10"}
	raw, _ := json.Marshal(inv)
	if err := db.Model(&row).Update("content_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	r := runCommand(t, s, actor, "approval.action", task.ID, financeBody(task))
	if r["action"] != "RETURN" || r["auto_returned"] != true {
		t.Fatalf("行程重大更正未要求重新审批: %+v", r)
	}
}

// TestFinanceRecheckRevalidatesPreflightProof 防止外部复核后票据变化或证明过期仍产生审批副作用。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckRevalidatesPreflightProof(t *testing.T) {
	for _, condition := range []string{"MISSING", "EXPIRED", "INVOICE_CHANGED"} {
		t.Run(condition, func(t *testing.T) {
			s, db, actor, task := financeFixture(t)
			raw, _ := json.Marshal(financeBody(task))
			var input CommandInput
			if err := json.Unmarshal(raw, &input); err != nil {
				t.Fatal(err)
			}
			cc := &commandContext{actor: actor, resourceID: task.ID, operation: "approval.action", input: input}
			if err := s.preflight(context.Background(), db, cc, raw); err != nil {
				t.Fatal(err)
			}
			switch condition {
			case "MISSING":
				cc.finance = nil
			case "EXPIRED":
				cc.finance.VerifiedAt = cc.finance.VerifiedAt.Add(-2 * time.Minute)
			case "INVOICE_CHANGED":
				if err := db.Model(&store.Invoice{}).Where("id = ?", "invoice").Update("version", gorm.Expr("version + 1")).Error; err != nil {
					t.Fatal(err)
				}
			}
			err := db.Transaction(func(tx *gorm.DB) error { _, _, err := s.approval(tx, cc, raw); return err })
			var p *domain.Problem
			if !errors.As(err, &p) || p.Status != 409 || p.Code != "FINANCE_RECHECK_STALE" {
				t.Fatalf("复检漂移未拒绝: %v", err)
			}
			assertFinanceUntouched(t, db, task)
		})
	}
}

// TestFinanceRecheckDirectoryFailureHasNoWrites 防止人员目录故障或撤权被转换为业务退回。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckDirectoryFailureHasNoWrites(t *testing.T) {
	s, db, actor, task := financeFixture(t)
	s.deps.Personnel = nil
	raw, _ := json.Marshal(financeBody(task))
	_, err := s.Execute(context.Background(), actor, "approval.action", task.ID, raw)
	var p *domain.Problem
	if !errors.As(err, &p) || p.Status != 503 {
		t.Fatalf("目录故障未保持503: %v", err)
	}
	assertFinanceUntouched(t, db, task)
}

// TestFinanceRecheckDoesNotAffectBusinessApproval 防止普通业务节点通过动作引入新的文件服务依赖。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckDoesNotAffectBusinessApproval(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	app, id := submissionFixture(t, db, true)
	runCommand(t, s, app, "claim.submit", id, map[string]any{"command_id": "non-finance-submit", "expected_version": 1})
	var lead store.Task
	if err := db.Where("claim_id = ? AND status = ?", id, "PENDING").First(&lead).Error; err != nil {
		t.Fatal(err)
	}
	s.deps.Files = financeUnavailableFiles{}
	actor := domain.Actor{TenantID: "t", SubjectID: lead.Assignee, Permissions: []string{"expense.approval.act", "expense.claim.read"}}
	r := runCommand(t, s, actor, "approval.action", lead.ID, map[string]any{"command_id": "non-finance-approve", "expected_version": 2, "task_version": lead.Version, "round": 1, "action": "APPROVE"})
	if r["action"] != "APPROVE" || r["auto_returned"] == true {
		t.Fatalf("业务节点被财务复检改变: %+v", r)
	}
}

// TestFinanceRecheckTwoAccountantsHaveOneFinalDecision 防止同轮两个真实加签会计并发重复退回或遗漏取消。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestFinanceRecheckTwoAccountantsHaveOneFinalDecision(t *testing.T) {
	s, db, actor, task := financeFixture(t)
	actor.Permissions = append(actor.Permissions, "expense.approval.transfer")
	seedPolicy(t, db, "assignments", domain.Assignment{ID: "finance-second", OrgID: "org", Role: "ACCOUNTANT", SubjectID: "accountant", ValidFrom: "2026-01-01"})
	seedPolicy(t, db, "scopes", domain.Scope{ID: "finance-second-scope", SubjectID: "accountant", OrgIDs: []string{"org"}})
	added := runCommand(t, s, actor, "approval.action", task.ID, map[string]any{"command_id": "finance-addsign", "expected_version": 3, "task_version": task.Version, "round": 1, "action": "ADDSIGN", "target_subject_id": "accountant", "reason": "独立双人会计复核"})
	if err := db.First(&task, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	var second store.Task
	if err := db.First(&second, "id = ?", added["added_task_id"]).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.InvoiceRisk{ID: "dual-risk", TenantID: "t", InvoiceID: "invoice", Status: "OPEN", Version: 1, ContentJSON: `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, current := range []store.Task{task, second} {
		wg.Add(1)
		go func(current store.Task) {
			defer wg.Done()
			accountant := actor
			accountant.SubjectID = current.Assignee
			raw, _ := json.Marshal(map[string]any{"command_id": "finance-decision-" + current.Assignee, "expected_version": 4, "task_version": current.Version, "round": 1, "action": "APPROVE"})
			_, err := s.Execute(context.Background(), accountant, "approval.action", current.ID, raw)
			results <- err
		}(current)
	}
	wg.Wait()
	close(results)
	succeeded, stale := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		var p *domain.Problem
		if !errors.As(err, &p) || p.Status != 409 {
			t.Fatalf("第二会计未收到版本冲突: %v", err)
		}
		stale++
	}
	if succeeded != 1 || stale != 1 {
		t.Fatalf("应恰有一次最终决定: succeeded=%d stale=%d", succeeded, stale)
	}
	var tasks []store.Task
	if err := db.Where("claim_id = ? AND node_code = ?", task.ClaimID, "ACCOUNTANT").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, current := range tasks {
		counts[current.Status]++
	}
	if counts["RETURNED"] != 1 || counts["CANCELLED"] != 1 {
		t.Fatalf("会计同轮取消不完整: %+v", counts)
	}
	var notices []store.Outbox
	if err := db.Where("topic = ?", "notification").Find(&notices).Error; err != nil {
		t.Fatal(err)
	}
	returned := 0
	for _, notice := range notices {
		var payload map[string]any
		if err := json.Unmarshal([]byte(notice.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["kind"] != "FINANCE_COMPLIANCE_RETURNED" {
			continue
		}
		returned++
		if payload["assignee_subject_id"] != "app" || payload["applicant_subject_id"] != "app" {
			t.Fatalf("退回通知扩散给申请人之外人员: %+v", payload)
		}
	}
	if returned != 1 {
		t.Fatalf("退回通知须仅一份: %d", returned)
	}
}
