package auth

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

type runtimeWriteErrorCache struct {
	cache.TokenCache
	err error
}

func (c runtimeWriteErrorCache) SetRuntime(context.Context, string, string, json.RawMessage, time.Duration) error {
	return c.err
}

func TestUsageSnapshot7dReadIsAtomic(t *testing.T) {
	first := UsageSnapshot7d{
		Percent:       11,
		Valid:         true,
		ResetAt:       time.Unix(100, 0),
		WindowSeconds: 111,
		UpdatedAt:     time.Unix(101, 0),
	}
	second := UsageSnapshot7d{
		Percent:       22,
		Valid:         true,
		ResetAt:       time.Unix(200, 0),
		WindowSeconds: 222,
		UpdatedAt:     time.Unix(202, 0),
	}
	account := &Account{}
	account.SetUsageSnapshot7d(first)

	var wg sync.WaitGroup
	errors := make(chan UsageSnapshot7d, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20_000; i++ {
			if i%2 == 0 {
				account.SetUsageSnapshot7d(second)
			} else {
				account.SetUsageSnapshot7d(first)
			}
		}
	}()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20_000; i++ {
				snapshot := account.GetUsageSnapshot7d()
				if snapshot != first && snapshot != second {
					select {
					case errors <- snapshot:
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case snapshot := <-errors:
		t.Fatalf("read mixed 7d snapshot: %+v", snapshot)
	default:
	}
}

func TestPersistUsageSnapshotStoresComplete7dObservationAndRecovery(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "snapshot", map[string]interface{}{
		"access_token": "token",
		"account_id":   "workspace-snapshot",
		"plan_type":    "plus",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &Account{DBID: id, AccessToken: "token", AccountID: "workspace-snapshot", PlanType: "plus"}
	recoveredAt := time.Date(2026, 8, 1, 1, 2, 3, 456789000, time.UTC)
	store.PersistUsageSnapshot7d(account, UsageSnapshot7d{
		Percent:       20,
		Valid:         true,
		ResetAt:       recoveredAt.Add(7 * 24 * time.Hour),
		WindowSeconds: 604_800,
		UpdatedAt:     recoveredAt,
	})
	highAt := recoveredAt.Add(time.Second)
	resetAt := highAt.Add(7 * 24 * time.Hour)
	store.PersistUsageSnapshot7d(account, UsageSnapshot7d{
		Percent:       99,
		Valid:         true,
		ResetAt:       resetAt,
		WindowSeconds: 604_800,
		UpdatedAt:     highAt,
	})

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if pct, ok := row.GetCredentialFloat64("codex_7d_used_percent"); !ok || pct != 99 {
		t.Fatalf("persisted percent = (%v, %v), want (99, true)", pct, ok)
	}
	if seconds, ok := row.GetCredentialInt64("codex_7d_window_seconds"); !ok || seconds != 604_800 {
		t.Fatalf("persisted window seconds = (%d, %v)", seconds, ok)
	}
	for key, want := range map[string]time.Time{
		"codex_7d_reset_at":                   resetAt,
		"codex_usage_updated_at":              highAt,
		"auto_reset_low_balance_recovered_at": recoveredAt,
	} {
		got, parseErr := time.Parse(time.RFC3339, row.GetCredential(key))
		if parseErr != nil || !got.Equal(want) {
			t.Fatalf("%s = %q (%v), want %s", key, row.GetCredential(key), parseErr, want)
		}
	}

	reloaded := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	if err := reloaded.Init(ctx); err != nil {
		t.Fatalf("reloaded.Init: %v", err)
	}
	loaded := reloaded.FindByID(id)
	if loaded == nil {
		t.Fatal("persisted account was not reloaded")
	}
	if snapshot := loaded.GetUsageSnapshot7d(); snapshot.Percent != 99 || !snapshot.Valid || !snapshot.ResetAt.Equal(resetAt) || snapshot.WindowSeconds != 604_800 || !snapshot.UpdatedAt.Equal(highAt) {
		t.Fatalf("reloaded snapshot = %+v", snapshot)
	}
	if state := loaded.GetAutoResetLowBalanceState(); !state.RecoveredAt.Equal(recoveredAt) {
		t.Fatalf("reloaded low-balance state = %+v", state)
	}
}

func TestPersistUsageSnapshot7dClearsUnknownResetAndPreservesKnownWindow(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "snapshot-zero.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	oldResetAt := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	id, err := db.InsertAccountWithCredentials(ctx, "snapshot-zero", map[string]interface{}{
		"access_token":            "token",
		"account_id":              "workspace-snapshot-zero",
		"plan_type":               "plus",
		"codex_7d_used_percent":   10,
		"codex_7d_reset_at":       oldResetAt.Format(time.RFC3339),
		"codex_7d_window_seconds": 604_800,
		"codex_usage_updated_at":  oldResetAt.Add(-time.Hour).Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	account := store.FindByID(id)
	if account == nil {
		t.Fatal("account was not loaded")
	}
	observedAt := oldResetAt.Add(time.Hour)
	store.PersistUsageSnapshot7d(account, UsageSnapshot7d{
		Percent:   25,
		Valid:     true,
		UpdatedAt: observedAt,
	})

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("codex_7d_reset_at"); got != "" {
		t.Fatalf("codex_7d_reset_at = %q, want cleared instead of a year-1 sentinel", got)
	}
	if seconds, ok := row.GetCredentialInt64("codex_7d_window_seconds"); !ok || seconds != 604_800 {
		t.Fatalf("codex_7d_window_seconds = (%d, %v), want (604800, true)", seconds, ok)
	}
	if snapshot := account.GetUsageSnapshot7d(); !snapshot.ResetAt.IsZero() || snapshot.WindowSeconds != 604_800 {
		t.Fatalf("runtime snapshot = %+v", snapshot)
	}

	reloaded := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	if err := reloaded.Init(ctx); err != nil {
		t.Fatalf("reloaded.Init: %v", err)
	}
	if snapshot := reloaded.FindByID(id).GetUsageSnapshot7d(); !snapshot.ResetAt.IsZero() || snapshot.WindowSeconds != 604_800 {
		t.Fatalf("reloaded snapshot = %+v", snapshot)
	}
}

func TestPersistUsageSnapshotCompatibilityPreservesResetMetadata(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	resetAt := time.Now().Add(6 * 24 * time.Hour)
	account := &Account{}
	account.SetUsageSnapshot7d(UsageSnapshot7d{
		Percent:       10,
		Valid:         true,
		ResetAt:       resetAt,
		WindowSeconds: 604_800,
		UpdatedAt:     time.Now().Add(-time.Minute),
	})

	store.PersistUsageSnapshot(account, 42)
	snapshot := account.GetUsageSnapshot7d()
	if snapshot.Percent != 42 || !snapshot.ResetAt.Equal(resetAt) || snapshot.WindowSeconds != 604_800 {
		t.Fatalf("compatibility snapshot = %+v", snapshot)
	}

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "snapshot-compatibility.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	persistedResetAt := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	id, err := db.InsertAccountWithCredentials(context.Background(), "snapshot-compatibility", map[string]interface{}{
		"codex_7d_reset_at":       persistedResetAt.Format(time.RFC3339),
		"codex_7d_window_seconds": 604_800,
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	dbStore := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	dbStore.PersistUsageSnapshot(&Account{DBID: id}, 99)
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("codex_7d_reset_at"); got != persistedResetAt.Format(time.RFC3339) {
		t.Fatalf("codex_7d_reset_at = %q, want preserved", got)
	}
	if seconds, ok := row.GetCredentialInt64("codex_7d_window_seconds"); !ok || seconds != 604_800 {
		t.Fatalf("codex_7d_window_seconds = (%d, %v), want (604800, true)", seconds, ok)
	}
}

func TestAutoResetLowBalanceStateBackendsFallback(t *testing.T) {
	settings := &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"}
	state := AutoResetLowBalanceState{ConsumedAt: time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)}

	t.Run("database_recovers_cache_write_failure", func(t *testing.T) {
		db, err := database.New("sqlite", filepath.Join(t.TempDir(), "state-db-fallback.db"))
		if err != nil {
			t.Fatalf("database.New: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		id, err := db.InsertAccountWithCredentials(context.Background(), "state", map[string]interface{}{
			"access_token": "token",
			"account_id":   "workspace-state-fallback",
			"plan_type":    "plus",
		}, "")
		if err != nil {
			t.Fatalf("InsertAccountWithCredentials: %v", err)
		}
		memory := cache.NewMemory(1)
		t.Cleanup(func() { _ = memory.Close() })
		store := NewStore(db, runtimeWriteErrorCache{TokenCache: memory, err: errors.New("cache unavailable")}, settings)
		account := &Account{DBID: id, AccountID: "workspace-state-fallback"}
		store.AddAccount(account)
		if err := store.SaveAutoResetLowBalanceState(context.Background(), account, state); err != nil {
			t.Fatalf("SaveAutoResetLowBalanceState: %v", err)
		}

		reader := NewStore(db, nil, settings)
		if err := reader.Init(context.Background()); err != nil {
			t.Fatalf("reader.Init: %v", err)
		}
		got, err := reader.LoadAutoResetLowBalanceState(context.Background(), reader.FindByID(id))
		if err != nil || !got.ConsumedAt.Equal(state.ConsumedAt) {
			t.Fatalf("loaded state = %+v, err=%v", got, err)
		}
	})

	t.Run("cache_recovers_database_write_failure", func(t *testing.T) {
		db, err := database.New("sqlite", filepath.Join(t.TempDir(), "state-cache-fallback.db"))
		if err != nil {
			t.Fatalf("database.New: %v", err)
		}
		id, err := db.InsertAccountWithCredentials(context.Background(), "state", map[string]interface{}{
			"access_token": "token",
			"account_id":   "workspace-cache-fallback",
			"plan_type":    "plus",
		}, "")
		if err != nil {
			t.Fatalf("InsertAccountWithCredentials: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close: %v", err)
		}
		memory := cache.NewMemory(1)
		t.Cleanup(func() { _ = memory.Close() })
		store := NewStore(db, memory, settings)
		account := &Account{DBID: id, AccountID: "workspace-cache-fallback"}
		store.AddAccount(account)
		if err := store.SaveAutoResetLowBalanceState(context.Background(), account, state); err != nil {
			t.Fatalf("SaveAutoResetLowBalanceState: %v", err)
		}

		reader := NewStore(nil, memory, settings)
		readerAccount := &Account{DBID: id, AccountID: "workspace-cache-fallback"}
		reader.AddAccount(readerAccount)
		got, err := reader.LoadAutoResetLowBalanceState(context.Background(), readerAccount)
		if err != nil || !got.ConsumedAt.Equal(state.ConsumedAt) {
			t.Fatalf("loaded state = %+v, err=%v", got, err)
		}
	})

	t.Run("all_backends_failed", func(t *testing.T) {
		db, err := database.New("sqlite", filepath.Join(t.TempDir(), "state-all-failed.db"))
		if err != nil {
			t.Fatalf("database.New: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close: %v", err)
		}
		memory := cache.NewMemory(1)
		t.Cleanup(func() { _ = memory.Close() })
		store := NewStore(db, runtimeWriteErrorCache{TokenCache: memory, err: errors.New("cache unavailable")}, settings)
		account := &Account{DBID: 1, AccountID: "workspace-all-failed"}
		store.AddAccount(account)
		if err := store.SaveAutoResetLowBalanceState(context.Background(), account, state); err == nil {
			t.Fatal("SaveAutoResetLowBalanceState() error = nil, want both-backend failure")
		}
		if _, err := store.LoadAutoResetLowBalanceState(context.Background(), account); err == nil {
			t.Fatal("LoadAutoResetLowBalanceState() error = nil on cache miss plus database failure")
		}
	})
}

func TestAutoResetLowBalanceStateNeverMovesBackwardAcrossStores(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "state-monotonic.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(context.Background(), "state", map[string]interface{}{
		"access_token": "token",
		"account_id":   "workspace-state-monotonic",
		"plan_type":    "plus",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	memory := cache.NewMemory(1)
	t.Cleanup(func() { _ = memory.Close() })
	settings := &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"}
	newStore := func() (*Store, *Account) {
		store := NewStore(db, memory, settings)
		account := &Account{DBID: id, AccountID: "workspace-state-monotonic"}
		store.AddAccount(account)
		return store, account
	}
	firstStore, firstAccount := newStore()
	secondStore, secondAccount := newStore()

	newConsumedAt := time.Date(2026, 8, 1, 4, 0, 0, 0, time.UTC)
	if err := firstStore.SaveAutoResetLowBalanceState(context.Background(), firstAccount, AutoResetLowBalanceState{ConsumedAt: newConsumedAt}); err != nil {
		t.Fatalf("save newest consumption: %v", err)
	}
	staleState := AutoResetLowBalanceState{
		ConsumedAt:  newConsumedAt.Add(-2 * time.Hour),
		RecoveredAt: newConsumedAt.Add(-time.Hour),
	}
	if err := secondStore.SaveAutoResetLowBalanceState(context.Background(), secondAccount, staleState); err != nil {
		t.Fatalf("save delayed recovery: %v", err)
	}

	state, err := secondStore.LoadAutoResetLowBalanceState(context.Background(), secondAccount)
	if err != nil {
		t.Fatalf("LoadAutoResetLowBalanceState: %v", err)
	}
	if !state.ConsumedAt.Equal(newConsumedAt) || !state.RecoveredAt.Equal(staleState.RecoveredAt) {
		t.Fatalf("merged state = %+v", state)
	}
	if !state.ConsumedAt.After(state.RecoveredAt) {
		t.Fatalf("delayed recovery rearmed a newer consumption: %+v", state)
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	persistedConsumedAt, err := time.Parse(time.RFC3339, row.GetCredential("auto_reset_low_balance_consumed_at"))
	if err != nil || !persistedConsumedAt.Equal(newConsumedAt) {
		t.Fatalf("persisted consumed_at = %q (%v), want %s", row.GetCredential("auto_reset_low_balance_consumed_at"), err, newConsumedAt)
	}
}
