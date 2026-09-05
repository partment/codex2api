package proxy

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	quotaPriorityMarkerPath          = "_codex2api_auto_fast"
	quotaPriorityUnsupportedCacheTTL = 30 * time.Minute
	quotaPriorityResponsePeekLimit   = 256 * 1024
)

var (
	quotaPriorityModelBlacklist = map[string]struct{}{
		"gpt-5.3-codex-spark": {},
	}
	quotaPriorityUnsupportedModels sync.Map // normalized model -> expiry time.Time
)

func applyQuotaPriorityServiceTier(account *auth.Account, body []byte, maxAge time.Duration) []byte {
	// Never allow a client-provided internal marker to reach the upstream.
	body, _ = sjson.DeleteBytes(body, quotaPriorityMarkerPath)
	if !shouldApplyQuotaPriorityServiceTier(account, body, maxAge, time.Now()) {
		return body
	}

	model := quotaPriorityModelKey(gjson.GetBytes(body, "model").String())
	originalTier := gjson.GetBytes(body, "service_tier")
	originalCamelTier := gjson.GetBytes(body, "serviceTier")
	updated, err := sjson.SetBytes(body, quotaPriorityMarkerPath+".applied", true)
	if err != nil {
		return body
	}
	updated, _ = sjson.SetBytes(updated, quotaPriorityMarkerPath+".model", model)
	updated, _ = sjson.SetBytes(updated, quotaPriorityMarkerPath+".had_service_tier", originalTier.Exists())
	updated, _ = sjson.SetBytes(updated, quotaPriorityMarkerPath+".had_serviceTier", originalCamelTier.Exists())
	if originalTier.Exists() {
		updated, _ = sjson.SetBytes(updated, quotaPriorityMarkerPath+".service_tier", originalTier.String())
	}
	if originalCamelTier.Exists() {
		updated, _ = sjson.SetBytes(updated, quotaPriorityMarkerPath+".serviceTier", originalCamelTier.String())
	}
	updated, err = sjson.SetBytes(updated, "service_tier", "priority")
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
	model := gjson.GetBytes(body, "model").String()
	if !supportsQuotaPriorityServiceTier(model, now) {
		return false
	}
	if settings.CodexPriorityMinRemainingRatio == 0 {
		return true
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

func quotaPriorityModelKey(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if base, stripped := stripCompactModelSuffix(model); stripped {
		model = base
	}
	if open := strings.IndexByte(model, '('); open > 0 && strings.HasSuffix(model, ")") {
		model = strings.TrimSpace(model[:open])
	}
	return model
}

func supportsQuotaPriorityServiceTier(model string, now time.Time) bool {
	model = quotaPriorityModelKey(model)
	if model == "" || isMediaOnlyModel(model) {
		return false
	}
	if _, blocked := quotaPriorityModelBlacklist[model]; blocked {
		return false
	}
	return !quotaPriorityModelTemporarilyUnsupported(model, now)
}

func quotaPriorityModelTemporarilyUnsupported(model string, now time.Time) bool {
	model = quotaPriorityModelKey(model)
	value, ok := quotaPriorityUnsupportedModels.Load(model)
	if !ok {
		return false
	}
	expiresAt, ok := value.(time.Time)
	if !ok || !now.Before(expiresAt) {
		quotaPriorityUnsupportedModels.Delete(model)
		return false
	}
	return true
}

func markQuotaPriorityModelUnsupported(model string, now time.Time) {
	model = quotaPriorityModelKey(model)
	if model != "" {
		quotaPriorityUnsupportedModels.Store(model, now.Add(quotaPriorityUnsupportedCacheTTL))
	}
}

func consumeQuotaPriorityServiceTierMarker(body []byte) (cleaned []byte, fallback []byte, model string, ok bool) {
	marker := gjson.GetBytes(body, quotaPriorityMarkerPath)
	cleaned, _ = sjson.DeleteBytes(body, quotaPriorityMarkerPath)
	if !marker.Get("applied").Bool() {
		return cleaned, nil, "", false
	}

	fallback = append([]byte(nil), cleaned...)
	if marker.Get("had_service_tier").Bool() {
		fallback, _ = sjson.SetBytes(fallback, "service_tier", marker.Get("service_tier").String())
	} else {
		fallback, _ = sjson.DeleteBytes(fallback, "service_tier")
	}
	if marker.Get("had_serviceTier").Bool() {
		fallback, _ = sjson.SetBytes(fallback, "serviceTier", marker.Get("serviceTier").String())
	} else {
		fallback, _ = sjson.DeleteBytes(fallback, "serviceTier")
	}
	model = quotaPriorityModelKey(marker.Get("model").String())
	if model == "" {
		model = quotaPriorityModelKey(gjson.GetBytes(cleaned, "model").String())
	}
	return cleaned, fallback, model, true
}

func quotaPriorityFallbackOwned(primaryBody, fallbackBody []byte, headers http.Header, identity *PayloadRuleIdentity) bool {
	primaryTier := extractServiceTier(primaryBody)
	fallbackTier := extractServiceTier(fallbackBody)
	if !responsesBodyRequestsImageGeneration(primaryBody) {
		primaryTier = EffectiveRequestedServiceTier(primaryBody, gjson.GetBytes(primaryBody, "model").String(), headers, identity)
		fallbackTier = EffectiveRequestedServiceTier(fallbackBody, gjson.GetBytes(fallbackBody, "model").String(), headers, identity)
	}
	return normalizeBillingServiceTier(primaryTier) == "priority" && normalizeBillingServiceTier(fallbackTier) != "priority"
}

func isQuotaPriorityUnsupportedResponse(statusCode int, payload []byte) bool {
	if statusCode == http.StatusOK {
		if nested := int(gjson.GetBytes(payload, "response.status_code").Int()); nested != 0 {
			statusCode = nested
		} else if nested := int(gjson.GetBytes(payload, "status_code").Int()); nested != 0 {
			statusCode = nested
		}
	}
	body := responseFailedErrorBody(payload)
	code := strings.ToLower(strings.TrimSpace(firstGJSONString(body,
		"error.code", "response.error.code", "code")))
	typeName := strings.ToLower(strings.TrimSpace(firstGJSONString(body,
		"error.type", "response.error.type", "type")))
	parameter := strings.ToLower(strings.TrimSpace(firstGJSONString(body,
		"error.param", "error.parameter", "response.error.param", "param", "parameter")))
	message := strings.ToLower(strings.TrimSpace(firstGJSONString(body,
		"error.message", "response.error.message", "message")))
	mentionsTier := strings.Contains(parameter, "service_tier") ||
		strings.Contains(message, "service_tier") || strings.Contains(message, "service tier")
	if !mentionsTier {
		return false
	}
	knownParameterError := false
	for _, marker := range []string{code, typeName} {
		switch marker {
		case "unsupported_parameter", "unsupported_value", "unknown_parameter", "invalid_parameter", "invalid_value", "invalid_request_error", "bad_request":
			knownParameterError = true
		}
	}
	if statusCode != http.StatusBadRequest && statusCode != http.StatusUnprocessableEntity && !(statusCode == http.StatusOK && knownParameterError) {
		return false
	}
	return knownParameterError || code == "" && typeName == ""
}

type quotaPriorityReplayReadCloser struct {
	io.Reader
	io.Closer
}

func retryQuotaPriorityUnsupportedResponse(resp *http.Response, err error, model string, retry func() (*http.Response, error)) (*http.Response, error) {
	if err != nil || resp == nil || resp.Body == nil || retry == nil {
		return resp, err
	}

	if resp.StatusCode != http.StatusOK {
		prefix, _ := io.ReadAll(io.LimitReader(resp.Body, quotaPriorityResponsePeekLimit+1))
		if len(prefix) <= quotaPriorityResponsePeekLimit && isQuotaPriorityUnsupportedResponse(resp.StatusCode, prefix) {
			_ = resp.Body.Close()
			markQuotaPriorityModelUnsupported(model, time.Now())
			log.Printf("Auto Fast unsupported by model %s; retrying the same account without the automatic service tier", model)
			return retry()
		}
		resp.Body = quotaPriorityReplayReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), resp.Body), Closer: resp.Body}
		return resp, nil
	}

	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return resp, nil
	}
	prefix, payload, reader, _ := readQuotaPrioritySSEPrefix(resp.Body)
	if payload != nil && isQuotaPriorityUnsupportedResponse(http.StatusOK, payload) {
		_ = resp.Body.Close()
		markQuotaPriorityModelUnsupported(model, time.Now())
		log.Printf("Auto Fast unsupported by model %s; retrying the same account without the automatic service tier", model)
		return retry()
	}
	resp.Body = quotaPriorityReplayReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), reader), Closer: resp.Body}
	return resp, nil
}

func readQuotaPrioritySSEPrefix(body io.Reader) (prefix []byte, failurePayload []byte, reader *bufio.Reader, err error) {
	reader = bufio.NewReader(body)
	var eventName string
	var dataLines []string
	for len(prefix) <= quotaPriorityResponsePeekLimit {
		line, readErr := reader.ReadString('\n')
		prefix = append(prefix, line...)
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		case trimmed == "" && len(dataLines) > 0:
			payload := []byte(strings.Join(dataLines, "\n"))
			eventType := strings.ToLower(strings.TrimSpace(normalizedUpstreamSSEEventType(eventName, payload)))
			if eventType == "error" || eventType == "response.failed" {
				return prefix, payload, reader, readErr
			}
			return prefix, nil, reader, readErr
		}
		if readErr != nil {
			return prefix, nil, reader, readErr
		}
	}
	return prefix, nil, reader, nil
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
