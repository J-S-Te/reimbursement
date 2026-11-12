package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCallWithHeadersForwardsCorrelation 验证审计单事件摄取所需的传输三元组
// 由受控调用方显式下发，且不污染通用 Call。
// 参数：t 为测试上下文。
// 返回值：无；头缺失或串值失败。
func TestCallWithHeadersForwardsCorrelation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{\"access_token\":\"secret\",\"token_type\":\"Bearer\",\"scope\":\"audit.ingest\",\"expires_in\":300}"))
			return
		}
		if r.URL.Path != "/api/v1/audit/events" || r.Method != http.MethodPost {
			t.Fatalf("unexpected call %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Request-ID") != "request-1" || r.Header.Get("X-Correlation-ID") != "correlation-1" || r.Header.Get("traceparent") != "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01" {
			t.Fatalf("missing audit correlation headers: %+v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"data\":{\"event_id\":\"01M77ZZZZZZZZZZZZZZZZZZZYE\",\"status\":\"ACCEPTED\"}}"))
	}))
	defer server.Close()
	c := NewClient(server.URL, server.URL, "audit", "secret", "audit.ingest")
	_, err := c.CallWithHeaders(context.Background(), http.MethodPost, "/api/v1/audit/events", map[string]any{"event_id": "event"}, map[string]string{
		"X-Request-ID": "request-1", "X-Correlation-ID": "correlation-1",
		"traceparent": "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
	})
	if err != nil {
		t.Fatal(err)
	}
}
