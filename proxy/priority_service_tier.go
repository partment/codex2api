package proxy

import (
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func applyQuotaPriorityServiceTier(account *auth.Account, body []byte, maxAge time.Duration) []byte {
	if !shouldApplyQuotaPriorityServiceTier(account, body, maxAge, time.Now()) {
		return body
	}
	updated, err := sjson.SetBytes(body, "service_tier", "priority")
	if err != nil {
		return body
	}
	return updated
}

func shouldApplyQuotaPriorityServiceTier(account *auth.Account, body []byte, maxAge time.Duration, now time.Time) bool {
	settings := CurrentRuntimeSettings()
	if account == nil || account.IsRelayStyle() || !settings.CodexPriorityServiceTierEnabled {
		return false
	}
	if !supportsQuotaPriorityServiceTier(gjson.GetBytes(body, "model").String()) {
		return false
	}

	account.Mu().RLock()
	pct7d := account.UsagePercent7d
	valid7d := account.UsagePercent7dValid
	reset7dAt := account.Reset7dAt
	updated7dAt := account.UsageUpdatedAt
	pct5h := account.UsagePercent5h
	valid5h := account.UsagePercent5hValid
	reset5hAt := account.Reset5hAt
	updated5hAt := account.UsageUpdatedAt5h
	account.Mu().RUnlock()

	if !quotaPrioritySnapshotFresh(valid7d, updated7dAt, reset7dAt, maxAge, now) || !quotaHasMinimumRemaining(pct7d, settings.CodexPriorityMinRemainingRatio) {
		return false
	}
	if valid5h && (!quotaPrioritySnapshotFresh(true, updated5hAt, reset5hAt, maxAge, now) || !quotaHasMinimumRemaining(pct5h, settings.CodexPriorityMinRemainingRatio)) {
		return false
	}
	return true
}

func quotaHasMinimumRemaining(usedPercent, minRemainingRatio float64) bool {
	// Absorb floating-point error when converting the configured ratio to used percentage.
	return usedPercent <= (1-minRemainingRatio)*100+1e-9
}

func supportsQuotaPriorityServiceTier(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-5.4", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return true
	default:
		return false
	}
}

func quotaPrioritySnapshotFresh(valid bool, updatedAt, resetAt time.Time, maxAge time.Duration, now time.Time) bool {
	if !valid || updatedAt.IsZero() {
		return false
	}
	if maxAge > 0 && now.After(updatedAt) && now.Sub(updatedAt) > maxAge {
		return false
	}
	return resetAt.IsZero() || resetAt.After(now) || !updatedAt.Before(resetAt)
}
