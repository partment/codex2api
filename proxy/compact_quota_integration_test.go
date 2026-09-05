package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesCompactWebsocketFallbackPreservesModelQuota(t *testing.T) {
	var httpCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		body := readUpstreamRequestBody(r)
		if !requestBodyHasCompactionTrigger(body) {
			t.Errorf("HTTP fallback missing compaction trigger: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact_quota\",\"status\":\"completed\",\"output\":[{\"type\":\"compaction\",\"encrypted_content\":\"summary\"}]}}\n\n")
	}))
	defer upstream.Close()
	previousResin := resinCfg.Load()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	h, row, router := newModelQuotaTestHandler(t, 1, "", true)
	settings := CurrentRuntimeSettings()
	settings.CompactViaResponses = true
	settings.CodexForceWebsocket = true
	settings.CodexWSSizeRouter = false
	ApplyRuntimeSettings(settings)

	previousExecute := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExecute })
	var wsCalls atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, _, _, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		if err := ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(body, "model").String()); err != nil {
			return nil, err
		}
		wsCalls.Add(1)
		if account.ID() != 1 {
			t.Errorf("unexpected account %d", account.ID())
		}
		return nil, &websocket.CloseError{Code: websocket.CloseMessageTooBig, Text: "message too big"}
	}

	body := `{"model":"gpt-6-astra","input":[{"role":"user","content":"Compact this"}]}`
	first := performModelQuotaRequest(router, "/v1/responses/compact", body)
	if first.Code != http.StatusOK || gjson.GetBytes(first.Body.Bytes(), "object").String() != "response.compaction" {
		t.Fatalf("fallback response=%d %s", first.Code, first.Body.String())
	}
	second := performModelQuotaRequest(router, "/v1/responses/compact", body)
	if second.Code != http.StatusTooManyRequests || gjson.GetBytes(second.Body.Bytes(), "error.code").String() != "rate_limit_reached" || second.Header().Get("Retry-After") == "" {
		t.Fatalf("quota response=%d %s", second.Code, second.Body.String())
	}
	if wsCalls.Load() != 1 || httpCalls.Load() != 1 {
		t.Fatalf("transport calls: WS=%d HTTP=%d, want 1/1", wsCalls.Load(), httpCalls.Load())
	}
	usage, err := h.db.GetAPIKeyModelRequestUsage(context.Background(), row.ID, row.Limits.ModelRequestLimits, time.Now())
	if err != nil || len(usage) != 1 || usage[0].Used != 1 {
		t.Fatalf("fallback must charge once: usage=%#v err=%v", usage, err)
	}
	for _, account := range h.store.Accounts() {
		if atomic.LoadInt64(&account.ActiveRequests) != 0 {
			t.Fatal("compact fallback or quota rejection leaked account lease")
		}
		if account.FailureStreak != 0 || account.LastFailureKind != "" {
			t.Fatalf("local rejection penalized account: streak=%d kind=%q", account.FailureStreak, account.LastFailureKind)
		}
	}
}
