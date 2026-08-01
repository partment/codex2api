package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestUpstreamResetErrorMessage_CreditsOnlyMapsToChineseAndKeepsRaw(t *testing.T) {
	body := []byte(`{"detail":{"code":"rate_limit_not_resettable","reason":"credits_only"}}`)
	msg := upstreamResetErrorMessage(http.StatusBadRequest, body)

	if !strings.Contains(msg, "额度（credits）计费") {
		t.Errorf("message missing Chinese explanation: %q", msg)
	}
	// 必须保留上游原文，便于排查。
	if !strings.Contains(msg, "rate_limit_not_resettable") || !strings.Contains(msg, "credits_only") {
		t.Errorf("message must retain raw upstream body: %q", msg)
	}
}

func TestUpstreamResetErrorMessage_KnownCodeWithoutReason(t *testing.T) {
	body := []byte(`{"detail":{"code":"rate_limit_not_resettable"}}`)
	msg := upstreamResetErrorMessage(http.StatusBadRequest, body)
	if !strings.Contains(msg, "不支持主动重置") {
		t.Errorf("expected generic not-resettable Chinese message, got %q", msg)
	}
	if !strings.Contains(msg, "rate_limit_not_resettable") {
		t.Errorf("expected raw body retained, got %q", msg)
	}
}

func TestUpstreamResetErrorMessage_UnknownCodeFallsBackToRaw(t *testing.T) {
	body := []byte(`{"detail":{"code":"something_new"}}`)
	msg := upstreamResetErrorMessage(http.StatusBadRequest, body)
	if !strings.Contains(msg, "something_new") {
		t.Errorf("unknown code should fall back to raw body, got %q", msg)
	}
	// 未识别 code 时不应硬塞中文说明。
	if strings.Contains(msg, "（上游：") {
		t.Errorf("unknown code should not be wrapped with Chinese prefix, got %q", msg)
	}
}

func TestUpstreamResetErrorMessage_EmptyBodyUsesStatus(t *testing.T) {
	msg := upstreamResetErrorMessage(http.StatusBadGateway, nil)
	if !strings.Contains(msg, "502") {
		t.Errorf("empty body should report status code, got %q", msg)
	}
}

func TestEarliestAutoResetCreditUsesConsumableUntilAndFutureWindow(t *testing.T) {
	now := time.Date(2026, 7, 12, 4, 0, 0, 0, time.UTC)
	credits := []proxy.WhamResetCreditItem{
		{ID: "past", ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339)},
		{ID: "outside", ConsumableUntil: now.Add(61 * time.Minute).Format(time.RFC3339)},
		{ID: "fallback", ExpiresAt: now.Add(40 * time.Minute).Format(time.RFC3339)},
		{
			ID:              "canonical",
			ExpiresAt:       now.Add(5 * time.Minute).Format(time.RFC3339),
			ConsumableUntil: now.Add(20 * time.Minute).Format(time.RFC3339Nano),
		},
	}

	credit, expiresAt, ok := earliestAutoResetCredit(credits, now, time.Hour)
	if !ok {
		t.Fatal("earliestAutoResetCredit() = no candidate")
	}
	if credit.ID != "canonical" {
		t.Fatalf("credit.ID = %q, want canonical", credit.ID)
	}
	if want := now.Add(20 * time.Minute); !expiresAt.Equal(want) {
		t.Fatalf("expiresAt = %s, want %s", expiresAt, want)
	}
}

func TestAutoResetCreditsLowBalanceRequiresUsable7dSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 12, 4, 0, 0, 0, time.UTC)
	maxAge := 10 * time.Minute
	tests := []struct {
		name      string
		usage     float64
		updatedAt time.Time
		resetAt   time.Time
		want      bool
	}{
		{name: "one_percent_remaining", usage: 99, updatedAt: now.Add(-time.Minute), resetAt: now.Add(24 * time.Hour), want: true},
		{name: "below_threshold", usage: 98.99, updatedAt: now.Add(-time.Minute), resetAt: now.Add(24 * time.Hour)},
		{name: "missing_timestamp", usage: 100, resetAt: now.Add(24 * time.Hour)},
		{name: "stale_snapshot", usage: 100, updatedAt: now.Add(-11 * time.Minute), resetAt: now.Add(24 * time.Hour)},
		{name: "snapshot_before_elapsed_reset", usage: 100, updatedAt: now.Add(-2 * time.Minute), resetAt: now.Add(-time.Minute)},
		{name: "snapshot_after_elapsed_reset", usage: 99, updatedAt: now.Add(-time.Minute), resetAt: now.Add(-2 * time.Minute), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			account := &auth.Account{}
			account.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: tc.usage, Valid: true, ResetAt: tc.resetAt, UpdatedAt: tc.updatedAt})
			if got := autoResetCreditsLowBalance(account, now, maxAge); got != tc.want {
				t.Fatalf("autoResetCreditsLowBalance() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAutoResetCreditsPlanOnlyPlusAndPro(t *testing.T) {
	for _, plan := range []string{"plus", "pro", "prolite", "PRO-LITE"} {
		if !isAutoResetCreditsPlan(plan) {
			t.Errorf("isAutoResetCreditsPlan(%q) = false, want true", plan)
		}
	}
	for _, plan := range []string{"", "free", "team", "k12", "business", "api"} {
		if isAutoResetCreditsPlan(plan) {
			t.Errorf("isAutoResetCreditsPlan(%q) = true, want false", plan)
		}
	}
}

func TestStableAutoResetCreditRequestID(t *testing.T) {
	account := &auth.Account{DBID: 7, AccountID: "workspace-1"}
	credit := proxy.WhamResetCreditItem{ID: "credit-1", ConsumableUntil: "2026-07-12T05:00:00Z"}
	first := stableAutoResetCreditRequestID(account, credit)
	second := stableAutoResetCreditRequestID(account, credit)
	if first == "" || first != second {
		t.Fatalf("stable request IDs = %q / %q", first, second)
	}
	other := stableAutoResetCreditRequestID(account, proxy.WhamResetCreditItem{ID: "credit-2"})
	if first == other {
		t.Fatalf("different credits share request ID %q", first)
	}
	lowBalance := stableAutoResetLowBalanceRequestID(account, "episode-1")
	if lowBalance == "" || lowBalance != stableAutoResetLowBalanceRequestID(account, "episode-1") {
		t.Fatalf("low-balance request ID is not stable: %q", lowBalance)
	}
	if lowBalance == stableAutoResetLowBalanceRequestID(account, "episode-2") {
		t.Fatalf("different low-balance episodes share request ID %q", lowBalance)
	}
}

func TestRunAutoResetCreditsScanConsumesOneAndAuditsAuto(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	runtimeSettings := proxy.DefaultRuntimeSettings()
	runtimeSettings.AutoResetCreditsEnabled = true
	runtimeSettings.AutoResetCreditsBeforeExpiryMin = 60
	proxy.ApplyRuntimeSettings(runtimeSettings)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 11, AccountID: "workspace-11", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(2)
	store.AddAccount(account)

	now := time.Date(2026, 7, 12, 4, 0, 0, 0, time.UTC)
	credit := proxy.WhamResetCreditItem{
		ID:              "credit-expiring",
		ResetType:       "codex_rate_limits",
		Status:          "available",
		ConsumableUntil: now.Add(30 * time.Minute).Format(time.RFC3339),
	}
	var gotRedeemID string
	var gotEventType, gotSource string
	probeDone := make(chan struct{}, 1)
	handler := &Handler{
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			return &proxy.WhamResetCreditsList{AvailableCount: 2, Credits: []proxy.WhamResetCreditItem{credit}}, nil, nil
		},
		consumeResetCredit: func(_ context.Context, _ *auth.Account, _ string, redeemID string) (*proxy.WhamResetResult, *http.Response, error) {
			gotRedeemID = redeemID
			return &proxy.WhamResetResult{WindowsReset: 2}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(_ int64, eventType, source string) {
			gotEventType, gotSource = eventType, source
		},
		probeUsage: func(context.Context, *auth.Account) error {
			probeDone <- struct{}{}
			return nil
		},
	}

	stats := handler.runAutoResetCreditsScan(context.Background(), now)
	if !stats.Enabled || stats.Scanned != 1 || stats.Queried != 1 || stats.Candidates != 1 || stats.Consumed != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if gotRedeemID != stableAutoResetCreditRequestID(account, credit) {
		t.Fatalf("redeem ID = %q, want stable ID", gotRedeemID)
	}
	if gotEventType != "reset_credit" || gotSource != "auto" {
		t.Fatalf("event = %q/%q, want reset_credit/auto", gotEventType, gotSource)
	}
	if count, ok := account.GetRateLimitResetCredits(); !ok || count != 1 {
		t.Fatalf("remaining credits = (%d,%v), want (1,true)", count, ok)
	}
	select {
	case <-probeDone:
	case <-time.After(time.Second):
		t.Fatal("post-reset usage probe was not triggered")
	}
}

func TestRunAutoResetCreditsScanConsumesNearestExpiryAtLowBalance(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	runtimeSettings := proxy.DefaultRuntimeSettings()
	runtimeSettings.AutoResetCreditsLowBalanceEnabled = true
	proxy.ApplyRuntimeSettings(runtimeSettings)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 21, AccountID: "workspace-21", AccessToken: "token", PlanType: "plus"}
	now := time.Date(2026, 7, 12, 4, 0, 0, 0, time.UTC)
	account.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: 99, Valid: true, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now.Add(-time.Minute)})
	store.AddAccount(account)

	nearest := proxy.WhamResetCreditItem{
		ID:              "nearest",
		ResetType:       "codex_rate_limits",
		Status:          "available",
		ConsumableUntil: now.Add(6 * time.Hour).Format(time.RFC3339),
	}
	later := proxy.WhamResetCreditItem{
		ID:              "later",
		ResetType:       "codex_rate_limits",
		Status:          "available",
		ConsumableUntil: now.Add(48 * time.Hour).Format(time.RFC3339),
	}
	var gotRedeemID string
	handler := &Handler{
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			return &proxy.WhamResetCreditsList{AvailableCount: 2, Credits: []proxy.WhamResetCreditItem{later, nearest}}, nil, nil
		},
		consumeResetCredit: func(_ context.Context, _ *auth.Account, _ string, redeemID string) (*proxy.WhamResetResult, *http.Response, error) {
			gotRedeemID = redeemID
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}

	stats := handler.runAutoResetCreditsScan(context.Background(), now)
	if !stats.Enabled || stats.Queried != 1 || stats.Candidates != 1 || stats.Consumed != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if want := stableAutoResetLowBalanceRequestID(account, "initial"); gotRedeemID != want {
		t.Fatalf("redeem ID = %q, want low-balance episode ID %q", gotRedeemID, want)
	}
}

func TestRunAutoResetCreditsScanLowBalanceSkipsBelowThresholdWithoutQuery(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	runtimeSettings := proxy.DefaultRuntimeSettings()
	runtimeSettings.AutoResetCreditsLowBalanceEnabled = true
	proxy.ApplyRuntimeSettings(runtimeSettings)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 22, AccessToken: "token", PlanType: "pro"}
	now := time.Date(2026, 7, 12, 4, 0, 0, 0, time.UTC)
	account.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: 98.9, Valid: true, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now.Add(-time.Minute)})
	store.AddAccount(account)

	queried := false
	handler := &Handler{
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			queried = true
			return nil, nil, nil
		},
	}
	stats := handler.runAutoResetCreditsScan(context.Background(), now)
	if !stats.Enabled || stats.Queried != 0 || stats.Consumed != 0 || queried {
		t.Fatalf("stats=%+v queried=%v, want enabled scan without query", stats, queried)
	}
}

func TestAutoResetLowBalanceConsumesOnlyOnceWhenPostResetProbeFails(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	settings := proxy.DefaultRuntimeSettings()
	settings.AutoResetCreditsLowBalanceEnabled = true
	proxy.ApplyRuntimeSettings(settings)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	now := time.Now()
	account := &auth.Account{DBID: 23, AccountID: "workspace-once", AccessToken: "token", PlanType: "plus"}
	account.SetUsageSnapshot7d(auth.UsageSnapshot7d{
		Percent:   99,
		Valid:     true,
		ResetAt:   now.Add(24 * time.Hour),
		UpdatedAt: now.Add(-time.Minute),
	})
	store.AddAccount(account)
	credit := proxy.WhamResetCreditItem{
		ID:              "low-balance-credit",
		ResetType:       "codex_rate_limits",
		Status:          "available",
		ConsumableUntil: now.Add(24 * time.Hour).Format(time.RFC3339),
	}
	queries := 0
	consumes := 0
	probeDone := make(chan struct{}, 1)
	handler := &Handler{
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			queries++
			return &proxy.WhamResetCreditsList{AvailableCount: 2, Credits: []proxy.WhamResetCreditItem{credit}}, nil, nil
		},
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes++
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage: func(context.Context, *auth.Account) error {
			probeDone <- struct{}{}
			return context.Canceled
		},
	}

	first := handler.runAutoResetCreditsScan(context.Background(), now)
	if first.Consumed != 1 || first.Failed != 0 {
		t.Fatalf("first scan = %+v", first)
	}
	select {
	case <-probeDone:
	case <-time.After(time.Second):
		t.Fatal("failed post-reset probe did not run")
	}
	handler.resetCreditLastSuccess.Delete(resetCreditLockKey(account))
	second := handler.runAutoResetCreditsScan(context.Background(), now.Add(6*time.Minute))
	if second.Queried != 0 || second.Consumed != 0 || queries != 1 || consumes != 1 {
		t.Fatalf("second scan=%+v queries=%d consumes=%d, want same episode blocked", second, queries, consumes)
	}
}

func TestAutoResetLowBalanceRearmsOnlyAfterLowThenNewHighSnapshot(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	settings := proxy.DefaultRuntimeSettings()
	settings.AutoResetCreditsLowBalanceEnabled = true
	proxy.ApplyRuntimeSettings(settings)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	now := time.Now()
	account := &auth.Account{DBID: 24, AccountID: "workspace-rearm", AccessToken: "token", PlanType: "pro"}
	account.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: 99, Valid: true, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now})
	store.AddAccount(account)
	credit := proxy.WhamResetCreditItem{ID: "rearm-credit", ResetType: "codex_rate_limits", Status: "available", ConsumableUntil: now.Add(24 * time.Hour).Format(time.RFC3339)}
	consumes := 0
	handler := &Handler{
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			return &proxy.WhamResetCreditsList{AvailableCount: 2, Credits: []proxy.WhamResetCreditItem{credit}}, nil, nil
		},
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes++
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}
	if stats := handler.runAutoResetCreditsScan(context.Background(), now); stats.Consumed != 1 {
		t.Fatalf("first scan = %+v", stats)
	}
	consumedAt := account.GetAutoResetLowBalanceState().ConsumedAt
	if consumedAt.IsZero() {
		t.Fatal("successful reset did not record consumed episode")
	}

	account.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: 99.5, Valid: true, ResetAt: consumedAt.Add(24 * time.Hour), UpdatedAt: consumedAt.Add(time.Second)})
	handler.resetCreditLastSuccess.Delete(resetCreditLockKey(account))
	if stats := handler.runAutoResetCreditsScan(context.Background(), consumedAt.Add(time.Second)); stats.Queried != 0 || stats.Consumed != 0 {
		t.Fatalf("newer high snapshot rearmed episode: %+v", stats)
	}

	recoveredAt := consumedAt.Add(2 * time.Second)
	account.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: 20, Valid: true, ResetAt: recoveredAt.Add(24 * time.Hour), UpdatedAt: recoveredAt})
	highAt := recoveredAt.Add(time.Second)
	account.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: 99, Valid: true, ResetAt: highAt.Add(24 * time.Hour), UpdatedAt: highAt})
	if stats := handler.runAutoResetCreditsScan(context.Background(), highAt); stats.Consumed != 1 {
		t.Fatalf("low then high snapshot did not rearm episode: %+v", stats)
	}
	if consumes != 2 {
		t.Fatalf("upstream consumes = %d, want 2 episodes", consumes)
	}
}

func TestManualResetAtHighUsageBlocksLowBalanceAutoReset(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	settings := proxy.DefaultRuntimeSettings()
	settings.AutoResetCreditsLowBalanceEnabled = true
	proxy.ApplyRuntimeSettings(settings)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	now := time.Now()
	account := &auth.Account{DBID: 25, AccountID: "workspace-manual-high", AccessToken: "token", PlanType: "plus"}
	account.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: 99, Valid: true, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now})
	store.AddAccount(account)
	manual := &Handler{
		store: store,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}
	if _, failure := manual.consumeResetCreditLocked(context.Background(), account, "manual-high", "manual", nil); failure != nil {
		t.Fatalf("manual consume failure: %+v", failure)
	}
	queried := false
	automatic := &Handler{
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			queried = true
			return nil, nil, nil
		},
	}
	if stats := automatic.runAutoResetCreditsScan(context.Background(), now.Add(time.Minute)); stats.Queried != 0 || stats.Consumed != 0 || queried {
		t.Fatalf("auto scan=%+v queried=%v, want manual reset to block stale high snapshot", stats, queried)
	}
}

func TestAutoResetLowBalanceStateIsSharedAcrossHandlers(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	settings := proxy.DefaultRuntimeSettings()
	settings.AutoResetCreditsLowBalanceEnabled = true
	proxy.ApplyRuntimeSettings(settings)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })

	now := time.Now()
	firstStore := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	firstAccount := &auth.Account{DBID: 26, AccountID: "workspace-shared-episode", AccessToken: "token", PlanType: "plus"}
	firstAccount.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: 99, Valid: true, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now})
	firstStore.AddAccount(firstAccount)
	secondStore := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	secondAccount := &auth.Account{DBID: 27, AccountID: "workspace-shared-episode", AccessToken: "token", PlanType: "plus"}
	secondAccount.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: 99, Valid: true, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now})
	secondStore.AddAccount(secondAccount)
	credit := proxy.WhamResetCreditItem{ID: "shared-credit", ResetType: "codex_rate_limits", Status: "available", ConsumableUntil: now.Add(24 * time.Hour).Format(time.RFC3339)}
	firstHandler := &Handler{
		store: firstStore,
		cache: tc,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			return &proxy.WhamResetCreditsList{AvailableCount: 2, Credits: []proxy.WhamResetCreditItem{credit}}, nil, nil
		},
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}
	if stats := firstHandler.runAutoResetCreditsScan(context.Background(), now); stats.Consumed != 1 {
		t.Fatalf("first handler scan = %+v", stats)
	}
	if err := tc.DeleteRuntime(context.Background(), resetCreditCooldownNamespace, resetCreditLockKey(firstAccount)); err != nil {
		t.Fatalf("DeleteRuntime cooldown: %v", err)
	}
	secondQueries := 0
	secondHandler := &Handler{
		store: secondStore,
		cache: tc,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			secondQueries++
			return nil, nil, nil
		},
	}
	if stats := secondHandler.runAutoResetCreditsScan(context.Background(), now.Add(time.Minute)); stats.Queried != 0 || stats.Consumed != 0 || secondQueries != 0 {
		t.Fatalf("second handler scan=%+v queries=%d, want shared episode blocked", stats, secondQueries)
	}
	if secondAccount.GetAutoResetLowBalanceState().ConsumedAt.IsZero() {
		t.Fatal("second handler did not hydrate shared consumed state")
	}

	consumedAt := firstAccount.GetAutoResetLowBalanceState().ConsumedAt
	recoveredAt := consumedAt.Add(time.Second)
	firstStore.PersistUsageSnapshot7d(firstAccount, auth.UsageSnapshot7d{
		Percent:   20,
		Valid:     true,
		ResetAt:   recoveredAt.Add(24 * time.Hour),
		UpdatedAt: recoveredAt,
	})
	highAt := recoveredAt.Add(time.Second)
	secondAccount.SetUsageSnapshot7d(auth.UsageSnapshot7d{
		Percent:   99,
		Valid:     true,
		ResetAt:   highAt.Add(24 * time.Hour),
		UpdatedAt: highAt,
	})
	decision, err := secondHandler.autoResetLowBalanceDecision(context.Background(), secondAccount, highAt, 10*time.Minute)
	if err != nil || !decision.Eligible || decision.Episode != recoveredAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("decision after cross-handler recovery = %+v, err=%v", decision, err)
	}
}

func TestAutoResetLowBalanceConsumedStateSurvivesRestart(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "restart-state", map[string]interface{}{
		"access_token": "token",
		"account_id":   "workspace-restart-state",
		"plan_type":    "plus",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	account := store.FindByID(id)
	if account == nil {
		t.Fatal("account was not loaded")
	}
	now := time.Now()
	store.PersistUsageSnapshot7d(account, auth.UsageSnapshot7d{Percent: 99, Valid: true, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now})
	handler := &Handler{
		store: store,
		db:    db,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}
	if _, failure := handler.consumeResetCreditLocked(ctx, account, "manual-before-restart", "manual", nil); failure != nil {
		t.Fatalf("consume failure: %+v", failure)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.GetCredential("auto_reset_low_balance_consumed_at") == "" {
		t.Fatal("consumed episode was not persisted")
	}

	reloadedStore := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	if err := reloadedStore.Init(ctx); err != nil {
		t.Fatalf("reloadedStore.Init: %v", err)
	}
	reloaded := reloadedStore.FindByID(id)
	if reloaded == nil || reloaded.GetAutoResetLowBalanceState().ConsumedAt.IsZero() {
		t.Fatalf("reloaded state = %+v", reloaded)
	}
	reloadedHandler := &Handler{store: reloadedStore}
	decision, err := reloadedHandler.autoResetLowBalanceDecision(ctx, reloaded, now.Add(time.Minute), 10*time.Minute)
	if err != nil || decision.Eligible {
		t.Fatalf("decision after restart=%+v err=%v, want blocked", decision, err)
	}
}

func TestLowBalanceValidatorRunsUnderLeaseAndRejectsChangedEpisode(t *testing.T) {
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	now := time.Now()
	account := &auth.Account{DBID: 28, AccountID: "workspace-final-check", AccessToken: "token", PlanType: "plus"}
	account.SetUsageSnapshot7d(auth.UsageSnapshot7d{Percent: 99, Valid: true, ResetAt: now.Add(24 * time.Hour), UpdatedAt: now})
	store.AddAccount(account)
	handler := &Handler{store: store, cache: tc}
	initial, err := handler.autoResetLowBalanceDecision(context.Background(), account, now, 10*time.Minute)
	if err != nil || !initial.Eligible {
		t.Fatalf("initial decision=%+v err=%v", initial, err)
	}
	account.MergeAutoResetLowBalanceState(auth.AutoResetLowBalanceState{ConsumedAt: now.Add(time.Second)})
	upstreamCalled := false
	handler.consumeResetCredit = func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
		upstreamCalled = true
		return nil, nil, nil
	}
	validator := func(ctx context.Context) (bool, error) {
		acquired, leaseErr := tc.AcquireLease(ctx, "reset-credit", resetCreditLockKey(account), "competing-owner", resetCreditLeaseTTL)
		if leaseErr != nil {
			return false, leaseErr
		}
		if acquired {
			_ = tc.ReleaseLease(ctx, "reset-credit", resetCreditLockKey(account), "competing-owner")
			t.Fatal("validator ran before the reset-credit lease was acquired")
		}
		current, decisionErr := handler.autoResetLowBalanceDecision(ctx, account, now, 10*time.Minute)
		return current.Eligible && current.Episode == initial.Episode, decisionErr
	}
	outcome, failure := handler.consumeResetCreditLocked(context.Background(), account, stableAutoResetLowBalanceRequestID(account, initial.Episode), "auto", validator)
	if failure != nil || !outcome.AlreadyHandled || upstreamCalled {
		t.Fatalf("outcome=%+v failure=%+v upstreamCalled=%v", outcome, failure, upstreamCalled)
	}
}

func TestRunAutoResetCreditsScanDisabledDoesNotQuery(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 12, AccessToken: "token", PlanType: "plus"})
	queried := false
	handler := &Handler{
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			queried = true
			return nil, nil, nil
		},
	}

	stats := handler.runAutoResetCreditsScan(context.Background(), time.Now())
	if stats.Enabled || queried {
		t.Fatalf("disabled scan stats=%+v queried=%v", stats, queried)
	}
}

func TestRunAutoResetCreditsScanUsesDatabaseSettingAsAuthority(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	runtimeSettings := proxy.DefaultRuntimeSettings()
	runtimeSettings.AutoResetCreditsEnabled = true
	proxy.ApplyRuntimeSettings(runtimeSettings)

	db := newTestAdminDB(t)
	persisted := defaultBootstrapSettings()
	persisted.AutoResetCreditsEnabled = false
	persisted.AutoResetCreditsBeforeExpiryMin = 60
	if err := db.UpdateSystemSettings(context.Background(), persisted); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 14, AccessToken: "token", PlanType: "plus"})
	queried := false
	handler := &Handler{
		db:    db,
		store: store,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			queried = true
			return nil, nil, nil
		},
	}

	stats := handler.runAutoResetCreditsScan(context.Background(), time.Now())
	if stats.Enabled || queried {
		t.Fatalf("database-disabled scan stats=%+v queried=%v", stats, queried)
	}
}

func TestAutoResetCreditsSkipsImmediatelyAfterManualReset(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	runtimeSettings := proxy.DefaultRuntimeSettings()
	runtimeSettings.AutoResetCreditsEnabled = true
	proxy.ApplyRuntimeSettings(runtimeSettings)

	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 15, AccountID: "workspace-15", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(2)
	store.AddAccount(account)

	queries := 0
	consumes := 0
	manualHandler := &Handler{
		store: store,
		cache: tc,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes++
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}
	autoHandler := &Handler{
		store: store,
		cache: tc,
		queryResetCredits: func(context.Context, *auth.Account, string) (*proxy.WhamResetCreditsList, *http.Response, error) {
			queries++
			return &proxy.WhamResetCreditsList{}, nil, nil
		},
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes++
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}

	lock := manualHandler.resetCreditLock(account)
	lock.Lock()
	_, failure := manualHandler.consumeResetCreditLocked(context.Background(), account, "manual-request", "manual", nil)
	lock.Unlock()
	if failure != nil {
		t.Fatalf("manual consume failure: %+v", failure)
	}

	stats := autoHandler.runAutoResetCreditsScan(context.Background(), time.Now())
	if stats.Queried != 0 || stats.Consumed != 0 || queries != 0 || consumes != 1 {
		t.Fatalf("stats=%+v queries=%d consumes=%d, want no immediate auto reset", stats, queries, consumes)
	}
}

func TestConsumeResetCreditLockedDoesNotReplaySuccessfulAutoRequestID(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 16, AccountID: "workspace-16", AccessToken: "token", PlanType: "pro"}
	account.SetRateLimitResetCredits(2)
	store.AddAccount(account)

	consumes := 0
	events := 0
	handler := &Handler{
		store: store,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes++
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) { events++ },
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}

	first, failure := handler.consumeResetCreditLocked(context.Background(), account, "stable-auto-id", "auto", nil)
	if failure != nil || first.AlreadyHandled {
		t.Fatalf("first outcome=%+v failure=%+v", first, failure)
	}
	second, failure := handler.consumeResetCreditLocked(context.Background(), account, "stable-auto-id", "auto", nil)
	if failure != nil || !second.AlreadyHandled {
		t.Fatalf("second outcome=%+v failure=%+v", second, failure)
	}
	if consumes != 1 || events != 1 {
		t.Fatalf("consumes=%d events=%d, want 1/1", consumes, events)
	}
	if count, ok := account.GetRateLimitResetCredits(); !ok || count != 1 {
		t.Fatalf("remaining credits=(%d,%v), want (1,true)", count, ok)
	}
}

func TestConsumeResetCreditLockedHonorsSharedWorkspaceLease(t *testing.T) {
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 17, AccountID: "workspace-shared", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(1)
	store.AddAccount(account)

	first := &Handler{store: store, cache: tc}
	acquired, release, err := first.acquireResetCreditLease(context.Background(), account)
	if err != nil || !acquired {
		t.Fatalf("first lease = (%v,%v)", acquired, err)
	}
	defer release()

	called := false
	second := &Handler{
		store: store,
		cache: tc,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			called = true
			return nil, nil, nil
		},
	}
	outcome, failure := second.consumeResetCreditLocked(context.Background(), account, "request-2", "auto", nil)
	if failure != nil || !outcome.InProgress || called {
		t.Fatalf("outcome=%+v failure=%+v called=%v, want in-progress without upstream call", outcome, failure, called)
	}
}

func TestAutoConsumeRechecksSharedCooldownAfterLease(t *testing.T) {
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 19, AccountID: "workspace-cooldown", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(2)
	store.AddAccount(account)

	manual := &Handler{
		store: store,
		cache: tc,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}
	if _, failure := manual.consumeResetCreditLocked(context.Background(), account, "manual-before-auto", "manual", nil); failure != nil {
		t.Fatalf("manual consume failure: %+v", failure)
	}

	called := false
	automatic := &Handler{
		store: store,
		cache: tc,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			called = true
			return nil, nil, nil
		},
	}
	outcome, failure := automatic.consumeResetCreditLocked(context.Background(), account, "auto-after-query", "auto", nil)
	if failure != nil || !outcome.AlreadyHandled || called {
		t.Fatalf("outcome=%+v failure=%+v called=%v, want cooldown skip after lease", outcome, failure, called)
	}
}

func TestWaitAutoResetCreditsCancelsPostResetProbe(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 18, AccessToken: "token", PlanType: "plus"}
	store.AddAccount(account)
	probeStarted := make(chan struct{})
	probeStopped := make(chan struct{})
	handler := &Handler{
		store: store,
		probeUsage: func(ctx context.Context, _ *auth.Account) error {
			close(probeStarted)
			<-ctx.Done()
			close(probeStopped)
			return ctx.Err()
		},
	}

	backgroundCtx, cancel := context.WithCancel(context.Background())
	handler.StartAutoResetCredits(backgroundCtx)
	handler.refreshUsageAfterReset(account)
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("post-reset probe did not start")
	}

	cancel()
	done := make(chan struct{})
	go func() {
		handler.WaitAutoResetCredits()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WaitAutoResetCredits did not return after cancellation")
	}
	select {
	case <-probeStopped:
	default:
		t.Fatal("post-reset probe did not observe cancellation")
	}
}

func TestConsumeResetCreditLockedRefreshes401WithSameRequestID(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 13, AccessToken: "old-token", PlanType: "pro"}
	account.SetRateLimitResetCredits(1)
	store.AddAccount(account)

	var redeemIDs []string
	refreshes := 0
	handler := &Handler{
		store: store,
		consumeResetCredit: func(_ context.Context, _ *auth.Account, _ string, redeemID string) (*proxy.WhamResetResult, *http.Response, error) {
			redeemIDs = append(redeemIDs, redeemID)
			if len(redeemIDs) == 1 {
				return nil, &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"expired"}`))}, nil
			}
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		refreshAccount: func(context.Context, int64) error {
			refreshes++
			account.Mu().Lock()
			account.AccessToken = "new-token"
			account.Mu().Unlock()
			return nil
		},
	}

	outcome, failure := handler.consumeResetCreditLocked(context.Background(), account, "stable-request-id", "auto", nil)
	if failure != nil {
		t.Fatalf("consumeResetCreditLocked failure: %+v", failure)
	}
	if refreshes != 1 || len(redeemIDs) != 2 || redeemIDs[0] != redeemIDs[1] {
		t.Fatalf("refreshes=%d redeemIDs=%v", refreshes, redeemIDs)
	}
	if outcome.WindowsReset != 1 || outcome.Remaining != 0 {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// 手动重置必须等用量探针跑完再响应。此前响应先于探针返回，前端收到成功立刻刷新，
// 拿到的还是旧的用量与状态，表现为「点了重置但进度条和状态没变，得去点测试连接再手动刷新」。
func TestResetCreditsWaitsForUsageProbeBeforeResponding(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 21, AccountID: "workspace-21", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(1)
	store.AddAccount(account)

	probeStarted := make(chan struct{})
	var probeFinished atomic.Bool
	handler := &Handler{
		store: store,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			return &proxy.WhamResetResult{WindowsReset: 2}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage: func(context.Context, *auth.Account) error {
			close(probeStarted)
			// 模拟一次真实的 wham 往返：响应绝不能抢在它前面返回。
			time.Sleep(150 * time.Millisecond)
			probeFinished.Store(true)
			return nil
		},
	}

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "21"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/accounts/21/reset-credits", nil)

	handler.ResetCredits(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	select {
	case <-probeStarted:
	default:
		t.Fatal("usage probe never started")
	}
	if !probeFinished.Load() {
		t.Fatal("response returned before the usage probe finished — the UI would show stale usage")
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if refreshed, _ := payload["usage_refreshed"].(bool); !refreshed {
		t.Errorf("usage_refreshed = %v, want true", payload["usage_refreshed"])
	}
}

// 重置券不可退：客户端断开不得中断已经发出的消费。若消费挂在请求 context 上，
// 断开会把「已经扣掉的券」报成失败，用户重试时另生成幂等键，同一张券被扣两次。
func TestResetCreditsSurvivesClientDisconnect(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 23, AccountID: "workspace-23", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(1)
	store.AddAccount(account)

	var consumeCtxErr error
	consumes := 0
	handler := &Handler{
		store: store,
		consumeResetCredit: func(ctx context.Context, _ *auth.Account, _, _ string) (*proxy.WhamResetResult, *http.Response, error) {
			consumes++
			// 上游往返期间客户端已经走了：这里的 context 不能是已取消的。
			consumeCtxErr = ctx.Err()
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         func(context.Context, *auth.Account) error { return nil },
	}

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "23"}}
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/23/reset-credits", nil)
	// 客户端在网关发出上游请求之前就断开。
	cancelledCtx, cancel := context.WithCancel(req.Context())
	cancel()
	c.Request = req.WithContext(cancelledCtx)

	handler.ResetCredits(c)

	if consumes != 1 {
		t.Fatalf("consume calls = %d, want exactly 1", consumes)
	}
	if consumeCtxErr != nil {
		t.Fatalf("consume ran with a cancelled context (%v) — an already-spent credit would be reported as failure and invite a double spend", consumeCtxErr)
	}
	if remaining, ok := account.GetRateLimitResetCredits(); !ok || remaining != 0 {
		t.Fatalf("remaining credits = %d (ok=%v), want 0 after a completed consume", remaining, ok)
	}
}

// 探针不可用时不能把请求挂住：usage_refreshed 报 false，让前端稍后补刷。
func TestResetCreditsReportsUnrefreshedWhenProbeMissing(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 22, AccountID: "workspace-22", AccessToken: "token", PlanType: "plus"}
	account.SetRateLimitResetCredits(1)
	store.AddAccount(account)

	handler := &Handler{
		store: store,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		recordAccountEvent: func(int64, string, string) {},
		probeUsage:         nil,
	}
	// 关闭后置刷新通道，模拟探针不可用。
	handler.resetCreditPostClosed = true

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "22"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/accounts/22/reset-credits", nil)

	start := time.Now()
	handler.ResetCredits(c)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("handler blocked for %s, want an immediate return when no probe is available", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if refreshed, _ := payload["usage_refreshed"].(bool); refreshed {
		t.Error("usage_refreshed = true, want false when no probe ran")
	}
}
