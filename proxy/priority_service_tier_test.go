package proxy

import (
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
	for _, model := range []string{"gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark"} {
		got := applyQuotaPriorityServiceTier(account, []byte(`{"model":"`+model+`"}`), 10*time.Minute)
		if gjson.GetBytes(got, "service_tier").Exists() {
			t.Fatalf("service_tier should not be injected for stale/unsupported model %q: %s", model, got)
		}
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
