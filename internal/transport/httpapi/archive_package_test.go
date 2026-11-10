package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// archivePackageHTTP 装配可撤权的显式身份与真实应用事务，仅验证 HTTP 到应用层契约。
// 参数：t 为测试上下文。
// 返回值：路由、隔离库与可在用例内修改的身份指针。
func archivePackageHTTP(t *testing.T) (*gin.Engine, *gorm.DB, *domain.Actor) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	a := &domain.Actor{TenantID: "t", SubjectID: "exporter", Permissions: []string{"expense.archive.read", "expense.archive.export", "expense.claim.read"}}
	router := New(application.New(db, application.Dependencies{}), func(c *gin.Context) { c.Set("expense_actor", *a); c.Next() })
	return router, db, a
}

// archivePackageSeed 写入本人可读单据与封存档案版本。
// 参数：t 为测试上下文，db 为隔离库。
// 返回值：档案行。
func archivePackageSeed(t *testing.T, db *gorm.DB) store.Archive {
	t.Helper()
	raw, err := json.Marshal(domain.Claim{ID: "claim", TenantID: "t", Applicant: "exporter", OrgID: "org"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&store.Claim{ID: "claim", TenantID: "t", Applicant: "exporter", OrgID: "org", ContentJSON: string(raw), Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
	archive := store.Archive{ID: "archive", TenantID: "t", ClaimID: "claim", PackageVersion: 1, Status: "SEALED", ManifestHash: strings.Repeat("a", 64), ManifestJSON: `{"package_version":1,"files":[{"file_id":"file","document_id":"doc","sha256":"` + strings.Repeat("b", 64) + `"}]}`}
	if err = db.Create(&archive).Error; err != nil {
		t.Fatal(err)
	}
	return archive
}

// archivePackageServe 执行一次请求并返回响应。
// 参数：router 为路由，method/path/body 为请求，id 为追踪号。
// 返回值：记录器。
func archivePackageServe(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// TestArchivePackageRoutesRegistered 验证建包、状态与下载路由注册且不与既有档案前缀冲突。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchivePackageRoutesRegistered(t *testing.T) {
	router, _, _ := archivePackageHTTP(t)
	registered := map[string]bool{}
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		"POST /api/v1/archives/:id/packages",
		"GET /api/v1/archives/:id/packages",
		"GET /api/v1/archive-packages/:id",
		"POST /api/v1/archive-packages/:id/download",
		"GET /api/v1/claims/:id/archives",
	} {
		if !registered[want] {
			t.Fatalf("缺少路由 %s", want)
		}
	}
}

// TestArchivePackageHTTPDownloadReauthorizes 验证建包、状态查询与每次下载重新鉴权。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchivePackageHTTPDownloadReauthorizes(t *testing.T) {
	router, db, current := archivePackageHTTP(t)
	archive := archivePackageSeed(t, db)
	// :id 是封存档案版本，单据归属与授权由应用层按档案重新推导。
	response := archivePackageServe(router, http.MethodPost, "/api/v1/archives/"+archive.ID+"/packages", `{"command_id":"http-package","expected_version":1}`)
	if response.Code != 200 {
		t.Fatalf("建包 HTTP %d: %s", response.Code, response.Body.String())
	}
	var created struct {
		Data struct {
			JobID  string `json:"job_id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.Data.JobID == "" || created.Data.Status != "PENDING" {
		t.Fatalf("排队回执不符: %s", response.Body.String())
	}
	jobID := created.Data.JobID
	if response = archivePackageServe(router, http.MethodPost, "/api/v1/archives/missing-archive/packages", `{"command_id":"http-missing","expected_version":1}`); response.Code != 404 {
		t.Fatalf("未知档案 HTTP %d: %s", response.Code, response.Body.String())
	}
	content := []byte("PK sealed archive bytes")
	sum := sha256.Sum256(content)
	download := store.ArchiveDownload{ID: "download", TenantID: "t", JobID: jobID, ClaimID: archive.ClaimID, ArchiveID: archive.ID, ManifestHash: archive.ManifestHash, SHA256: hex.EncodeToString(sum[:]), Content: content, CreatedAt: time.Now().UTC()}
	if err := db.Model(&store.Job{}).Where("id = ?", jobID).Update("status", "DONE").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	response = archivePackageServe(router, http.MethodGet, "/api/v1/archive-packages/"+jobID, "")
	if response.Code != 200 {
		t.Fatalf("状态 HTTP %d: %s", response.Code, response.Body.String())
	}
	var status struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.Data["status"] != "DONE" || status.Data["sha256"] != download.SHA256 || status.Data["manifest_hash"] != archive.ManifestHash {
		t.Fatalf("状态摘要不符: %s", response.Body.String())
	}
	if _, held := status.Data["content"]; held {
		t.Fatal("状态接口泄露文件内容")
	}
	response = archivePackageServe(router, http.MethodPost, "/api/v1/archive-packages/"+jobID+"/download", "")
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), content) {
		t.Fatalf("下载 HTTP %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/zip" || !strings.Contains(response.Header().Get("Content-Disposition"), `filename="reimbursement-archive-download.zip"`) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("下载响应头不安全: %v", response.Header())
	}
	// 撤权后下载与状态都按当前权限拒绝，历史任务ID不是访问凭证。
	current.Permissions = []string{"expense.archive.read", "expense.claim.read"}
	response = archivePackageServe(router, http.MethodPost, "/api/v1/archive-packages/"+jobID+"/download", "")
	if response.Code != 403 {
		t.Fatalf("撤权后下载 HTTP %d: %s", response.Code, response.Body.String())
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil || problem.Code != "FORBIDDEN" {
		t.Fatalf("撤权错误语义不符: %s", response.Body.String())
	}
	if response = archivePackageServe(router, http.MethodGet, "/api/v1/archive-packages/"+jobID, ""); response.Code != 403 {
		t.Fatalf("撤权后状态 HTTP %d: %s", response.Code, response.Body.String())
	}
}

// TestArchivePackagesListHTTP 验证按档案列包路由恢复本人 job_id、只回元数据并保持分页与鉴权口径。
// 参数：t 为测试上下文。
// 返回值：无。
func TestArchivePackagesListHTTP(t *testing.T) {
	router, db, current := archivePackageHTTP(t)
	archive := archivePackageSeed(t, db)
	response := archivePackageServe(router, http.MethodPost, "/api/v1/archives/"+archive.ID+"/packages", `{"command_id":"http-list","expected_version":1}`)
	if response.Code != 200 {
		t.Fatalf("建包 HTTP %d: %s", response.Code, response.Body.String())
	}
	var created struct {
		Data struct {
			JobID string `json:"job_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.Data.JobID == "" {
		t.Fatalf("建包回执不符: %s", response.Body.String())
	}
	jobID := created.Data.JobID
	list := func(query string) (*httptest.ResponseRecorder, map[string]any, []map[string]any) {
		t.Helper()
		recorder := archivePackageServe(router, http.MethodGet, "/api/v1/archives/"+archive.ID+"/packages"+query, "")
		var page struct {
			Data struct {
				Items    []map[string]any `json:"items"`
				Total    int              `json:"total"`
				Page     int              `json:"page"`
				PageSize int              `json:"page_size"`
			} `json:"data"`
		}
		if recorder.Code == 200 {
			if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
				t.Fatalf("列表响应不是JSON: %s", recorder.Body.String())
			}
		}
		summary := map[string]any{"total": page.Data.Total, "page": page.Data.Page, "page_size": page.Data.PageSize}
		return recorder, summary, page.Data.Items
	}
	recorder, summary, items := list("?page=1&page_size=5")
	if recorder.Code != 200 || summary["total"] != 1 || len(items) != 1 {
		t.Fatalf("列表 HTTP %d %v: %s", recorder.Code, summary, recorder.Body.String())
	}
	if items[0]["job_id"] != jobID || items[0]["status"] != "PENDING" || items[0]["archive_id"] != archive.ID || items[0]["sha256"] != "" || items[0]["manifest_hash"] != "" {
		t.Fatalf("列表元数据不符: %v", items[0])
	}
	if _, held := items[0]["content"]; held || strings.Contains(recorder.Body.String(), "content") {
		t.Fatalf("列表泄露文件内容: %s", recorder.Body.String())
	}
	// 完成并落库后列表可直接恢复摘要，UI 不再依赖丢失的建包回执。
	content := []byte("PK listed archive bytes")
	sum := sha256.Sum256(content)
	download := store.ArchiveDownload{ID: "download", TenantID: "t", JobID: jobID, ClaimID: archive.ClaimID, ArchiveID: archive.ID, ManifestHash: archive.ManifestHash, SHA256: hex.EncodeToString(sum[:]), Content: content, CreatedAt: time.Now().UTC()}
	if err := db.Model(&store.Job{}).Where("id = ?", jobID).Update("status", "DONE").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	recorder, _, items = list("?page_size=500")
	if recorder.Code != 200 || len(items) != 1 || items[0]["status"] != "DONE" || items[0]["sha256"] != download.SHA256 || items[0]["manifest_hash"] != archive.ManifestHash {
		t.Fatalf("完成后列表摘要不符 HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	_, summary, _ = list("?page_size=500")
	if summary["page_size"] != 100 || summary["page"] != 1 {
		t.Fatalf("列表分页未按约定收敛: %v", summary)
	}
	// 跨租户或未知档案、撤权后都按既有错误语义拒绝。
	if recorder = archivePackageServe(router, http.MethodGet, "/api/v1/archives/missing-archive/packages", ""); recorder.Code != 404 {
		t.Fatalf("未知档案列表 HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	current.Permissions = []string{"expense.archive.read", "expense.claim.read"}
	if recorder = archivePackageServe(router, http.MethodGet, "/api/v1/archives/"+archive.ID+"/packages", ""); recorder.Code != 403 {
		t.Fatalf("撤权后列表 HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
}
