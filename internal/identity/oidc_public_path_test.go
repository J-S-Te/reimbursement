package identity

import "testing"

// TestPublicPathPreservesApplicationReturn 用途：防止前端完整返回路径重复拼接报销前缀。
// 参数：t为纯路径回归上下文，不访问身份服务或数据库。
// 返回值：无；登录回跳或退出地址不符合应用边界时失败。
func TestPublicPathPreservesApplicationReturn(t *testing.T) {
	auth := &OIDCAuthenticator{options: OIDCOptions{PathPrefix: "/reimbursement"}}
	cases := map[string]string{
		"": "/reimbursement/", "/": "/reimbursement/",
		"/claims":                            "/reimbursement/claims",
		"/logged-out":                        "/reimbursement/logged-out",
		"/reimbursement":                     "/reimbursement",
		"/reimbursement/":                    "/reimbursement/",
		"/reimbursement/claims?status=DRAFT": "/reimbursement/claims?status=DRAFT",
		"/reimbursement?section=claims":      "/reimbursement?section=claims",
		"/reimbursement-other":               "/reimbursement/reimbursement-other",
	}
	for input, expected := range cases {
		if actual := auth.PublicPath(input); actual != expected {
			t.Errorf("PublicPath(%q)=%q, want %q", input, actual, expected)
		}
	}
}
