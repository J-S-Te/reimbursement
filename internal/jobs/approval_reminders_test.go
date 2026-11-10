package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/internal/integration"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type reminderBatchRunner interface {
	ApprovalReminderBatch(context.Context, time.Time, time.Duration, time.Duration, int, string) (string, int, error)
}

type reminderPersonnel struct {
	unavailable bool
	denied      bool
	hook        func()
}

// VerifyApprover 模拟当前目录失效或在资格核验后完成任务的边界。
// 参数：ctx/actor/target/permission为正式目录协议参数。
// 返回值：只允许测试会计的精确财务审核能力。
func (p *reminderPersonnel) VerifyApprover(_ context.Context, actor domain.Actor,
	target, permission string) error {
	if p.hook != nil {
		p.hook()
	}
	if p.unavailable {
		return domain.Fail("PERSONNEL_UNAVAILABLE", "人员目录暂不可用", 503)
	}
	if p.denied || actor.TenantID != "t" || target != "acct" || permission != "expense.finance.review" {
		return domain.Fail("APPROVER_INELIGIBLE", "当前目录已撤销资格", 422)
	}
	return nil
}

// reminderFixture 构造仅含真实临时SQLite及明确财务范围的催办上下文。
// 参数：t为测试上下文。
// 返回值：处理器、当前时间和可控制的人员目录夹具。
func reminderFixture(t *testing.T) (*Processor, time.Time, *reminderPersonnel) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "reminder.sqlite")),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	c := domain.Claim{ID: "claim", TenantID: "t", Applicant: "app", OrgID: "org",
		Status: "IN_APPROVAL", Version: 2, CurrentRound: 1}
	raw, _ := json.Marshal(c)
	if err = db.Create(&store.Claim{ID: c.ID, TenantID: "t", Applicant: "app", OrgID: "org",
		Status: c.Status, Version: c.Version, ContentJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	task := domain.Task{ID: "task", ClaimID: c.ID, Round: 1, Node: 2, NodeCode: "ACCOUNTANT",
		Assignee: "acct", Status: "PENDING", Version: 1, RequiredPermission: "expense.finance.review"}
	raw, _ = json.Marshal(task)
	if err = db.Create(&store.Task{ID: task.ID, TenantID: "t", ClaimID: c.ID, Round: 1,
		Node: 2, NodeCode: "ACCOUNTANT", Assignee: "acct", Status: "PENDING", Version: 1,
		ContentJSON: string(raw), CreatedAt: now.Add(-49 * time.Hour),
		UpdatedAt: now.Add(-49 * time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(domain.Scope{ID: "scope", SubjectID: "acct", OrgIDs: []string{"org"}})
	if err = db.Create(&store.Config{ID: "scope", TenantID: "t", Kind: "scopes",
		Published: true, ContentJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	people := &reminderPersonnel{}
	return &Processor{DB: db, License: recoveryLicense{},
		Service: application.New(db, application.Dependencies{Personnel: people})}, now, people
}

// reminderRunner 通过公开批次接口检测缺失调度能力。
// 参数：t为测试上下文，p为真实处理器。
// 返回值：已实现的催办批次接口。
func reminderRunner(t *testing.T, p *Processor) reminderBatchRunner {
	t.Helper()
	runner, ok := any(p).(reminderBatchRunner)
	if !ok {
		t.Fatal("处理器尚未实现超期催办批次")
	}
	return runner
}

// TestApprovalRemindersQueueOncePerBucket 防止重复扫描、多实例及重启重复通知。
// 参数：t为测试上下文。
// 返回值：无。
func TestApprovalRemindersQueueOncePerBucket(t *testing.T) {
	p, now, _ := reminderFixture(t)
	runner := reminderRunner(t, p)
	for i, want := range []int{1, 0, 1} {
		when := now
		if i == 2 {
			when = now.Add(24 * time.Hour)
		}
		_, count, err := runner.ApprovalReminderBatch(context.Background(), when,
			48*time.Hour, 24*time.Hour, 10, "")
		if err != nil || count != want {
			t.Fatalf("扫描%d通知数应%d，实%d: %v", i, want, count, err)
		}
	}
	var rows []store.Outbox
	if err := p.DB.Order("created_at").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].EventID == rows[1].EventID {
		t.Fatalf("时间桶必须有唯一稳定事件: %+v", rows)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(rows[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["kind"] != "APPROVAL_REMINDER" || payload["assignee_subject_id"] != "acct" ||
		payload["task_id"] != "task" || payload["claim_id"] != "claim" || payload["round"] != float64(1) {
		t.Fatalf("提醒必须绑定同任务受理人: %+v", payload)
	}
}

// TestApprovalRemindersSkipIneligibleAndChangedTasks 防止给撤权、未到期或已完成任务发提醒。
// 参数：t为测试上下文。
// 返回值：无。
func TestApprovalRemindersSkipIneligibleAndChangedTasks(t *testing.T) {
	for _, name := range []string{"too-new", "completed", "old-round", "denied", "completed-during-check", "license"} {
		t.Run(name, func(t *testing.T) {
			p, now, people := reminderFixture(t)
			switch name {
			case "too-new":
				p.DB.Model(&store.Task{}).Where("id = ?", "task").Update("updated_at", now.Add(-time.Hour))
			case "completed":
				p.DB.Model(&store.Task{}).Where("id = ?", "task").Update("status", "APPROVED")
			case "old-round":
				p.DB.Model(&store.Task{}).Where("id = ?", "task").Update("round", 2)
			case "denied":
				people.denied = true
			case "completed-during-check":
				people.hook = func() {
					p.DB.Model(&store.Task{}).Where("id = ?", "task").Update("status", "APPROVED")
				}
			case "license":
				p.License = expiredRebuildLicense{now: now}
			}
			_, count, err := reminderRunner(t, p).ApprovalReminderBatch(context.Background(), now,
				48*time.Hour, 24*time.Hour, 10, "")
			if name == "license" && err == nil {
				t.Fatal("过期许可必须阻止生成新催办")
			}
			if name != "license" && err != nil {
				t.Fatal(err)
			}
			var stored int64
			if e := p.DB.Model(&store.Outbox{}).Count(&stored).Error; e != nil || count != 0 || stored != 0 {
				t.Fatalf("不合格任务不应生成通知: count=%d stored=%d err=%v", count, stored, e)
			}
		})
	}
}

type reminderDeliveryVerifier interface {
	ApprovalReminderDeliverable(context.Context, Record, map[string]any) (bool, error)
}

// TestApprovalReminderDeliveryRechecksCurrentTask 防止入队后已完成或撤权的任务仍向平台投递。
// 参数：t为测试上下文。
// 返回值：无。
func TestApprovalReminderDeliveryRechecksCurrentTask(t *testing.T) {
	for _, name := range []string{"valid", "completed", "revoked", "recipient-changed"} {
		t.Run(name, func(t *testing.T) {
			p, now, people := reminderFixture(t)
			_, _, err := reminderRunner(t, p).ApprovalReminderBatch(context.Background(), now,
				48*time.Hour, 24*time.Hour, 10, "")
			if err != nil {
				t.Fatal(err)
			}
			var outbox store.Outbox
			if err = p.DB.First(&outbox).Error; err != nil {
				t.Fatal(err)
			}
			switch name {
			case "completed":
				p.DB.Model(&store.Task{}).Where("id = ?", "task").UpdateColumn("status", "APPROVED")
			case "revoked":
				people.denied = true
			case "recipient-changed":
				p.DB.Model(&store.Task{}).Where("id = ?", "task").UpdateColumn("assignee", "other")
			}
			verifier, ok := any(p).(reminderDeliveryVerifier)
			if !ok {
				t.Fatal("处理器尚未实现催办投递前复核")
			}
			var data map[string]any
			_ = json.Unmarshal([]byte(outbox.PayloadJSON), &data)
			allowed, err := verifier.ApprovalReminderDeliverable(context.Background(), Record{
				TenantID: "t", ResourceID: "claim", PayloadJSON: outbox.PayloadJSON}, data)
			if err != nil || allowed != (name == "valid") {
				t.Fatalf("投递资格%s错误: %t %v", name, allowed, err)
			}
		})
	}
}

// TestApprovalReminderBatchBoundsAndConfig 防止催办扫描无界或忽略自定义超期及间隔。
// 参数：t为测试上下文。
// 返回值：无。
func TestApprovalReminderBatchBoundsAndConfig(t *testing.T) {
	p, now, _ := reminderFixture(t)
	runner := reminderRunner(t, p)
	if _, _, err := runner.ApprovalReminderBatch(context.Background(), now,
		48*time.Hour, time.Hour, 101, ""); err == nil {
		t.Fatal("单批超过100必须拒绝")
	}
	if _, count, err := runner.ApprovalReminderBatch(context.Background(), now,
		72*time.Hour, 12*time.Hour, 1, ""); err != nil || count != 0 {
		t.Fatalf("自定义72小时超期应尚未提醒: %d %v", count, err)
	}
	next, count, err := runner.ApprovalReminderBatch(context.Background(), now,
		24*time.Hour, 12*time.Hour, 1, "")
	if err != nil || count != 1 || next != "task" {
		t.Fatalf("受限页应返回稳定下一页: %q %d %v", next, count, err)
	}
	next, count, err = runner.ApprovalReminderBatch(context.Background(), now,
		24*time.Hour, 12*time.Hour, 1, next)
	if err != nil || count != 0 || next != "" {
		t.Fatalf("末页应回到扫描起点: %q %d %v", next, count, err)
	}
	_, count, err = runner.ApprovalReminderBatch(context.Background(), now.Add(12*time.Hour),
		24*time.Hour, 12*time.Hour, 1, "")
	if err != nil || count != 1 {
		t.Fatalf("自定义12小时间隔应进入下一桶: %d %v", count, err)
	}
}

// TestApprovalReminderDeliveryClosesAuthorityWaitWindow 防止目录等待期间变化的任务仍实际通知旧受理人。
// 参数：t为隔离测试上下文。
// 返回值：无。
func TestApprovalReminderDeliveryClosesAuthorityWaitWindow(t *testing.T) {
	for _, change := range []string{"completed", "transferred", "task-version", "claim-version", "claim-round", "scope-revoked"} {
		t.Run(change, func(t *testing.T) {
			p, now, people := reminderFixture(t)
			_, _, err := reminderRunner(t, p).ApprovalReminderBatch(context.Background(), now,
				48*time.Hour, 24*time.Hour, 10, "")
			if err != nil {
				t.Fatal(err)
			}
			var outbox store.Outbox
			if err = p.DB.First(&outbox).Error; err != nil {
				t.Fatal(err)
			}
			var originalClaim store.Claim
			if err = p.DB.First(&originalClaim, "id = ?", "claim").Error; err != nil {
				t.Fatal(err)
			}
			people.hook = func() {
				var mutation *gorm.DB
				switch change {
				case "completed":
					mutation = p.DB.Model(&store.Task{}).Where("id = ?", "task").UpdateColumn("status", "APPROVED")
				case "transferred":
					mutation = p.DB.Model(&store.Task{}).Where("id = ?", "task").UpdateColumn("assignee", "other")
				case "task-version":
					mutation = p.DB.Model(&store.Task{}).Where("id = ?", "task").UpdateColumn("version", 2)
				case "claim-version":
					mutation = p.DB.Model(&store.Claim{}).Where("id = ?", "claim").UpdateColumn("version", 3)
				case "claim-round":
					var claim store.Claim
					if e := p.DB.First(&claim, "id = ?", "claim").Error; e != nil {
						t.Fatal(e)
					}
					var dto domain.Claim
					_ = json.Unmarshal([]byte(claim.ContentJSON), &dto)
					dto.CurrentRound++
					raw, _ := json.Marshal(dto)
					mutation = p.DB.Model(&claim).UpdateColumn("content_json", string(raw))
				case "scope-revoked":
					mutation = p.DB.Model(&store.Config{}).Where("id = ?", "scope").UpdateColumn("published", false)
				}
				if mutation.Error != nil {
					t.Fatal(mutation.Error)
				}
			}
			var data map[string]any
			_ = json.Unmarshal([]byte(outbox.PayloadJSON), &data)
			until := time.Now().UTC().Add(time.Minute)
			if err = p.DB.Model(&outbox).Updates(map[string]any{"status": "PROCESSING", "version": 2,
				"lease_owner": "worker", "lease_until": until}).Error; err != nil {
				t.Fatal(err)
			}
			record := Record{ID: outbox.ID, TenantID: "t", ResourceID: "claim", Topic: "notification",
				EventID: outbox.EventID, PayloadJSON: outbox.PayloadJSON, CreatedAt: outbox.CreatedAt,
				Version: 2, LeaseOwner: "worker"}
			allowed, err := p.ApprovalReminderDeliverable(context.Background(), record, data)
			if err != nil || allowed {
				t.Errorf("目录核验等待期间%s后不应继续投递: %t %v", change, allowed, err)
			}
			// 恢复最初状态，使真实Outbox调用也必须独立跨过同一目录等待窗。
			for _, result := range []*gorm.DB{
				p.DB.Model(&store.Task{}).Where("id = ?", "task").UpdateColumns(map[string]any{
					"status": "PENDING", "assignee": "acct", "version": 1}),
				p.DB.Model(&store.Claim{}).Where("id = ?", "claim").UpdateColumns(map[string]any{
					"version": originalClaim.Version, "content_json": originalClaim.ContentJSON}),
				p.DB.Model(&store.Config{}).Where("id = ?", "scope").UpdateColumn("published", true),
			} {
				if result.Error != nil {
					t.Fatal(result.Error)
				}
			}
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/oauth2/token" {
					_, _ = w.Write([]byte(`{"access_token":"test-token","token_type":"Bearer","scope":"notification.ingest","expires_in":300}`))
					return
				}
				_, _ = w.Write([]byte(`{"data":{"ReceiptID":"receipt","EventID":"` + outbox.EventID + `","Status":"ACCEPTED"}}`))
			}))
			t.Cleanup(server.Close)
			p.Notifications = integration.NewClient(server.URL, server.URL, "client", "test-secret", "notification.ingest")
			if err = p.Outbox(context.Background(), record); err != nil {
				t.Errorf("已失效提醒应正常跳过: %v", err)
			}
			if requests.Load() != 0 {
				t.Fatalf("失效提醒触发了真实HTTP出站: %d", requests.Load())
			}
		})
	}
}

// TestApprovalReminderDeliveryPreservesDependencyRetries 验证有效提醒实际出站而目录故障保持重试。
// 参数：t为真实临时HTTP及SQLite上下文。
// 返回值：无。
func TestApprovalReminderDeliveryPreservesDependencyRetries(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "directory-unavailable"}[unavailable], func(t *testing.T) {
			p, now, people := reminderFixture(t)
			if _, _, err := p.ApprovalReminderBatch(context.Background(), now,
				48*time.Hour, 24*time.Hour, 10, ""); err != nil {
				t.Fatal(err)
			}
			var outbox store.Outbox
			if err := p.DB.First(&outbox).Error; err != nil {
				t.Fatal(err)
			}
			until := time.Now().UTC().Add(time.Minute)
			if err := p.DB.Model(&outbox).Updates(map[string]any{"status": "PROCESSING", "version": 2,
				"lease_owner": "worker", "lease_until": until}).Error; err != nil {
				t.Fatal(err)
			}
			people.unavailable = unavailable
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/oauth2/token" {
					_, _ = w.Write([]byte(`{"access_token":"test-token","token_type":"Bearer","scope":"notification.ingest","expires_in":300}`))
					return
				}
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
					"ReceiptID": "receipt", "EventID": request["event_id"], "Status": "ACCEPTED"}})
			}))
			t.Cleanup(server.Close)
			p.Notifications = integration.NewClient(server.URL, server.URL, "client", "test-secret", "notification.ingest")
			record := Record{ID: outbox.ID, TenantID: "t", ResourceID: "claim", Topic: "notification",
				EventID: outbox.EventID, PayloadJSON: outbox.PayloadJSON, CreatedAt: outbox.CreatedAt,
				Version: 2, LeaseOwner: "worker", Attempt: 1}
			err := p.Outbox(context.Background(), record)
			if !unavailable {
				if err != nil || requests.Load() != 2 {
					t.Fatalf("有效提醒应实际完成鉴权及通知HTTP受理: %d %v", requests.Load(), err)
				}
				return
			}
			if err == nil || requests.Load() != 0 {
				t.Fatalf("目录故障不得吞错或出站: %d %v", requests.Load(), err)
			}
			queue := Queue{DB: p.DB, Owner: "worker", Lease: time.Minute}
			if err = queue.Finish(context.Background(), "exp_outbox", record, err); err != nil {
				t.Fatal(err)
			}
			if err = p.DB.First(&outbox, "id = ?", record.ID).Error; err != nil {
				t.Fatal(err)
			}
			if outbox.Status != "PENDING" {
				t.Fatalf("目录依赖故障应留在可重试队列: %+v", outbox)
			}
		})
	}
}
