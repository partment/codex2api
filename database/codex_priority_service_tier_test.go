package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexAutoFastAndKeepaliveSettingsPersistIndependently(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "settings.db")
			if driver == "postgres" {
				dsn = os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
				}
			}
			db, err := New(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			settings := &SystemSettings{PromptFilterCustomPatterns: `[{"id":"fixture"}]`, PromptFilterReviewAPIKey: "fixture-key"}
			for _, tc := range []struct {
				enabled bool
				ratio   float64
				slots   int
			}{
				{true, 0.8, 3},
				{false, 0, 7},
			} {
				settings.CodexPriorityServiceTierEnabled = tc.enabled
				settings.CodexPriorityMinRemainingRatio = tc.ratio
				settings.CodexWSDownstreamKeepaliveSlots = tc.slots
				if err := db.UpdateSystemSettings(ctx, settings); err != nil {
					t.Fatal(err)
				}
				got, err := db.GetSystemSettings(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if got.CodexPriorityServiceTierEnabled != tc.enabled || got.CodexPriorityMinRemainingRatio != tc.ratio || got.CodexWSDownstreamKeepaliveSlots != tc.slots {
					t.Fatalf("Auto Fast/keepalive = %v/%v/%d, want %v/%v/%d", got.CodexPriorityServiceTierEnabled, got.CodexPriorityMinRemainingRatio, got.CodexWSDownstreamKeepaliveSlots, tc.enabled, tc.ratio, tc.slots)
				}
				if got.PromptFilterCustomPatterns != `[{"id":"fixture"}]` || got.PromptFilterReviewAPIKey != "fixture-key" {
					t.Fatal("Auto Fast/keepalive parameters disturbed prompt-filter preservation")
				}
				settings.PreservePromptFilterCustomPatterns = true
				settings.PreservePromptFilterReviewAPIKey = true
				settings.PromptFilterCustomPatterns = "[]"
				settings.PromptFilterReviewAPIKey = ""
			}
		})
	}
}
