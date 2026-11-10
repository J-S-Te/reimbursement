package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/reimbursement/internal/domain"
)

// TestProjectValidationBatchesWithoutPartialSuccess 验证大分摊复核分批且任一失败不返回半份资格。
// 参数：t为隔离HTTP适配测试上下文。
// 返回值：无。
func TestProjectValidationBatchesWithoutPartialSuccess(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(fmt.Sprint(failSecond), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/oauth2/token" {
					w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"read","expires_in":300}`))
					return
				}
				calls++
				var input struct {
					ClaimID      string                    `json:"claim_id"`
					DraftVersion int64                     `json:"draft_version"`
					References   []domain.ReferenceRequest `json:"references"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if len(input.References) > 100 || input.ClaimID != "claim" || input.DraftVersion != 7 {
					w.WriteHeader(400)
					return
				}
				if failSecond && calls == 2 {
					w.WriteHeader(503)
					return
				}
				items := make([]domain.ProjectReference, 0, len(input.References))
				for _, ref := range input.References {
					items = append(items, domain.ProjectReference{ProjectID: ref.ProjectID})
				}
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
					"claim_id": input.ClaimID, "draft_version": input.DraftVersion, "items": items,
				}})
			}))
			defer server.Close()
			refs := make([]domain.ReferenceRequest, 205)
			for i := range refs {
				refs[i].ProjectID = fmt.Sprintf("p-%d", i)
			}
			projects := Projects{Client: NewClient(server.URL, server.URL, "client", "secret", "read")}
			got, err := projects.Validate(context.Background(), domain.Actor{TenantID: "t", SubjectID: "app"}, "claim", 7, refs)
			if failSecond {
				if err == nil || len(got) != 0 || calls != 2 {
					t.Fatalf("部分批次错误放行: count=%d calls=%d err=%v", len(got), calls, err)
				}
				return
			}
			if err != nil || len(got) != 205 || calls != 3 || got[204].ProjectID != "p-204" {
				t.Fatalf("有界批次错误: count=%d calls=%d err=%v", len(got), calls, err)
			}
		})
	}
}
