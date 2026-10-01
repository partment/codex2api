package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestApplyQuotaPriorityServiceTier(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexPriorityServiceTierEnabled = true
	settings.CodexPriorityMinRemainingRatio = 0.5
	ApplyRuntimeSettings(settings)

	now := time.Now()
	account := &auth.Account{}
	account.SetUsageSnapshot5hAt(50, now.Add(4*time.Hour), now)
	account.SetUsageSnapshot(49.9, now)
	account.SetReset7dAt(now.Add(6 * 24 * time.Hour))

	got := applyQuotaPriorityServiceTier(account, []byte(`{"model":"gpt-5.4","service_tier":"flex"}`), 10*time.Minute)
	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("service_tier = %q, want priority", tier)
	}

	account.SetUsageSnapshot5hAt(50.1, now.Add(4*time.Hour), now)
	got = applyQuotaPriorityServiceTier(account, []byte(`{"model":"gpt-5.4"}`), 10*time.Minute)
	if gjson.GetBytes(got, "service_tier").Exists() {
		t.Fatalf("service_tier should not be injected when 5h remaining quota is below 50%%: %s", got)
	}
}

func TestApplyQuotaPriorityServiceTierUsesLongWindowWhen5hIsAbsent(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexPriorityServiceTierEnabled = true
	settings.CodexPriorityMinRemainingRatio = 0.5
	ApplyRuntimeSettings(settings)

	now := time.Now()
	account := &auth.Account{}
	account.SetUsageSnapshot(20, now)
	account.SetReset7dAt(now.Add(6 * 24 * time.Hour))

	got := applyQuotaPriorityServiceTier(account, []byte(`{"model":"gpt-5.5"}`), 10*time.Minute)
	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("service_tier = %q, want priority from long-window quota", tier)
	}
}

func TestQuotaPriorityServiceTierLoggingAndPayloadRulePrecedence(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexPriorityServiceTierEnabled = true
	settings.CodexPriorityMinRemainingRatio = 0.5
	ApplyRuntimeSettings(settings)
	withPayloadRules(t, `{"override":[{"params":{"service_tier":"default"}}]}`)

	now := time.Now()
	account := &auth.Account{}
	account.SetUsageSnapshot(20, now)
	account.SetReset7dAt(now.Add(6 * 24 * time.Hour))
	autoBody := applyQuotaPriorityServiceTier(account, []byte(`{"model":"gpt-5.6-sol","service_tier":"flex"}`), 10*time.Minute)
	if tier := extractServiceTier(autoBody); tier != "priority" {
		t.Fatalf("auto service_tier = %q, want priority", tier)
	}
	if tier := EffectiveRequestedServiceTier(autoBody, "gpt-5.6-sol", nil, nil); tier != "default" {
		t.Fatalf("payload-rule service_tier = %q, want default", tier)
	}

	tiers := resolveUsageServiceTiers("", extractServiceTier(autoBody))
	if tiers.ServiceTier != "fast" || tiers.RequestedServiceTier != "priority" || tiers.BillingServiceTier != "priority" {
		t.Fatalf("usage tiers = %#v, want display fast and requested/billing priority", tiers)
	}
}

func TestApplyQuotaPriorityServiceTierRejectsStaleOrUnsupportedQuota(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexPriorityServiceTierEnabled = true
	settings.CodexPriorityMinRemainingRatio = 0.5
	ApplyRuntimeSettings(settings)

	now := time.Now()
	account := &auth.Account{}
	account.SetUsageSnapshot(20, now.Add(-11*time.Minute))
	account.SetReset7dAt(now.Add(6 * 24 * time.Hour))
	got := applyQuotaPriorityServiceTier(account, []byte(`{"model":"gpt-7-new"}`), 10*time.Minute)
	if gjson.GetBytes(got, "service_tier").Exists() {
		t.Fatalf("service_tier should not be injected for stale quota: %s", got)
	}
}

func TestQuotaPriorityServiceTierSupportsAllCodexTextModelsExceptBlacklist(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexPriorityServiceTierEnabled = true
	settings.CodexPriorityMinRemainingRatio = 0
	ApplyRuntimeSettings(settings)

	account := &auth.Account{}
	for _, model := range []string{"gpt-5.4-mini", "gpt-6-astra", "gpt-7-new", "codex-auto-review", "gpt-reserve"} {
		got := applyQuotaPriorityServiceTier(account, []byte(`{"model":"`+model+`"}`), 10*time.Minute)
		if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
			t.Errorf("model %q service_tier = %q, want priority", model, tier)
		}
	}

	for _, model := range []string{"gpt-5.3-codex-spark", "GPT-5.3-CODEX-SPARK(xhigh)", "gpt-image-2", "gpt-image-2-4k"} {
		got := applyQuotaPriorityServiceTier(account, []byte(`{"model":"`+model+`"}`), 10*time.Minute)
		if gjson.GetBytes(got, "service_tier").Exists() {
			t.Errorf("model %q must not receive automatic Fast: %s", model, got)
		}
	}
}

func TestQuotaPriorityUnsupportedModelCacheExpires(t *testing.T) {
	model := "gpt-7-cache-test"
	quotaPriorityUnsupportedModels.Delete(model)
	t.Cleanup(func() { quotaPriorityUnsupportedModels.Delete(model) })

	now := time.Now()
	markQuotaPriorityModelUnsupported("GPT-7-CACHE-TEST(xhigh)", now)
	if supportsQuotaPriorityServiceTier(model, now.Add(time.Minute)) {
		t.Fatal("temporarily unsupported model should be skipped")
	}
	if !supportsQuotaPriorityServiceTier(model, now.Add(quotaPriorityUnsupportedCacheTTL+time.Second)) {
		t.Fatal("expired unsupported-model cache should allow Fast again")
	}
}

func TestQuotaPriorityMarkerRestoresOriginalTier(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexPriorityServiceTierEnabled = true
	settings.CodexPriorityMinRemainingRatio = 0
	ApplyRuntimeSettings(settings)

	marked := applyQuotaPriorityServiceTier(&auth.Account{}, []byte(`{"model":"gpt-7-new","service_tier":"flex"}`), 0)
	cleaned, fallback, model, ok := consumeQuotaPriorityServiceTierMarker(marked)
	if !ok || model != "gpt-7-new" {
		t.Fatalf("marker = (%t,%q), want true/gpt-7-new", ok, model)
	}
	if gjson.GetBytes(cleaned, quotaPriorityMarkerPath).Exists() || extractServiceTier(cleaned) != "priority" {
		t.Fatalf("cleaned auto Fast body = %s", cleaned)
	}
	if tier := extractServiceTier(fallback); tier != "flex" {
		t.Fatalf("fallback tier = %q, want flex; body=%s", tier, fallback)
	}
}

func TestQuotaPriorityFallbackRespectsExplicitPayloadRule(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexPriorityServiceTierEnabled = true
	settings.CodexPriorityMinRemainingRatio = 0
	ApplyRuntimeSettings(settings)
	withPayloadRules(t, `{"override":[{"params":{"service_tier":"priority"}}]}`)

	marked := applyQuotaPriorityServiceTier(&auth.Account{}, []byte(`{"model":"gpt-7-new"}`), 0)
	primary, fallback, _, ok := consumeQuotaPriorityServiceTierMarker(marked)
	if !ok {
		t.Fatal("auto Fast marker missing")
	}
	if quotaPriorityFallbackOwned(primary, fallback, nil, nil) {
		t.Fatal("an explicit Payload Rule priority tier must not be silently downgraded")
	}
}

func TestRetryQuotaPriorityUnsupportedHTTPResponse(t *testing.T) {
	model := "gpt-7-http-fallback"
	quotaPriorityUnsupportedModels.Delete(model)
	t.Cleanup(func() { quotaPriorityUnsupportedModels.Delete(model) })

	first := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"service_tier","message":"Unsupported value: priority is not supported with this model"}}`)),
	}
	retries := 0
	got, err := retryQuotaPriorityUnsupportedResponse(first, nil, model, func() (*http.Response, error) {
		retries++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})
	if err != nil || got.StatusCode != http.StatusOK || retries != 1 {
		t.Fatalf("fallback = status %d retries %d err %v", got.StatusCode, retries, err)
	}
	if !quotaPriorityModelTemporarilyUnsupported(model, time.Now()) {
		t.Fatal("unsupported model should be cached after fallback")
	}
}

func TestRetryQuotaPriorityPreservesSupportedResponse(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		contentType string
		body        string
	}{
		{
			name:       "unrelated HTTP error",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"code":"invalid_request_error","param":"input","message":"Invalid input"}}`,
		},
		{
			name:        "successful SSE first event",
			statusCode:  http.StatusOK,
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\ndata: {\"type\":\"response.completed\"}\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: tc.statusCode,
				Header:     http.Header{"Content-Type": []string{tc.contentType}},
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
			retries := 0
			got, err := retryQuotaPriorityUnsupportedResponse(response, nil, "gpt-7-supported", func() (*http.Response, error) {
				retries++
				return nil, nil
			})
			if err != nil {
				t.Fatalf("retryQuotaPriorityUnsupportedResponse error: %v", err)
			}
			defer got.Body.Close()
			body, err := io.ReadAll(got.Body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if retries != 0 || string(body) != tc.body {
				t.Fatalf("retries=%d body=%q, want 0 and %q", retries, body, tc.body)
			}
		})
	}
}

func TestExecuteRequestRetriesAutoFastSSEFailureOnSameAccount(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	settings := previousSettings
	settings.CodexPriorityServiceTierEnabled = true
	settings.CodexPriorityMinRemainingRatio = 0
	ApplyRuntimeSettings(settings)

	model := "gpt-5.6-sol"
	quotaPriorityUnsupportedModels.Delete(model)
	t.Cleanup(func() { quotaPriorityUnsupportedModels.Delete(model) })
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

	account := &auth.Account{DBID: 42}
	calls := 0
	WebsocketExecuteFunc = func(_ context.Context, gotAccount *auth.Account, body []byte, _ string, _ string, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		calls++
		if gotAccount != account {
			t.Fatalf("fallback account changed: got %p want %p", gotAccount, account)
		}
		if gjson.GetBytes(body, quotaPriorityMarkerPath).Exists() {
			t.Fatalf("internal Auto Fast marker reached upstream: %s", body)
		}
		if !gjson.GetBytes(body, codexResponsesLiteWSMetadataPath).Bool() {
			t.Fatalf("Responses Lite signal was lost on call %d: %s", calls, body)
		}
		tier := extractServiceTier(body)
		if calls == 1 {
			if tier != "priority" {
				t.Fatalf("first request tier = %q, want priority", tier)
			}
			failure := strings.Join([]string{
				`data: {"type":"response.failed","response":{"status_code":400,"error":{"code":"unsupported_parameter","param":"service_tier","message":"Unsupported parameter: service_tier"}}}`,
				``,
			}, "\n")
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(failure))}, nil
		}
		if tier != "" {
			t.Fatalf("fallback request tier = %q, want empty", tier)
		}
		success := `data: {"type":"response.completed","response":{"id":"resp_default","status":"completed"}}` + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(success))}, nil
	}

	body := applyQuotaPriorityServiceTier(account, []byte(`{"model":"`+model+`","input":"hello"}`), 0)
	headers := make(http.Header)
	headers.Set(codexResponsesLiteHeader, "true")
	resp, err := ExecuteRequest(context.Background(), account, body, "session-1", "", "", &DeviceProfileConfig{}, headers, true)
	if err != nil {
		t.Fatalf("ExecuteRequest error: %v", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(responseBody), "response.completed") || calls != 2 {
		t.Fatalf("response=%s calls=%d err=%v", responseBody, calls, err)
	}
}

func TestApplyQuotaPriorityServiceTierDisabledByDefault(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	ApplyRuntimeSettings(DefaultRuntimeSettings())

	account := &auth.Account{}
	account.SetUsageSnapshot5hAt(10, time.Time{}, time.Now())
	account.SetUsageSnapshot(10, time.Now())
	body := []byte(`{"model":"gpt-5.4"}`)
	got := applyQuotaPriorityServiceTier(account, body, 10*time.Minute)
	if gjson.GetBytes(got, "service_tier").Exists() {
		t.Fatalf("service_tier should not be injected while disabled: %s", got)
	}
	if ratio := DefaultRuntimeSettings().CodexPriorityMinRemainingRatio; ratio != 0.5 {
		t.Fatalf("default minimum remaining ratio = %v, want 0.5", ratio)
	}
}

func TestQuotaPriorityServiceTierUsesConfiguredMinimumRemainingRatio(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	now := time.Now()
	account := &auth.Account{}
	account.SetReset7dAt(now.Add(6 * 24 * time.Hour))
	body := []byte(`{"model":"gpt-5.4"}`)
	settings := previous
	settings.CodexPriorityServiceTierEnabled = true

	for _, tc := range []struct {
		name              string
		minRemainingRatio float64
		usedPercent       float64
		want              bool
	}{
		{name: "80 percent boundary", minRemainingRatio: 0.8, usedPercent: 20, want: true},
		{name: "below 80 percent remaining", minRemainingRatio: 0.8, usedPercent: 20.1, want: false},
		{name: "20 percent boundary", minRemainingRatio: 0.2, usedPercent: 80, want: true},
		{name: "below 20 percent remaining", minRemainingRatio: 0.2, usedPercent: 80.1, want: false},
		{name: "zero boundary", minRemainingRatio: 0, usedPercent: 100, want: true},
		{name: "zero ignores over-quota snapshot", minRemainingRatio: 0, usedPercent: 100.1, want: true},
		{name: "full quota boundary", minRemainingRatio: 1, usedPercent: 0, want: true},
		{name: "used quota at one", minRemainingRatio: 1, usedPercent: 0.1, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings.CodexPriorityMinRemainingRatio = tc.minRemainingRatio
			ApplyRuntimeSettings(settings)
			account.SetUsageSnapshot(tc.usedPercent, now)

			got := shouldApplyQuotaPriorityServiceTier(account, body, 10*time.Minute, now)
			if got != tc.want {
				t.Fatalf("shouldApplyQuotaPriorityServiceTier() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestQuotaPriorityServiceTierZeroRatioDoesNotRequireQuotaSnapshot(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexPriorityServiceTierEnabled = true
	settings.CodexPriorityMinRemainingRatio = 0
	ApplyRuntimeSettings(settings)

	now := time.Now()
	accounts := map[string]*auth.Account{
		"missing snapshot": {},
		"stale snapshot": {
			UsagePercent7d:      20,
			UsagePercent7dValid: true,
			Reset7dAt:           now.Add(6 * 24 * time.Hour),
			UsageUpdatedAt:      now.Add(-time.Hour),
			UsagePercent5h:      20,
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(4 * time.Hour),
			UsageUpdatedAt5h:    now.Add(-time.Hour),
		},
		"pre-reset snapshot": {
			UsagePercent7d:      20,
			UsagePercent7dValid: true,
			Reset7dAt:           now.Add(-time.Minute),
			UsageUpdatedAt:      now.Add(-2 * time.Minute),
		},
	}

	for name, account := range accounts {
		t.Run(name, func(t *testing.T) {
			if !shouldApplyQuotaPriorityServiceTier(account, []byte(`{"model":"gpt-5.6-sol"}`), 10*time.Minute, now) {
				t.Fatal("zero minimum remaining ratio should not require a quota snapshot")
			}
		})
	}
}

func TestNormalizeRuntimeSettingsCodexPriorityServiceTierRatio(t *testing.T) {
	settings := DefaultRuntimeSettings()
	settings.CodexPriorityMinRemainingRatio = 0
	if got := NormalizeRuntimeSettings(settings).CodexPriorityMinRemainingRatio; got != 0 {
		t.Fatalf("zero ratio = %v, want 0", got)
	}

	settings.CodexPriorityMinRemainingRatio = -0.1
	if got := NormalizeRuntimeSettings(settings).CodexPriorityMinRemainingRatio; got != 0.5 {
		t.Fatalf("negative ratio = %v, want 0.5", got)
	}

	settings.CodexPriorityMinRemainingRatio = 1.1
	if got := NormalizeRuntimeSettings(settings).CodexPriorityMinRemainingRatio; got != 0.5 {
		t.Fatalf("ratio above one = %v, want 0.5", got)
	}
}

func TestApplyRuntimeSettingsFromSystemCodexPriorityServiceTierRatio(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	settings := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		CodexPriorityServiceTierEnabled: true,
		CodexPriorityMinRemainingRatio:  0.8,
	})
	if !settings.CodexPriorityServiceTierEnabled || settings.CodexPriorityMinRemainingRatio != 0.8 {
		t.Fatalf("runtime auto Fast settings = (%t,%v), want (true,0.8)", settings.CodexPriorityServiceTierEnabled, settings.CodexPriorityMinRemainingRatio)
	}
}
