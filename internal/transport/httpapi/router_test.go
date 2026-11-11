package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/reimbursement/internal/application"
	"github.com/j-s-te/reimbursement/internal/domain"
	store "github.com/j-s-te/reimbursement/internal/infrastructure/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type httpPersonnel struct{}

// VerifyApprover 用途：提供HTTP事务夹具已指定的有效审批人员边界。
// 参数：ctx/actor为可信测试请求；target/permission为夹具目标及节点能力。
// 返回值：无错误，仅用于隔离路由测试，不替代真实目录验收。
func (httpPersonnel) VerifyApprover(ctx context.Context, actor domain.Actor, target, permission string) error {
	return nil
}

// CurrentApplicant 用途：为HTTP夹具提供明确的本人在职组织资料。
// 参数：ctx为隔离请求，actor为路由中间件给出的可信测试身份。
// 返回值：测试本人及部门，不从请求正文读取身份。
func (httpPersonnel) CurrentApplicant(ctx context.Context, actor domain.Actor) (domain.PersonOption, error) {
	return domain.PersonOption{ID: actor.SubjectID, Name: "测试申请人",
		Organizations: []domain.OrganizationOption{{ID: "org", Name: "测试部门", IsPrimary: true}}}, nil
}

// testHTTP 创建明确临时SQLite与显式身份替身，仅验证HTTP到真实应用事务的契约。
// 参数：t为测试上下文；actor是该用例模拟的可信认证结果。
// 返回值：HTTP组件路由；不代表真实OIDC验收。
func testHTTP(t *testing.T, actor domain.Actor) *gin.Engine {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(store.Models()...); err != nil {
		t.Fatal(err)
	}
	return New(application.New(db, application.Dependencies{Personnel: httpPersonnel{}}), func(c *gin.Context) { c.Set("expense_actor", actor); c.Next() })
}

// TestDraftHTTPUsesAuthenticatedIdentity 防止客户端伪造申请人和租户进入持久化草稿。
// 参数：t为测试上下文。
// 返回值：无。
func TestDraftHTTPUsesAuthenticatedIdentity(t *testing.T) {
	router := testHTTP(t, domain.Actor{TenantID: "trusted-tenant", SubjectID: "trusted-applicant", Permissions: []string{"expense.claim.create", "expense.claim.read"}})
	request := httptest.NewRequest("POST", "/api/v1/claims", strings.NewReader(`{"command_id":"create-1","title":"未完成草稿","tenant_id":"forged-tenant","applicant_subject_id":"forged-applicant","route_type":"TECHNICAL"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 200 && response.Code != 201 {
		t.Fatalf("draft HTTP status %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Data domain.Claim `json:"data"`
	}
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Data.ID == "" || body.Data.TenantID != "trusted-tenant" || body.Data.Applicant != "trusted-applicant" {
		t.Fatalf("identity contract failed: %s", response.Body.String())
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing request ID")
	}
}

// TestInvoiceHTTPRejectsNumericMoney 防止金额协议错误变成500或被隐式浮点接受。
// 参数：t为测试上下文。
// 返回值：无。
func TestInvoiceHTTPRejectsNumericMoney(t *testing.T) {
	router := testHTTP(t, domain.Actor{TenantID: "t", SubjectID: "applicant", Permissions: []string{"expense.invoice.upload", "expense.invoice.read"}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/invoices", strings.NewReader(`{"command_id":"invoice-1","invoice_type":"VAT","total_minor":100}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 422 {
		t.Fatalf("numeric money status=%d body=%s", response.Code, response.Body.String())
	}
}
