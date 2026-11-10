package mysql_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/bootstrap"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"github.com/j-s-te/reimbursement/migrations"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const phase1Tenant = "phase1-synthetic-tenant"
const phase1Borrower = "phase1-synthetic-borrower"

type phase1Personnel struct{}

// VerifyApprover 仅为本门禁固定合成申请人的两个读取能力提供目录替身。
// 参数：ctx为请求上下文，actor为固定独立审批人，target为合成申请人，permission为只读能力。
// 返回值：任何未明确列入夹具的身份或权限均拒绝，不代表真实平台核验。
func (phase1Personnel) VerifyApprover(ctx context.Context, actor domain.Actor, target, permission string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reviewer := actor.SubjectID == "phase1-synthetic-reviewer-a" || actor.SubjectID == "phase1-synthetic-reviewer-b"
	read := permission == "expense.archive.read" || permission == "expense.claim.read"
	if actor.TenantID != phase1Tenant || !reviewer || target != phase1Borrower || !read {
		return domain.Fail("APPROVER_INELIGIBLE", "合成目录未授予目标或能力", 422)
	}
	return nil
}

// TestReimbursementPhase1MySQL84BorrowGate 验证隔离MySQL迁移、借阅审批CAS和真实任务排队。
// 参数：t为测试上下文，必须明确提供本门禁DSN及独立容器UUID和hostname。
// 返回值：无；缺DSN跳过，拒绝非空库且绝不清库或伪造任务完成。
func TestReimbursementPhase1MySQL84BorrowGate(t *testing.T) {
	if os.Getenv("REIMBURSEMENT_PHASE1_TEST_DSN") == "" {
		t.Skip("未提供REIMBURSEMENT_PHASE1_TEST_DSN；本门禁不寻找或复用其他数据库")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db := phase1FreshMySQL(t, ctx)
	phase1Migrations(t, ctx, db)
	archive, requester, reviewers := phase1ArchiveFixture(t, db)
	service := application.New(db, application.Dependencies{Personnel: phase1Personnel{}, Environment: "test"})
	phase1BorrowCAS(t, ctx, service, db, archive, requester, reviewers)
	phase1QueryFilters(t, ctx, service, db, requester)
	t.Log("PASS: MySQL8.4全新库迁移重放、借阅双审批CAS、命令幂等、唯一PENDING建包任务及JSON只读筛选")
	t.Log("范围仅DB事务与任务排队；Personnel为固定合成替身，未消费worker、未生成下载包或验证真实平台")
}

// phase1DSNConfig 在任何连接前限制本机高端口、专用schema和UTC时间解释。
// 参数：t为测试上下文，配置仅从本门禁专用环境变量读取。
// 返回值：通过严格边界校验的驱动配置，不输出DSN及密码。
func phase1DSNConfig(t *testing.T) *driver.Config {
	t.Helper()
	cfg, err := driver.ParseDSN(os.Getenv("REIMBURSEMENT_PHASE1_TEST_DSN"))
	if err != nil {
		t.Fatal("本门禁DSN格式不合法，未输出连接参数")
	}
	host, portText, err := net.SplitHostPort(cfg.Addr)
	port, portErr := strconv.Atoi(portText)
	schema := regexp.MustCompile(`^exp_reimbursement_test_phase1_[A-Za-z0-9_]+$`)
	if err != nil || portErr != nil || cfg.Net != "tcp" || host != "127.0.0.1" ||
		port < 20000 || port > 65535 || port == 3306 || !schema.MatchString(cfg.DBName) || len(cfg.DBName) > 64 {
		t.Fatal("仅允许127.0.0.1随机高端口及exp_reimbursement_test_phase1_专用schema")
	}
	if !cfg.ParseTime || cfg.Loc != time.UTC {
		t.Fatal("本门禁要求parseTime=true且loc=UTC，禁止静默改变时间解释")
	}
	cfg.MultiStatements = true
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 15*time.Second, 15*time.Second
	return cfg
}

// phase1FreshMySQL 先固定容器身份再检查零表，验证通过前不执行任何迁移或夹具写入。
// 参数：t为测试上下文，ctx为有界连接与迁移上下文。
// 返回值：只连接已明确批准的独立MySQL8.4空库；非空立即失败不清理。
func phase1FreshMySQL(t *testing.T, ctx context.Context) *gorm.DB {
	t.Helper()
	cfg := phase1DSNConfig(t)
	uuid, hostname := phase1ExpectedServer(t)
	sqlDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("不能建立隔离MySQL驱动，未输出连接参数")
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	phase1PinServer(t, ctx, sqlDB, uuid, hostname)
	var tables int64
	err = sqlDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&tables)
	if err != nil {
		t.Fatal("不能检查专用schema是否为空，未进行写入")
	}
	if tables != 0 {
		t.Fatal("专用schema非空：拒绝清理或复用，请由执行方创建全新schema")
	}
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), NowFunc: func() time.Time { return time.Now().UTC() },
		DisableAutomaticPing: true,
	})
	if err != nil {
		t.Fatal("隔离MySQL ORM初始化失败，未输出连接参数")
	}
	return db
}

// phase1ExpectedServer 检查预先独立取得的服务UUID及容器hostname边界。
// 参数：t为测试上下文，身份来自本门禁两个专用环境变量。
// 返回值：预期UUID和hostname；缺失或格式错误时在连接前失败。
func phase1ExpectedServer(t *testing.T) (string, string) {
	t.Helper()
	uuid := os.Getenv("REIMBURSEMENT_PHASE1_SERVER_UUID")
	hostname := os.Getenv("REIMBURSEMENT_PHASE1_CONTAINER_HOSTNAME")
	validUUID := regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
	validHost := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	if !validUUID.MatchString(uuid) || !validHost.MatchString(hostname) {
		t.Fatal("必须提供有效REIMBURSEMENT_PHASE1_SERVER_UUID和REIMBURSEMENT_PHASE1_CONTAINER_HOSTNAME")
	}
	return uuid, hostname
}

// phase1PinServer 用第一次实际SQL连接核对容器身份和MySQL8.4版本。
// 参数：t为测试上下文，ctx为连接边界，db为尚未查询业务数据的连接池，uuid/hostname为预期身份。
// 返回值：无；不匹配或连接失败直接终止，不输出连接参数。
func phase1PinServer(t *testing.T, ctx context.Context, db *sql.DB, uuid, hostname string) {
	t.Helper()
	var actualUUID, actualHostname, version string
	err := db.QueryRowContext(ctx, "SELECT @@server_uuid, @@hostname, VERSION()").Scan(&actualUUID, &actualHostname, &version)
	if err != nil {
		t.Fatal("不能核对隔离MySQL身份，未输出连接参数且未进行写入")
	}
	if !strings.EqualFold(actualUUID, uuid) || actualHostname != hostname || !strings.HasPrefix(version, "8.4.") {
		t.Fatal("隔离容器UUID、hostname或MySQL8.4版本与预期不符，拒绝查询或写入业务schema")
	}
}

// phase1Migrations 执行真实嵌入式SQL迁移两次并验证本轮表和索引存在。
// 参数：t为测试上下文，ctx为执行边界，db为已验证零表的隔离库。
// 返回值：无；任何迁移或结构断言失败直接停止，不自动重置数据库。
func phase1Migrations(t *testing.T, ctx context.Context, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal("无法取得隔离数据库连接")
	}
	for run := 0; run < 2; run++ {
		if err := bootstrap.Migrate(ctx, sqlDB, migrations.Files); err != nil {
			t.Fatalf("真实迁移第%d次失败: %v", run+1, err)
		}
	}
	var tables, indexes, versions int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", "exp_archive_borrow").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?", "exp_archive_download", "idx_archive_download_created").Scan(&indexes).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("SELECT COUNT(*) FROM exp_schema_migration WHERE name IN (?, ?)", "000010_archive_download_retention.sql", "000011_archive_borrow.sql").Scan(&versions).Error; err != nil {
		t.Fatal(err)
	}
	if tables != 1 || indexes != 1 || versions != 2 {
		t.Fatalf("迁移结构不完整: borrow表%d/保留期索引%d/版本%d", tables, indexes, versions)
	}
}

// phase1JSON 仅编码受控合成测试输入和回执，不读取运行凭据。
// 参数：t为测试上下文，value为需编码的夹具或业务回执。
// 返回值：JSON字节；编码失败终止测试。
func phase1JSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// phase1ArchiveFixture 准备一个合成已付状态单据及内容摘要真实绑定的封存清单。
// 参数：t为测试上下文，db为全新迁移后的隔离库。
// 返回值：唯一档案、合成借阅人和两个独立可信审批actor；不证明真实付款或文件归档。
func phase1ArchiveFixture(t *testing.T, db *gorm.DB) (store.Archive, domain.Actor, []domain.Actor) {
	t.Helper()
	start := time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
	phase1SeedClaim(t, db, "phase1-claim", phase1Tenant, "TRAVEL", "合成100%_档案", start)
	manifest := phase1JSON(t, map[string]any{"schema_version": 1, "tenant_id": phase1Tenant,
		"claim_id": "phase1-claim", "package_version": 1, "files": []any{}})
	sum := sha256.Sum256(manifest)
	archive := store.Archive{ID: "phase1-archive", TenantID: phase1Tenant, ClaimID: "phase1-claim", PackageVersion: 1,
		Status: "SEALED", ManifestJSON: string(manifest), ManifestHash: hex.EncodeToString(sum[:])}
	if err := db.Create(&archive).Error; err != nil {
		t.Fatal(err)
	}
	requester := domain.Actor{TenantID: phase1Tenant, SubjectID: phase1Borrower,
		Permissions: []string{"expense.archive.read", "expense.claim.read"}}
	reviewers := []domain.Actor{}
	for _, subject := range []string{"phase1-synthetic-reviewer-a", "phase1-synthetic-reviewer-b"} {
		reviewers = append(reviewers, domain.Actor{TenantID: phase1Tenant, SubjectID: subject,
			Permissions: []string{"expense.archive.read", "expense.archive.export", "expense.claim.read", "expense.scope.tenant_all"}})
	}
	return archive, requester, reviewers
}

// phase1SeedClaim 仅向本门禁隔离库写入只读筛选与档案授权的合成单据夹具。
// 参数：t/db为隔离上下文，id/tenant为对象边界，kind/title为JSON筛选字段，at为UTC创建时刻。
// 返回值：无；这不是付款业务操作，不创建支付成功回执或下载产物。
func phase1SeedClaim(t *testing.T, db *gorm.DB, id, tenant, kind, title string, at time.Time) {
	t.Helper()
	claim := domain.Claim{ID: id, TenantID: tenant, Applicant: phase1Borrower, OrgID: "phase1-org", Title: title,
		ClaimNo: "PHASE1-" + id, ClaimType: kind, Status: "PAID", Version: 1, Currency: "CNY", CreatedAt: at, UpdatedAt: at}
	row := store.Claim{ID: id, TenantID: tenant, Applicant: phase1Borrower, OrgID: claim.OrgID,
		Status: claim.Status, Version: 1, ContentJSON: string(phase1JSON(t, claim)), CreatedAt: at, UpdatedAt: at}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

// phase1Receipt 解析真实借阅服务回执，兼容首次结构体与命令回放JSON。
// 参数：t为测试上下文，value为真实业务服务返回值。
// 返回值：强类型借阅回执，格式错误终止测试。
func phase1Receipt(t *testing.T, value any) store.ArchiveBorrow {
	t.Helper()
	var row store.ArchiveBorrow
	if err := json.Unmarshal(phase1JSON(t, value), &row); err != nil {
		t.Fatal(err)
	}
	return row
}

// phase1BorrowCAS 使用同一借阅对象验证双独立审批、幂等与异载荷拒绝。
// 参数：t/ctx为执行边界，s/db为真实业务服务和隔离库，archive为封存版本，requester/reviewers为合成身份。
// 返回值：无；审批只应产生一个真实PENDING任务，不制造文件或消费任务。
func phase1BorrowCAS(t *testing.T, ctx context.Context, s *application.Service, db *gorm.DB,
	archive store.Archive, requester domain.Actor, reviewers []domain.Actor) {
	t.Helper()
	body := phase1JSON(t, map[string]any{"command_id": "phase1-request", "expected_version": 1, "reason": "合成档案借阅并发门禁"})
	value, err := s.RequestArchiveBorrow(ctx, requester, archive.ID, body)
	if err != nil {
		t.Fatal(err)
	}
	first := phase1Receipt(t, value)
	if first.ID == "" || first.Status != "PENDING" || first.ManifestHash != archive.ManifestHash {
		t.Fatal("真实借阅申请未绑定封存清单")
	}
	replay, err := s.RequestArchiveBorrow(ctx, requester, archive.ID, body)
	if err != nil || phase1Receipt(t, replay).ID != first.ID {
		t.Fatalf("申请命令回放不稳定: %v", err)
	}
	winner, approved, approvalBody := phase1ConcurrentApproval(t, ctx, s, first.ID, reviewers)
	value, err = s.DecideArchiveBorrow(ctx, reviewers[winner], first.ID, approvalBody)
	if err != nil || phase1Receipt(t, value).JobID != approved.JobID {
		t.Fatalf("审批回放重复或丢失任务: %v", err)
	}
	changed := phase1JSON(t, map[string]any{"command_id": "phase1-approve-" + strconv.Itoa(winner),
		"expected_version": 1, "decision": "APPROVE", "reason": "异载荷不得复用命令"})
	_, err = s.DecideArchiveBorrow(ctx, reviewers[winner], first.ID, changed)
	phase1Conflict(t, err, "COMMAND_PAYLOAD_CONFLICT")
	phase1QueuedJob(t, db, archive, first.ID, approved.JobID, reviewers[winner])
}

// phase1ConcurrentApproval 同时放行两个独立审批人并验证MySQL行锁与CAS唯一成功。
// 参数：t/ctx为执行边界，s为真实服务，id为同一借阅，reviewers为固定两名审批人。
// 返回值：唯一胜出下标、真实批准回执及其原始命令载荷。
func phase1ConcurrentApproval(t *testing.T, ctx context.Context, s *application.Service,
	id string, reviewers []domain.Actor) (int, store.ArchiveBorrow, json.RawMessage) {
	t.Helper()
	var values [2]any
	var failures [2]error
	var bodies [2]json.RawMessage
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range reviewers {
		bodies[i] = phase1JSON(t, map[string]any{"command_id": "phase1-approve-" + strconv.Itoa(i),
			"expected_version": 1, "decision": "APPROVE", "reason": "独立合成审批"})
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			values[index], failures[index] = s.DecideArchiveBorrow(ctx, reviewers[index], id, bodies[index])
		}(i)
	}
	close(start)
	wait.Wait()
	winner, success := -1, 0
	for i, err := range failures {
		if err == nil {
			winner, success = i, success+1
			continue
		}
		var p *domain.Problem
		if !errors.As(err, &p) || p.Status != 409 || (p.Code != "VERSION_CONFLICT" && p.Code != "STATE_CONFLICT") {
			t.Fatalf("竞争失败未返回CAS冲突: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("双审批必须恰好一成功一冲突，实际成功%d", success)
	}
	approved := phase1Receipt(t, values[winner])
	if approved.Status != "APPROVED" || approved.Version != 2 || approved.JobID == "" || approved.Approver != reviewers[winner].SubjectID {
		t.Fatal("审批回执未冻结唯一审批人与任务")
	}
	return winner, approved, bodies[winner]
}

// phase1Conflict 验证公开幂等冲突，避免把驱动错误误当业务拒绝。
// 参数：t为测试上下文，err为真实服务错误，code为预期公开错误码。
// 返回值：无，非409或错误码不符时终止测试。
func phase1Conflict(t *testing.T, err error, code string) {
	t.Helper()
	var p *domain.Problem
	if !errors.As(err, &p) || p.Status != 409 || p.Code != code {
		t.Fatalf("应返回%s/409，实际%v", code, err)
	}
}

// phase1QueuedJob 验证唯一真实建包任务绑定审批人、封存清单且仍待worker处理。
// 参数：t/db为隔离上下文，archive为档案，borrowID/jobID为业务回执，reviewer为胜出可信身份。
// 返回值：无，不写任务DONE、不生成假包或外部ACK。
func phase1QueuedJob(t *testing.T, db *gorm.DB, archive store.Archive, borrowID, jobID string, reviewer domain.Actor) {
	t.Helper()
	var jobs []store.Job
	if err := db.Where("kind = ?", "ARCHIVE_PACKAGE").Find(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != jobID || jobs[0].Status != "PENDING" || jobs[0].Version != 1 || jobs[0].ResourceID != archive.ClaimID {
		t.Fatal("建包任务非唯一真实PENDING队列事实")
	}
	var payload struct {
		Actor        domain.Actor `json:"actor"`
		ArchiveID    string       `json:"archive_id"`
		ManifestHash string       `json:"manifest_hash"`
		ClaimIDs     []string     `json:"claim_ids"`
	}
	if err := json.Unmarshal([]byte(jobs[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Actor.TenantID != phase1Tenant || payload.Actor.SubjectID != reviewer.SubjectID || !payload.Actor.Has("expense.archive.export") ||
		payload.ArchiveID != archive.ID || payload.ManifestHash != archive.ManifestHash || len(payload.ClaimIDs) != 1 || payload.ClaimIDs[0] != archive.ClaimID {
		t.Fatal("任务未绑定真实审批人和同一封存清单")
	}
	var borrow store.ArchiveBorrow
	if err := db.Take(&borrow, "id = ?", borrowID).Error; err != nil {
		t.Fatal(err)
	}
	if borrow.JobID != jobID || borrow.Approver != reviewer.SubjectID || borrow.Status != "APPROVED" || borrow.Version != 2 {
		t.Fatal("借阅事实与任务不一致")
	}
	var downloads int64
	if err := db.Model(&store.ArchiveDownload{}).Count(&downloads).Error; err != nil {
		t.Fatal(err)
	}
	if downloads != 0 {
		t.Fatal("队列门禁意外产生了未消费任务的下载包")
	}
}

// phase1QueryFilters 在同一专用库以只读业务查询验证MySQL JSON、字面转义和北京日界。
// 参数：t/ctx为执行边界，s/db为真实服务和同一专用库，actor为合成只读申请人。
// 返回值：无，任何跨租户可见或条件失效终止测试。
func phase1QueryFilters(t *testing.T, ctx context.Context, s *application.Service, db *gorm.DB, actor domain.Actor) {
	t.Helper()
	start := time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
	fixtures := []struct {
		id, tenant, kind, title string
		at                      time.Time
	}{
		{"phase1-percent-decoy", phase1Tenant, "TRAVEL", "合成100AB_档案", start},
		{"phase1-underscore-decoy", phase1Tenant, "TRAVEL", "合成100%X档案", start},
		{"phase1-type-decoy", phase1Tenant, "DAILY", "合成100%_档案", start},
		{"phase1-before-decoy", phase1Tenant, "TRAVEL", "合成100%_档案", start.Add(-time.Second)},
		{"phase1-after-decoy", phase1Tenant, "TRAVEL", "合成100%_档案", start.Add(24 * time.Hour)},
		{"phase1-foreign-decoy", "phase1-other-tenant", "TRAVEL", "合成100%_档案", start},
	}
	for _, fixture := range fixtures {
		phase1SeedClaim(t, db, fixture.id, fixture.tenant, fixture.kind, fixture.title, fixture.at)
	}
	raw, err := s.Query(ctx, actor, "claims", "", map[string]string{"q": "100%_", "claim_type": "TRAVEL", "date_from": "2026-11-11", "date_to": "2026-11-11"})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Total int64 `json:"total"`
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != "phase1-claim" {
		t.Fatal("MySQL JSON组合筛选、字面%/_或北京日期/租户边界不正确")
	}
	_, err = s.Query(ctx, actor, "claim", "phase1-foreign-decoy", nil)
	var p *domain.Problem
	if !errors.As(err, &p) || p.Status != 404 {
		t.Fatalf("跨租户详情未隐藏: %v", err)
	}
}

// TestReimbursementPhase1MySQL84ParseExceptionGate 验证真实MySQL异常查询方言、原件范围和线路过滤。
// 参数：t为测试上下文，必须提供本门禁DSN及独立容器UUID和hostname。
// 返回值：无；缺DSN跳过，非空库或身份不匹配时拒绝写入。
func TestReimbursementPhase1MySQL84ParseExceptionGate(t *testing.T) {
	if os.Getenv("REIMBURSEMENT_PHASE1_TEST_DSN") == "" {
		t.Skip("未提供REIMBURSEMENT_PHASE1_TEST_DSN；不寻找或复用其他数据库")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db := phase1FreshMySQL(t, ctx)
	phase1Migrations(t, ctx, db)
	phase1ParseExceptionFixture(t, db)
	s := application.New(db, application.Dependencies{Environment: "test"})
	a := domain.Actor{TenantID: phase1Tenant, SubjectID: phase1Borrower,
		Permissions: []string{"expense.invoice.read", "expense.claim.read", "expense.scope.tenant_all"}}
	value, err := s.ParseExceptions(ctx, a, map[string]string{
		"status": "FAILED", "date_from": "2026-11-11", "date_to": "2026-11-11", "page_size": "1"})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Total int64 `json:"total"`
		Items []struct {
			DocumentID           string `json:"document_id"`
			RunID                string `json:"parse_run_id"`
			JobID                string `json:"parse_job_id"`
			CandidateCount       int    `json:"candidate_count"`
			ParsedCandidateCount int    `json:"parsed_candidate_count"`
		} `json:"items"`
	}
	raw := phase1JSON(t, value)
	if err = json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].DocumentID != "query-own" ||
		page.Items[0].RunID != "query-own-run" || page.Items[0].JobID != "query-own-job" ||
		page.Items[0].CandidateCount != 0 || page.Items[0].ParsedCandidateCount != 2 {
		t.Fatalf("MySQL异常列表的最新运行、所有权、JSON长度或北京日期不符: %s", raw)
	}
	if strings.Contains(string(raw), "SYNTHETIC_PRIVATE_RAW") {
		t.Fatal("异常列表暴露未授权原始解析内容")
	}
	phase1ParseRouteQuery(t, ctx, s, db, a)
	t.Log("PASS: 仅本人最新FAILED无票原件；跨租户/他人、成功覆盖旧失败、人工来源排除；JSON长度/北京日期/字面%_与TECHNICAL线路")
	t.Log("范围仅全新独立MySQL8.4的迁移与服务层SQL查询，不代表真实平台、解析器或浏览器业务验收")
}

// phase1ParseExceptionFixture 写入一个目标异常及最小范围、历史运行和人工票据排除夹具。
// 参数：t为测试上下文，db为已固定身份且真实迁移后的独占数据库。
// 返回值：无，失败立即终止且不复用其他数据库。
func phase1ParseExceptionFixture(t *testing.T, db *gorm.DB) {
	t.Helper()
	start := time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
	for _, id := range []string{"own", "foreign", "other-owner", "success", "manual", "before", "after"} {
		tenant, owner, at := phase1Tenant, phase1Borrower, start
		if id == "foreign" {
			tenant = "query-other-tenant"
		}
		if id == "other-owner" {
			owner = "query-other-owner"
		}
		if id == "before" {
			at = start.Add(-time.Second)
		}
		if id == "after" {
			at = start.Add(24 * time.Hour)
		}
		doc := store.Document{ID: "query-" + id, TenantID: tenant, Owner: owner,
			FileID: "query-file-" + id, Status: "REVIEW_REQUIRED", Version: 1,
			ContentJSON: `{"purpose":"reimbursement.invoice.original","filename":"synthetic100%_.pdf","invoice_ids":[]}`}
		run := store.InvoiceParseRun{ID: doc.ID + "-run", TenantID: tenant, DocumentID: doc.ID,
			JobID: doc.ID + "-job", Attempt: 1, Status: "FAILED", FailureCode: "PARSER_FAILED",
			ResultJSON: `{"documents":[{"raw_fields":"SYNTHETIC_PRIVATE_RAW"},{}]}`, CreatedAt: at}
		if err := db.Create(&doc).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		if id == "success" {
			run.ID, run.Attempt, run.Status = "query-success-new-run", 2, "SUCCEEDED"
			if err := db.Create(&run).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	invoice := store.Invoice{ID: "query-manual-invoice", TenantID: phase1Tenant, Owner: phase1Borrower,
		Version: 1, ReservationState: "AVAILABLE", ContentJSON: `{}`}
	if err := db.Create(&invoice).Error; err != nil {
		t.Fatal(err)
	}
	source := store.InvoiceSource{ID: "query-manual-source", TenantID: phase1Tenant,
		InvoiceID: invoice.ID, DocumentID: "query-manual", ParseRunID: "", CandidateIndex: -1,
		SourcesJSON: `[]`, SourcesHash: strings.Repeat("0", 64)}
	if err := db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
}

// phase1ParseRouteQuery 验证新增线路字段在实际MySQL JSON查询中与字面关键词组合生效。
// 参数：t/ctx为执行边界，s/db为真实服务和独占库，a为本人合成读取主体。
// 返回值：无；错误线路或未转义百分号扩大结果时终止测试。
func phase1ParseRouteQuery(t *testing.T, ctx context.Context, s *application.Service,
	db *gorm.DB, a domain.Actor) {
	t.Helper()
	at := time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
	for _, id := range []string{"technical", "sales", "wildcard"} {
		title, route := "合成100%_档案", "TECHNICAL"
		if id == "sales" {
			route = "SALES"
		}
		if id == "wildcard" {
			title = "合成100ABX档案"
		}
		phase1SeedClaim(t, db, "query-claim-"+id, phase1Tenant, "TRAVEL", title, at)
		if err := db.Model(&store.Claim{}).Where("id = ?", "query-claim-"+id).
			Update("content_json", gorm.Expr("JSON_SET(content_json, '$.route_type', ?)", route)).Error; err != nil {
			t.Fatal(err)
		}
	}
	raw, err := s.Query(ctx, a, "claims", "", map[string]string{
		"route_type": "TECHNICAL", "q": "100%_", "date_from": "2026-11-11", "date_to": "2026-11-11"})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Total int64 `json:"total"`
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err = json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != "query-claim-technical" {
		t.Fatalf("MySQL route_type/字面%%_筛选错误: %s", raw)
	}
}
