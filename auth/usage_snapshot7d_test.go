package auth

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestUsageSnapshot7dReplacesMetadataAtomically(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	first := UsageSnapshot7d{Percent: 10, Valid: true, ResetAt: now.Add(time.Hour), WindowSeconds: 604800, UpdatedAt: now}
	second := UsageSnapshot7d{Percent: 80, Valid: true, ResetAt: now.Add(2 * time.Hour), WindowSeconds: 3600, UpdatedAt: now.Add(time.Second)}
	acc := &Account{}
	acc.SetUsageSnapshot7d(first)
	var writers sync.WaitGroup
	for _, snapshot := range []UsageSnapshot7d{first, second} {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for range 2000 {
				acc.SetUsageSnapshot7d(snapshot)
			}
		}()
	}
	for range 2000 {
		if got := acc.GetUsageSnapshot7d(); got != first && got != second {
			t.Errorf("mixed observation: %+v", got)
			break
		}
	}
	writers.Wait()

	acc.SetUsageSnapshot7d(first)
	acc.SetUsageSnapshot7d(UsageSnapshot7d{Percent: 25, Valid: true, UpdatedAt: now})
	got := acc.GetUsageSnapshot7d()
	if got.Percent != 25 || !got.ResetAt.IsZero() || got.WindowSeconds != first.WindowSeconds {
		t.Fatalf("unknown reset must clear stale reset, unknown length must preserve length: %+v", got)
	}
}

func TestPersistUsageSnapshot7dRoundTripAndLegacyCompatibility(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "usage-snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(func() { store.Stop(); _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(ctx, "snapshot", map[string]interface{}{"access_token": "test-token"}, "")
	if err != nil {
		t.Fatal(err)
	}
	acc := &Account{DBID: id, AccessToken: "test-token", Status: StatusReady}
	now := time.Now().UTC().Truncate(time.Second)
	acc.SetUsageSnapshot5hAt(15, now.Add(time.Hour), now)
	want := UsageSnapshot7d{Percent: 20, Valid: true, ResetAt: now.Add(24 * time.Hour), WindowSeconds: 604800, UpdatedAt: now}
	store.PersistUsageSnapshot7d(acc, want)
	reloaded, err := store.BuildTransientAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.GetUsageSnapshot7d(); got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if percent, valid := reloaded.GetUsagePercent5h(); !valid || percent != 15 {
		t.Fatalf("5h snapshot lost: percent=%v valid=%v", percent, valid)
	}

	// Percentage-only writers cannot accidentally erase known reset metadata.
	store.PersistUsageSnapshot(acc, 35)
	if got := acc.GetUsageSnapshot7d(); got.Percent != 35 || got.ResetAt != want.ResetAt || got.WindowSeconds != want.WindowSeconds {
		t.Fatalf("legacy writer changed complete metadata: %+v", got)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil || row.GetCredential("codex_7d_reset_at") != want.ResetAt.Format(time.RFC3339) {
		t.Fatalf("legacy persistence erased reset: row=%+v err=%v", row, err)
	}

	want.Percent = 5
	want.ResetAt = time.Time{}
	store.PersistUsageSnapshot7d(acc, want)
	reloaded, err = store.BuildTransientAccountByID(ctx, id)
	if err != nil || reloaded.GetUsageSnapshot7d() != want {
		t.Fatalf("authoritative reset clearing failed: account=%+v err=%v", reloaded, err)
	}
	store.PersistUsageSnapshot7d(acc, UsageSnapshot7d{})
	row, err = db.GetAccountByID(ctx, id)
	if err != nil || row.GetCredential("codex_7d_used_percent") != "" || row.GetCredential("codex_usage_updated_at") != "" || row.GetCredential("codex_7d_reset_at") != "" {
		t.Fatalf("invalid snapshot retained stale observation: row=%+v err=%v", row, err)
	}
}

func TestPersistUsageSnapshot7dWithoutStore(t *testing.T) {
	var store *Store
	acc := &Account{}
	want := UsageSnapshot7d{Percent: 30, Valid: true, UpdatedAt: time.Now()}
	store.PersistUsageSnapshot7d(acc, want)
	if got := acc.GetUsageSnapshot7d(); got != want {
		t.Fatalf("in-memory observation = %+v, want %+v", got, want)
	}
}
