package application

import (
	"context"
	"errors"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"testing"
	"time"
)

type reconciliationFixture struct{ fail bool }

// Reconcile 提供受控项目分页故障用于验证游标不跳过。
// 参数：ctx 为调用边界，tenant/cursor/size 为服务端分页。
// 返回值：空末页或可恢复错误。
func (f *reconciliationFixture) Reconcile(context.Context, string, string, int) (ReconciliationPage, error) {
	if f.fail {
		return ReconciliationPage{}, errors.New("remote unavailable")
	}
	return ReconciliationPage{Items: []ReconciliationItem{}}, nil
}

// TestReconciliationFailurePreservesCursorAndFindsMissing 验证远端失败不推进且源端漏投可见。
// 参数：t 为测试上下文。
// 返回值：无。
func TestReconciliationFailurePreservesCursorAndFindsMissing(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	if e := db.AutoMigrate(RecoveryModels()...); e != nil {
		t.Fatal(e)
	}
	raw, _ := encode(domain.Claim{ID: "c", TenantID: "t", OrgID: "org", Version: 1, ProjectionRevision: 1})
	db.Create(&store.Claim{ID: "c", TenantID: "t", OrgID: "org", Version: 1, ContentJSON: raw, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	remote := &reconciliationFixture{fail: true}
	if _, e := s.ReconcileStep(context.Background(), "t", remote, 1); e == nil {
		t.Fatal("remote failure hidden")
	}
	var before store.ReconciliationCursor
	db.First(&before, "tenant_id = ?", "t")
	if before.Phase != "REMOTE" || before.RemoteCursor != "" {
		t.Fatalf("cursor jumped %+v", before)
	}
	remote.fail = false
	for i := 0; i < 4; i++ {
		if _, e := s.ReconcileStep(context.Background(), "t", remote, 1); e != nil {
			t.Fatal(e)
		}
	}
	var finding store.ReconciliationFinding
	if e := db.First(&finding, "tenant_id = ? AND claim_id = ?", "t", "c").Error; e != nil {
		var cursor store.ReconciliationCursor
		db.First(&cursor, "tenant_id = ?", "t")
		var claims []store.Claim
		db.Find(&claims)
		t.Fatalf("%v cursor=%+v claims=%+v", e, cursor, claims)
	}
	if finding.Status != "AUTHORITATIVE_SNAPSHOT_MISSING" {
		t.Fatalf("missing snapshot hidden %+v", finding)
	}
}

// TestMissingRemoteRecoveryReusesSnapshot 验证缺失投影只重发原完整快照且不制造财务版本。
// 参数：t 为测试上下文。
// 返回值：无。
func TestMissingRemoteRecoveryReusesSnapshot(t *testing.T) {
	s, db, _ := setupWorkflow(t)
	db.AutoMigrate(RecoveryModels()...)
	c := domain.Claim{ID: "c", TenantID: "t", OrgID: "org", Version: 2, ProjectionRevision: 1}
	raw, _ := encode(c)
	db.Create(&store.Claim{ID: "c", TenantID: "t", OrgID: "org", Version: 2, ContentJSON: raw})
	snapshot := domain.SpendSnapshot{SchemaVersion: 1, EventID: "event", TenantID: "t", SourceApplication: "reimbursement", SourceEnvironment: "test", ClaimID: "c", ClaimNo: "BX-1", DocumentRevision: 2, ApprovalRound: 1, ProjectionRevision: 1, CompleteSnapshot: true, Trigger: "PAYMENT_RECORDED", Currency: "CNY", Effects: []domain.SpendEffect{{EffectID: "pay", PaymentRecordID: "pay", Kind: "PAYMENT_RECORDED", AmountMinor: "1", OccurredAt: "2026-11-10T01:00:00Z", RecordedAt: "2026-11-10T01:00:00Z", ApprovedContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Allocations: []domain.SpendAllocation{{ProjectID: "p", FeeType: "TRAVEL", AmountMinor: "1"}}}}, Contributions: []domain.SpendContribution{{ProjectID: "p", FeeType: "TRAVEL", PaidNetMinor: "1"}}}
	snapshot.SnapshotHash, _ = domain.SpendSnapshotHash(snapshot)
	payload, _ := encode(snapshot)
	db.Create(&store.Outbox{ID: "event", EventID: "event", TenantID: "t", ResourceID: "c", Topic: "project", PayloadJSON: payload, Status: "DONE", Version: 4})
	db.Create(&store.ReconciliationFinding{ID: "finding", TenantID: "t", ClaimID: "c", SourceRevision: 1, SourceHash: snapshot.SnapshotHash, Status: "REMOTE_MISSING", Version: 2})
	a := domain.Actor{TenantID: "t", SubjectID: "ops", Permissions: []string{"expense.integration.manage", "expense.scope.tenant_all"}}
	if _, e := s.RecoverIntegration(context.Background(), a, "rebuild", "finding", RecoveryInput{CommandInput: CommandInput{CommandID: "repair", ExpectedVersion: 2}, Reason: "项目漏投"}); e != nil {
		t.Fatal(e)
	}
	var outbox store.Outbox
	db.First(&outbox, "id = ?", "event")
	if outbox.PayloadJSON != payload || outbox.Status != "PENDING" || outbox.Version != 5 {
		t.Fatalf("snapshot authority changed %+v", outbox)
	}
	persisted, _, e := loadClaim(db, "t", "c", false)
	if e != nil || persisted.Version != 2 || persisted.ProjectionRevision != 1 {
		t.Fatalf("finance revision mutated %+v %v", persisted, e)
	}
}
