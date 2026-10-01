package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
				if atomic.LoadInt64(&account.ActiveRequests) != 0 {
					t.Fatal("compact fallback leaked account lease")
				}
			}
			if got := quotaPriorityModelTemporarilyUnsupported(model, time.Now()); got != (len(tc.wantTiers) == 2) {
				t.Fatalf("unsupported model cache=%v, want only automatic fallback to mark it", got)
			}
		})
	}
}
