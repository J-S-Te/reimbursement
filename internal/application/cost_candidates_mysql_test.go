package application

import (
	"context"
	"encoding/json"
	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCostCandidatesMySQL 验证真实付款历史关联批准快照而非当前草稿，且租户隔离。
// 参数：t为测试上下文，只允许明确私有回环测试DSN。
// 返回值：无，冻结身份、预付、冲正、范围或未知历史错误即失败。
func TestCostCandidatesMySQL(t *testing.T) {
	path := os.Getenv("REIMBURSEMENT_COST_TEST_DSN_FILE")
	if path == "" {
		t.Skip("explicit private isolated DSN file required")
	}
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private DSN file required")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal("private isolated DSN read failed")
	}
	cfg, e := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if e != nil {
		t.Fatal("invalid isolated DSN")
	}
	host, _, e := net.SplitHostPort(cfg.Addr)
	if e != nil || cfg.Net != "tcp" || host != "127.0.0.1" || !strings.HasPrefix(cfg.DBName, "reimbursement_test_") {
		t.Fatal("explicit isolated schema required")
	}
	db, e := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal("open isolated DB failed")
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	suffix := time.Now().UTC().Format("150405.000000000")
	tenant := "cost-" + suffix
	c := domain.Claim{ID: "claim-" + suffix, TenantID: tenant, ClaimNo: "QA-" + suffix, Applicant: "frozen-person", OrgID: "frozen-org", Status: "PAID_PENDING_ARCHIVE", CurrentRound: 1, Version: 3, Currency: "CNY", Payable: 101, ClaimType: "TRAVEL", ProjectionRevision: 2, Items: []domain.Item{{ID: "item-a", ExpenseNature: "ACTUAL", FeeType: "TRAVEL", OccurredDate: "2026-11-01", Requested: 41, Allocations: []domain.Allocation{{ProjectID: "p", Amount: 14}, {ProjectID: "q", Amount: 27}}}, {ID: "item-b", ExpenseNature: "PREPAYMENT", FeeType: "TRAVEL", OccurredDate: "2026-11-02", Requested: 60, Allocations: []domain.Allocation{{ProjectID: "p", Amount: 60}}}}}
	hash, e := approvedContentHash(c, approvalPlan{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	c.ContentHash = hash
	frozen, _ := json.Marshal(approvalSnapshot{Claim: c})
	snapshot := store.Snapshot{ID: "snapshot-" + suffix, TenantID: tenant, ClaimID: c.ID, Round: 1, ContentHash: hash, ContentJSON: string(frozen)}
	if e = db.Create(&snapshot).Error; e != nil {
		t.Fatal(e)
	}
	current := c
	current.Items = nil
	current.Applicant = "changed-person"
	current.OrgID = "changed-org"
	currentRaw, _ := json.Marshal(current)
	if e = db.Create(&store.Claim{ID: c.ID, TenantID: tenant, Applicant: current.Applicant, OrgID: current.OrgID, Status: current.Status, Version: current.Version, ContentJSON: string(currentRaw)}).Error; e != nil {
		t.Fatal(e)
	}
	effect := paymentEffect(c, "payment-"+suffix, "2026-11-03")
	if e = storeEffect(db, c, effect); e != nil {
		t.Fatal(e)
	}
	reverse := reservedReversal(effect)
	reverse.EffectID = "refund-" + suffix
	if e = storeEffect(db, c, reverse); e != nil {
		t.Fatal(e)
	}
	svc := New(db, Dependencies{Environment: "local"})
	result, e := svc.CostCandidates(context.Background(), tenant, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if !result.Complete || len(result.Claims) != 1 {
		t.Fatalf("invalid snapshot=%+v", result)
	}
	claim := result.Claims[0]
	if claim.Status != "KNOWN" || claim.ClaimType != "TRAVEL" || claim.Applicant != "frozen-person" || claim.OrganizationID != "frozen-org" || len(claim.FrozenItems) != 3 || len(claim.Effects) != 2 || claim.FrozenItems[2].OccurredDate != "2026-11-02" {
		t.Fatalf("lost frozen facts=%+v", claim)
	}
	if claim.FrozenItems[0].AmountMinor+claim.FrozenItems[1].AmountMinor != 41 || claim.FrozenItems[2].AmountMinor != 60 || claim.FrozenItems[0].ExpenseNature != "ACTUAL" || claim.FrozenItems[2].ExpenseNature != "PREPAYMENT" {
		t.Fatalf("mixed exact allocation lost=%+v", claim.FrozenItems)
	}
	if path := os.Getenv("REIMBURSEMENT_COST_FIXTURE_PATH"); path != "" {
		raw, e := json.MarshalIndent(map[string]any{"code": "OK", "data": result}, "", "  ")
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
	other, e := svc.CostCandidates(context.Background(), tenant+"other", c.ID)
	if e != nil || len(other.Claims) != 0 {
		t.Fatalf("cross tenant=%+v err=%v", other, e)
	}
	if e = db.Delete(&snapshot).Error; e != nil {
		t.Fatal(e)
	}
	result, e = svc.CostCandidates(context.Background(), tenant, c.ID)
	if e != nil || result.Claims[0].Status != "UNKNOWN" || len(result.Claims[0].Effects) != 2 {
		t.Fatalf("historical unknown=%+v err=%v", result, e)
	}
}
