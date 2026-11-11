package httpapi

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type evidenceHTTPFiles struct{ downloads int }

// Validate 提供此路由契约未使用的文件预检边界。
// 参数：上下文、身份、文件及用途。
// 返回值：无错误。
func (*evidenceHTTPFiles) Validate(context.Context, domain.Actor, []string, string) error { return nil }

// Download 在应用完成真实本地ACL后模拟精确票据边界。
// 参数：上下文、可信身份和已授权文件。
// 返回值：受控测试票据，不代表真实平台验收。
func (f *evidenceHTTPFiles) Download(context.Context, domain.Actor, string) (json.RawMessage, error) {
	f.downloads++
	return json.RawMessage(`{"ticket":"test-only","download_url":"/trusted-file"}`), nil
}

// evidenceHTTP 调用真实Gin路由并断言成功JSON响应。
// 参数：t 为测试上下文，router 为路由，method/path/body 为请求。
// 返回值：响应data字段。
func evidenceHTTP(t *testing.T, router *gin.Engine, method, path, body string) json.RawMessage {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("%s %s=%d %s", method, path, w.Code, w.Body.String())
	}
	var response struct {
		Data json.RawMessage `json:"data"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	return response.Data
}

// TestClaimEvidenceHTTPAndPolicyHints 验证政策真实发布/选项回显及证据路由不会被草稿绑定绕过归属。
// 参数：t 为测试上下文。
// 返回值：无。
func TestClaimEvidenceHTTPAndPolicyHints(t *testing.T) {
	db, e := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "evidence.sqlite")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	if e = db.AutoMigrate(store.Models()...); e != nil {
		t.Fatal(e)
	}
	rawDB, _ := db.DB()
	rawDB.SetMaxOpenConns(1)
	defer rawDB.Close()
	for _, entry := range []struct{ ID, Kind, Raw string }{{"profile", "employee_profiles", `{"id":"profile","subject_id":"app","org_id":"org","grade":"G","route_type":"TECHNICAL","active":true}`}, {"standard", "standards", `{"id":"standard","org_id":"org","fee_type":"TRAVEL","grade":"G","unit":"PER_OCCURRENCE","limit_minor":"100","valid_from":"2026-01-01","requires_travel_facts":true}`}} {
		if e = db.Create(&store.Config{ID: entry.ID, TenantID: "t", Kind: entry.Kind, Version: 1, Published: true, ContentJSON: entry.Raw}).Error; e != nil {
			t.Fatal(e)
		}
	}
	for _, entry := range []struct{ ID, Owner string }{{"owned-proof", "app"}, {"foreign-proof", "other"}} {
		if e = db.Create(&store.Document{ID: entry.ID, TenantID: "t", Owner: entry.Owner, FileID: entry.ID, Status: "READY", Version: 1, ContentJSON: `{"purpose":"reimbursement.claim.evidence"}`}).Error; e != nil {
			t.Fatal(e)
		}
	}
	files := &evidenceHTTPFiles{}
	actor := domain.Actor{TenantID: "t", SubjectID: "app", Permissions: []string{"expense.claim.create", "expense.claim.update", "expense.claim.read", "expense.config.manage", "expense.policy.publish"}}
	router := New(application.New(db, application.Dependencies{Files: files, Personnel: httpPersonnel{}}), func(c *gin.Context) { c.Set("expense_actor", actor); c.Next() })
	created := evidenceHTTP(t, router, "POST", "/api/v1/configurations/historical_tail_policies", `{"command_id":"tail-draft","org_id":"org","project_id":"p1","expense_cutoff":"2026-11-10","valid_from":"2026-01-01"}`)
	var config struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	_ = json.Unmarshal(created, &config)
	evidenceHTTP(t, router, "POST", "/api/v1/configurations/historical_tail_policies/"+config.ID+"/publish", `{"command_id":"tail-publish","expected_version":1}`)
	options := evidenceHTTP(t, router, "GET", "/api/v1/claim-options", "")
	var hints struct {
		Standards []domain.Standard             `json:"standards"`
		Policies  []domain.HistoricalTailPolicy `json:"historical_tail_policies"`
	}
	_ = json.Unmarshal(options, &hints)
	if len(hints.Standards) != 1 || !hints.Standards[0].RequiresTravelFacts || len(hints.Policies) != 1 || hints.Policies[0].Enabled {
		t.Fatal("标准提示或默认关闭政策没有经实际HTTP回显")
	}
	created = evidenceHTTP(t, router, "POST", "/api/v1/claims", `{"command_id":"evidence-draft","org_id":"org","route_type":"TECHNICAL","items":[{"id":"item","historical_tail_evidence_file_ids":["owned-proof","foreign-proof"],"travel_assessment":{"status":"MATCH"}}]}`)
	var claim domain.Claim
	_ = json.Unmarshal(created, &claim)
	if claim.Items[0].TravelAssessment != nil {
		t.Fatal("前端伪造了服务器派生结论")
	}
	evidenceHTTP(t, router, "POST", "/api/v1/claims/"+claim.ID+"/evidence/owned-proof/download-ticket", `{}`)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/claims/"+claim.ID+"/evidence/foreign-proof/download-ticket", strings.NewReader(`{}`)))
	if w.Code != 404 || files.downloads != 1 {
		t.Fatal("草稿绑定他人证据绕过实际文件归属ACL")
	}
}
