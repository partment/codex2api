package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestNativeCompactAutoFastFallbackAndTierPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tier       string
		rules      string
		wantTiers  []string
		wantStatus int
	}{
		{name: "automatic priority falls back", wantTiers: []string{"priority", ""}, wantStatus: http.StatusOK},
		{name: "automatic flex upgrade falls back to supported default", tier: "flex", wantTiers: []string{"priority", ""}, wantStatus: http.StatusOK},
		{name: "fallback applies tier-only default rule", rules: `{"default":[{"params":{"service_tier":"flex","temperature":0.7}}]}`, wantTiers: []string{"priority", ""}, wantStatus: http.StatusOK},
		{name: "explicit ultrafast survives", tier: "ultrafast", wantTiers: []string{"ultrafast"}, wantStatus: http.StatusOK},
		{name: "explicit priority is not downgraded", tier: "priority", wantTiers: []string{"priority"}, wantStatus: http.StatusBadRequest},
		{name: "rule priority is not downgraded", rules: `{"override":[{"params":{"service_tier":"priority","temperature":0.7}}]}`, wantTiers: []string{"priority"}, wantStatus: http.StatusBadRequest},
		{name: "rule default overrides auto fast", rules: `{"override":[{"params":{"service_tier":"default","temperature":0.7}}]}`, wantTiers: []string{""}, wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const model = "gpt-6-astra"
			quotaPriorityUnsupportedModels.Delete(model)
			t.Cleanup(func() { quotaPriorityUnsupportedModels.Delete(model) })
			var mu sync.Mutex
			var tiers, accounts []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := readUpstreamRequestBody(r)
				if gjson.GetBytes(body, quotaPriorityMarkerPath).Exists() || gjson.GetBytes(body, "prompt_cache_retention").Exists() {
					t.Errorf("internal or unsupported field reached compact upstream: %s", body)
				}
				if gjson.GetBytes(body, "temperature").Exists() {
					t.Errorf("native compact applied a non-tier Payload Rule: %s", body)
				}
				tier := extractServiceTier(body)
				mu.Lock()
				tiers = append(tiers, tier)
				accounts = append(accounts, r.Header.Get("Authorization")+"|"+r.Header.Get("Chatgpt-Account-Id"))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if tier == "priority" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","param":"service_tier","message":"Unsupported parameter: service_tier"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"resp_native_compact","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"summary"}]}`)
			}))
			t.Cleanup(upstream.Close)
			previousResin := resinCfg.Load()
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
			t.Cleanup(func() { resinCfg.Store(previousResin) })
			h, row, router := newModelQuotaTestHandler(t, 1, "", true)
			settings := CurrentRuntimeSettings()
			settings.CompactViaResponses = false
			settings.CodexForceWebsocket = false
			settings.CodexPriorityServiceTierEnabled = true
			settings.CodexPriorityMinRemainingRatio = 0
			ApplyRuntimeSettings(settings)
			withPayloadRules(t, tc.rules)

			body := `{"model":"gpt-6-astra","input":[{"role":"user","content":"Compact this"}],"prompt_cache_retention":"24h"}`
			if tc.tier != "" {
				body, _ = sjson.Set(body, "service_tier", tc.tier)
			}
			response := performModelQuotaRequest(router, "/v1/responses/compact", body)
			if response.Code != tc.wantStatus {
				t.Fatalf("response=%d %s, want %d", response.Code, response.Body.String(), tc.wantStatus)
			}
			mu.Lock()
			gotTiers, gotAccounts := slices.Clone(tiers), slices.Clone(accounts)
			mu.Unlock()
			if !slices.Equal(gotTiers, tc.wantTiers) {
				t.Fatalf("upstream tiers=%v, want %v", gotTiers, tc.wantTiers)
			}
			for _, account := range gotAccounts {
				if account != gotAccounts[0] {
					t.Fatalf("fallback changed account: %v", gotAccounts)
				}
			}
			usage, err := h.db.GetAPIKeyModelRequestUsage(context.Background(), row.ID, row.Limits.ModelRequestLimits, time.Now())
			if err != nil || len(usage) != 1 || usage[0].Used != 1 {
				t.Fatalf("compact attempt must charge once: usage=%#v err=%v", usage, err)
			}
			for _, account := range h.store.Accounts() {
				if account.ActiveRequests.Load() != 0 {
					t.Fatal("compact fallback leaked account lease")
				}
			}
			if got := quotaPriorityModelTemporarilyUnsupported(model, time.Now()); got != (len(tc.wantTiers) == 2) {
				t.Fatalf("unsupported model cache=%v, want only automatic fallback to mark it", got)
			}
		})
	}
}

// TestAutoFastMessagesPreservesWebSearch covers Auto Fast through the Messages search bridge.
func TestAutoFastMessagesPreservesWebSearch(t *testing.T) {
	const model = "gpt-6-astra"
	quotaPriorityUnsupportedModels.Delete(model)
	t.Cleanup(func() { quotaPriorityUnsupportedModels.Delete(model) })
	var rounds, searches atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			searches.Add(1)
			_, _ = io.WriteString(w, `{"output":"Python website","results":[{"url":"https://www.python.org/","title":"Python"}]}`)
			return
		}
		body := readUpstreamRequestBody(r)
		if extractServiceTier(body) != "priority" || gjson.GetBytes(body, quotaPriorityMarkerPath).Exists() {
			t.Errorf("Auto Fast tier or internal marker changed: %s", body)
		}
		if rounds.Add(1) == 1 {
			if gjson.GetBytes(body, "tools.0.type").String() != "function" || gjson.GetBytes(body, "tools.0.name").String() != "codex2api_web_search" {
				t.Errorf("hosted search was not bridged: %s", body)
			}
			writeCodexSSE(w, codexSearchTestCompleted(codexSearchTestCall("codex2api_web_search", "Python")))
			return
		}
		writeCodexSSE(w, codexSearchTestCompleted(map[string]any{
			"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Python website"}},
		}))
	}))
	t.Cleanup(upstream.Close)
	previousResin, previousSearchURL := resinCfg.Load(), codexAlphaSearchURLForTest
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
	codexAlphaSearchURLForTest = upstream.URL + "/search"
	t.Cleanup(func() { resinCfg.Store(previousResin); codexAlphaSearchURLForTest = previousSearchURL })
	h, row, router := newModelQuotaTestHandler(t, 1, "", true)
	settings := CurrentRuntimeSettings()
	settings.CodexPriorityServiceTierEnabled = true
	settings.CodexPriorityMinRemainingRatio = 0
	ApplyRuntimeSettings(settings)
	response := performModelQuotaRequest(router, "/v1/messages", `{"model":"gpt-6-astra","max_tokens":100,"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}],"messages":[{"role":"user","content":"Find Python"}]}`)
	if response.Code != http.StatusOK || rounds.Load() != 2 || searches.Load() != 1 || !strings.Contains(response.Body.String(), "web_search_tool_result") || !strings.Contains(response.Body.String(), "Python website") {
		t.Fatalf("Messages search failed: status=%d rounds=%d searches=%d body=%s", response.Code, rounds.Load(), searches.Load(), response.Body.String())
	}
	usage, err := h.db.GetAPIKeyModelRequestUsage(context.Background(), row.ID, row.Limits.ModelRequestLimits, time.Now())
	if err != nil || len(usage) != 1 || usage[0].Used != 1 {
		t.Fatalf("search continuation charged quota more than once: usage=%+v err=%v", usage, err)
	}
}

// TestCodexUsageHeadersPersistComplete7dSnapshot keeps Auto Fast quota metadata across reloads.
func TestCodexUsageHeadersPersistComplete7dSnapshot(t *testing.T) {
	h, _, _ := newModelQuotaTestHandler(t, 1, "", true)
	ctx := context.Background()
	id, err := h.db.InsertAccountWithCredentials(ctx, "header-snapshot", map[string]any{"access_token": "test-token", "plan_type": "pro"}, "")
	if err != nil {
		t.Fatal(err)
	}
	account := h.store.FindByID(id)
	if account == nil {
		t.Fatal("fixture account missing")
	}
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "20")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-primary-reset-after-seconds", "3600")
	SyncCodexUsageState(h.store, account, &http.Response{StatusCode: http.StatusOK, Header: headers})
	want := account.GetUsageSnapshot7d()
	reloaded, err := h.store.BuildTransientAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.GetUsageSnapshot7d()
	if !got.Valid || got.Percent != 20 || got.ResetAt.IsZero() || !got.ResetAt.Equal(want.ResetAt.Truncate(time.Second)) || got.WindowSeconds != 604800 || !got.UpdatedAt.Equal(want.UpdatedAt.Truncate(time.Second)) {
		t.Fatalf("incomplete persisted header snapshot: got=%+v want=%+v", got, want)
	}
}
