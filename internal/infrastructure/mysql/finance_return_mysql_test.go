package mysql_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/gorm"
)

// TestReimbursementPhase1MySQL84FinanceReturnGate 验证真实财务退回、冻结重放与去重退回率。
// 参数：t 为测试边界；必须提供专用空库DSN和固定独立容器身份。
// 返回值：无；缺DSN跳过，任何身份或非空库检查失败均不写入。
func TestReimbursementPhase1MySQL84FinanceReturnGate(t *testing.T) {
	if os.Getenv("REIMBURSEMENT_PHASE1_TEST_DSN") == "" {
		t.Skip("未提供专用隔离DSN；不寻找或复用业务数据库")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db := phase1FreshMySQL(t, ctx)
	phase1Migrations(t, ctx, db)
	mysqlFixture(t, db)
	service := application.New(db, application.Dependencies{
		Projects: &mysqlProjects{}, Files: mysqlFiles{}, Personnel: mysqlPersonnel{}, Environment: "test",
	})
	app := domain.Actor{TenantID: "t", SubjectID: "app",
		Permissions: []string{"expense.claim.read", "expense.claim.submit"}}
	financeReturnCommand(t, ctx, service, app, "claim.submit", "claim-a",
		map[string]any{"command_id": "return-submit", "expected_version": 1})
	var lead, task store.Task
	if err := db.Where("claim_id = ? AND status = ?", "claim-a", "PENDING").First(&lead).Error; err != nil {
		t.Fatal(err)
	}
	reviewer := domain.Actor{TenantID: "t", SubjectID: "lead",
		Permissions: []string{"expense.claim.read", "expense.approval.act"}}
	financeReturnCommand(t, ctx, service, reviewer, "approval.action", lead.ID,
		map[string]any{"command_id": "return-lead", "expected_version": 2, "task_version": lead.Version, "round": 1, "action": "APPROVE"})
	if err := db.Where("claim_id = ? AND node_code = ?", "claim-a", "ACCOUNTANT").First(&task).Error; err != nil {
		t.Fatal(err)
	}
	financeReturnMismatch(t, db)
	reviewer.SubjectID = "acct"
	reviewer.Permissions = append(reviewer.Permissions, "expense.finance.review")
	body := map[string]any{"command_id": "return-accountant", "expected_version": 3, "task_version": task.Version, "round": 1, "action": "APPROVE"}
	result := financeReturnCommand(t, ctx, service, reviewer, "approval.action", task.ID, body)
	var receipt map[string]any
	if err := json.Unmarshal(result, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["action"] != "RETURN" || receipt["auto_returned"] != true || receipt["status"] != "RETURNED" {
		t.Fatalf("未实际退回: %s", result)
	}
	replay := financeReturnCommand(t, ctx, service, reviewer, "approval.action", task.ID, body)
	if string(replay) != string(result) {
		t.Fatal("真实MySQL JSON规范化破坏冻结重放")
	}
	financeReturnFacts(t, db)
	value, err := service.Statistics(ctx, app, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	var stats struct {
		Submitted int  `json:"submitted_count"`
		Returned  int  `json:"returned_count"`
		Rate      *int `json:"return_rate_basis_points"`
	}
	if err = json.Unmarshal(phase1JSON(t, value), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Submitted != 1 || stats.Returned != 1 || stats.Rate == nil || *stats.Rate != 10000 {
		t.Fatalf("真实退回计数错误: %+v", stats)
	}
	t.Log("PASS: 独立MySQL8.4迁移重放→真实提交→业务通过→会计实际RETURN→命令重放→统计1/1；文件/人员/项目为合成替身")
}

// financeReturnCommand 执行真实服务命令并保留冻结响应供精确重放比较。
// 参数：t/ctx 为测试边界，s/a 为服务与主体，operation/id/body 为业务请求。
// 返回值：成功原始响应；任何命令错误终止测试。
func financeReturnCommand(t *testing.T, ctx context.Context, s *application.Service,
	a domain.Actor, operation, id string, body any) json.RawMessage {
	t.Helper()
	result, err := s.Execute(ctx, a, operation, id, phase1JSON(t, body))
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
	return result
}

// financeReturnMismatch 设置唯一合成票据的真实不一致事实以触发财务复检。
// 参数：t 为测试边界，db 为固定独立空库迁移后的连接。
// 返回值：无；保留票面、租户、身份与占用，不改批准快照。
func financeReturnMismatch(t *testing.T, db *gorm.DB) {
	t.Helper()
	var row store.Invoice
	if err := db.First(&row, "id = ?", "invoice").Error; err != nil {
		t.Fatal(err)
	}
	var inv domain.Invoice
	if err := json.Unmarshal([]byte(row.ContentJSON), &inv); err != nil {
		t.Fatal(err)
	}
	inv.VerificationStatus = "MISMATCH"
	if err := db.Model(&row).Update("content_json", string(phase1JSON(t, inv))).Error; err != nil {
		t.Fatal(err)
	}
}

// financeReturnFacts 核对动作去重、任务取消、占用释放与无资金副作用。
// 参数：t 为测试边界，db 为固定独立测试库。
// 返回值：无；任何事实不一致终止测试。
func financeReturnFacts(t *testing.T, db *gorm.DB) {
	t.Helper()
	var actions, effects, pending, payments, reversals int64
	for _, check := range []struct {
		model     any
		condition string
		want      int64
		count     *int64
	}{
		{&store.Action{}, "action = 'RETURN'", 1, &actions},
		{&store.Effect{}, "1 = 1", 0, &effects},
		{&store.Payment{}, "1 = 1", 0, &payments},
		{&store.Reversal{}, "1 = 1", 0, &reversals},
		{&store.Task{}, "status IN ('PENDING','WAITING','BLOCKED')", 0, &pending},
	} {
		if err := db.Model(check.model).Where(check.condition).Count(check.count).Error; err != nil {
			t.Fatal(err)
		}
		if *check.count != check.want {
			t.Fatalf("事实计数错误: %T %d", check.model, *check.count)
		}
	}
	var invoice store.Invoice
	if err := db.First(&invoice, "id = ?", "invoice").Error; err != nil {
		t.Fatal(err)
	}
	if invoice.ReservationState != "AVAILABLE" || invoice.ReservationClaimID != "" {
		t.Fatal("退回未释放本单预留")
	}
}
