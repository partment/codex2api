package admin

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/google/uuid"
)

const (
	autoResetCreditsScanInterval   = 5 * time.Minute
	autoResetCreditsAccountTimeout = 30 * time.Second
	autoResetCreditsConcurrency    = 4
)

type autoResetCreditsScanStats struct {
	Enabled    bool
	Scanned    int
	Queried    int
	Candidates int
	Consumed   int
	Failed     int
}

type autoResetCreditsConfig struct {
	ExpiryEnabled     bool
	BeforeExpiryMin   int
	LowBalanceEnabled bool
	UsageMaxAge       time.Duration
}

func (c autoResetCreditsConfig) anyEnabled() bool {
	return c.ExpiryEnabled || c.LowBalanceEnabled
}

// StartAutoResetCredits 启动主动重置次数的后台自动消费扫描。设置默认关闭；开启或修改
// 自动消费设置时，UpdateSettings 会唤醒本循环立即扫描，之后每 5 分钟扫描一次。
func (h *Handler) StartAutoResetCredits(ctx context.Context) {
	if h == nil || h.store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.autoResetCreditsStartOnce.Do(func() {
		if h.autoResetCreditsWake == nil {
			h.autoResetCreditsWake = make(chan struct{}, 1)
		}
		h.resetCreditPostMu.Lock()
		if h.resetCreditPostCtx == nil && !h.resetCreditPostClosed {
			h.resetCreditPostCtx, h.resetCreditPostCancel = context.WithCancel(ctx)
		}
		h.resetCreditPostMu.Unlock()
		h.autoResetCreditsWG.Add(1)
		go func() {
			defer h.autoResetCreditsWG.Done()
			ticker := time.NewTicker(autoResetCreditsScanInterval)
			defer ticker.Stop()
			firstScan := true
			for {
				if firstScan {
					firstScan = false
				} else {
					select {
					case <-ctx.Done():
						return
					case <-h.autoResetCreditsWake:
					case <-ticker.C:
					}
				}
				// 合并扫描期间积累的设置唤醒和周期 tick，避免同一时刻连续
				// 扫描两次、对同一账号紧接着消耗两张券。
			drainSignals:
				for {
					select {
					case <-h.autoResetCreditsWake:
					case <-ticker.C:
					default:
						break drainSignals
					}
				}
				if ctx.Err() != nil {
					return
				}

				// 零值表示生产时钟；测试可传固定时间保证候选判断可重复。
				stats := h.runAutoResetCreditsScan(ctx, time.Time{})
				if stats.Enabled {
					log.Printf("[auto-reset-credits] 扫描完成: scanned=%d queried=%d candidates=%d consumed=%d failed=%d",
						stats.Scanned, stats.Queried, stats.Candidates, stats.Consumed, stats.Failed)
				}
			}
		}()
	})
}

// WaitAutoResetCredits 等待后台扫描退出；调用前应先取消传给 Start 的 context。
func (h *Handler) WaitAutoResetCredits() {
	if h != nil {
		h.resetCreditPostMu.Lock()
		h.resetCreditPostClosed = true
		if h.resetCreditPostCancel != nil {
			h.resetCreditPostCancel()
		}
		h.resetCreditPostMu.Unlock()
		h.autoResetCreditsWG.Wait()
		h.resetCreditPostWG.Wait()
	}
}

func (h *Handler) triggerAutoResetCreditsScan() {
	if h == nil || h.autoResetCreditsWake == nil {
		return
	}
	select {
	case h.autoResetCreditsWake <- struct{}{}:
	default:
	}
}

func (h *Handler) runAutoResetCreditsScan(ctx context.Context, now time.Time) autoResetCreditsScanStats {
	settings, settingsErr := h.loadAutoResetCreditsConfig(ctx)
	stats := autoResetCreditsScanStats{Enabled: settings.anyEnabled()}
	if settingsErr != nil {
		stats.Failed = 1
		log.Printf("[auto-reset-credits] 读取系统设置失败，已跳过本轮扫描: %v", settingsErr)
		return stats
	}
	if !settings.anyEnabled() || h == nil || h.store == nil {
		return stats
	}

	accounts := h.store.Accounts()
	sem := make(chan struct{}, autoResetCreditsConcurrency)
	var wg sync.WaitGroup
	var statsMu sync.Mutex

	for _, account := range accounts {
		if account == nil {
			continue
		}
		stats.Scanned++
		if !isAutoResetCreditsPlan(account.GetPlanType()) || strings.TrimSpace(account.GetAccessToken()) == "" {
			continue
		}

		select {
		case <-ctx.Done():
			wg.Wait()
			return stats
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(account *auth.Account) {
			defer wg.Done()
			defer func() { <-sem }()

			accountCtx, cancel := context.WithTimeout(ctx, autoResetCreditsAccountTimeout)
			defer cancel()
			queried, candidate, consumed, err := h.autoResetCreditsForAccount(accountCtx, account, now, settings)

			statsMu.Lock()
			if queried {
				stats.Queried++
			}
			if candidate {
				stats.Candidates++
			}
			if consumed {
				stats.Consumed++
			}
			if err != nil {
				stats.Failed++
			}
			statsMu.Unlock()
			if err != nil {
				log.Printf("[auto-reset-credits] 账号 %d 处理失败: %v", account.DBID, err)
			}
		}(account)
	}
	wg.Wait()
	return stats
}

func (h *Handler) autoResetCreditsForAccount(ctx context.Context, account *auth.Account, now time.Time, settings autoResetCreditsConfig) (queried, candidate, consumed bool, err error) {
	lock := h.resetCreditLock(account)
	lock.Lock()
	defer lock.Unlock()
	coolingDown, cooldownErr := h.resetCreditCooldownActive(ctx, account)
	if cooldownErr != nil {
		return false, false, false, fmt.Errorf("check reset-credit cooldown: %w", cooldownErr)
	}
	if coolingDown {
		return false, false, false, nil
	}
	if !settings.anyEnabled() || !isAutoResetCreditsPlan(account.GetPlanType()) || strings.TrimSpace(account.GetAccessToken()) == "" {
		return false, false, false, nil
	}
	decisionNow := autoResetCreditsDecisionTime(now)
	lowBalance := autoResetLowBalanceDecision{}
	var lowBalanceErr error
	if settings.LowBalanceEnabled {
		lowBalance, lowBalanceErr = h.autoResetLowBalanceDecision(ctx, account, decisionNow, settings.UsageMaxAge)
		if lowBalanceErr != nil && !settings.ExpiryEnabled {
			return false, false, false, fmt.Errorf("check low-balance reset episode: %w", lowBalanceErr)
		}
	}
	if !settings.ExpiryEnabled && !lowBalance.Eligible {
		return false, false, false, nil
	}

	list, err := h.queryResetCreditsWithRefresh(ctx, account)
	if err != nil {
		return true, false, false, err
	}
	credits := list.AvailableCodexCredits()
	available := list.AvailableCount
	if available == 0 && len(credits) > 0 {
		available = len(credits)
	}
	account.SetRateLimitResetCredits(available)
	if available <= 0 {
		return true, false, false, nil
	}

	if !isAutoResetCreditsPlan(account.GetPlanType()) || strings.TrimSpace(account.GetAccessToken()) == "" {
		return true, false, false, nil
	}
	credit, expiresAt, reason, ok := selectAutoResetCredit(credits, decisionNow, settings, lowBalance.Eligible)
	if !ok {
		if lowBalanceErr != nil {
			return true, false, false, fmt.Errorf("check low-balance reset episode: %w", lowBalanceErr)
		}
		return true, false, false, nil
	}
	candidate = true

	// 真正消费前再次读取完整配置：管理员在查询/排队期间关闭功能或缩短
	// 提前窗口后，旧扫描不能继续按旧阈值执行不可逆消费。
	settings, settingsErr := h.loadAutoResetCreditsConfig(ctx)
	if settingsErr != nil {
		return true, true, false, fmt.Errorf("reload system settings before consume: %w", settingsErr)
	}
	if !settings.anyEnabled() {
		return true, true, false, nil
	}
	decisionNow = autoResetCreditsDecisionTime(now)
	lowBalance = autoResetLowBalanceDecision{}
	lowBalanceErr = nil
	if settings.LowBalanceEnabled {
		lowBalance, lowBalanceErr = h.autoResetLowBalanceDecision(ctx, account, decisionNow, settings.UsageMaxAge)
		if lowBalanceErr != nil && !settings.ExpiryEnabled {
			return true, true, false, fmt.Errorf("recheck low-balance reset episode: %w", lowBalanceErr)
		}
	}
	credit, expiresAt, reason, ok = selectAutoResetCredit(credits, decisionNow, settings, lowBalance.Eligible)
	if !ok {
		if lowBalanceErr != nil {
			return true, true, false, fmt.Errorf("recheck low-balance reset episode: %w", lowBalanceErr)
		}
		return true, true, false, nil
	}
	redeemRequestID := stableAutoResetCreditRequestID(account, credit)
	var validate resetCreditConsumeValidator
	if reason == "low_balance" {
		expectedEpisode := lowBalance.Episode
		redeemRequestID = stableAutoResetLowBalanceRequestID(account, expectedEpisode)
		validate = func(validateCtx context.Context) (bool, error) {
			currentSettings, settingsErr := h.loadAutoResetCreditsConfig(validateCtx)
			if settingsErr != nil {
				return false, settingsErr
			}
			if !currentSettings.LowBalanceEnabled {
				return false, nil
			}
			current, decisionErr := h.autoResetLowBalanceDecision(validateCtx, account, autoResetCreditsDecisionTime(now), currentSettings.UsageMaxAge)
			if decisionErr != nil {
				return false, decisionErr
			}
			return current.Eligible && current.Episode == expectedEpisode, nil
		}
	}
	outcome, failure := h.consumeResetCreditLocked(ctx, account, redeemRequestID, "auto", validate)
	if failure != nil {
		return true, true, false, autoResetCreditFailureError(failure)
	}
	if outcome.AlreadyHandled {
		return true, true, false, nil
	}
	if outcome.InProgress {
		return true, true, false, nil
	}
	if outcome.StatePersistenceErr != nil {
		return true, true, true, fmt.Errorf("persist low-balance reset episode: %w", outcome.StatePersistenceErr)
	}
	log.Printf("[auto-reset-credits] 账号 %d 已自动消耗重置额度: reason=%s expires_at=%s windows_reset=%d remaining=%d",
		account.DBID, reason, expiresAt.UTC().Format(time.RFC3339), outcome.WindowsReset, outcome.Remaining)
	return true, true, true, nil
}

func (h *Handler) loadAutoResetCreditsConfig(ctx context.Context) (autoResetCreditsConfig, error) {
	runtimeSettings := proxy.CurrentRuntimeSettings()
	config := autoResetCreditsConfig{
		ExpiryEnabled:     runtimeSettings.AutoResetCreditsEnabled,
		BeforeExpiryMin:   runtimeSettings.AutoResetCreditsBeforeExpiryMin,
		LowBalanceEnabled: runtimeSettings.AutoResetCreditsLowBalanceEnabled,
	}
	if h != nil && h.store != nil {
		config.UsageMaxAge = h.store.GetUsageProbeMaxAge()
	}
	if h == nil || h.db == nil {
		return config, nil
	}
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		return autoResetCreditsConfig{}, err
	}
	if settings == nil {
		return autoResetCreditsConfig{
			BeforeExpiryMin: proxy.DefaultRuntimeSettings().AutoResetCreditsBeforeExpiryMin,
			UsageMaxAge:     config.UsageMaxAge,
		}, nil
	}
	usageMaxAge := config.UsageMaxAge
	if settings.UsageProbeMaxAgeMinutes > 0 {
		usageMaxAge = time.Duration(settings.UsageProbeMaxAgeMinutes) * time.Minute
	}
	return autoResetCreditsConfig{
		ExpiryEnabled:     settings.AutoResetCreditsEnabled,
		BeforeExpiryMin:   settings.AutoResetCreditsBeforeExpiryMin,
		LowBalanceEnabled: settings.AutoResetCreditsLowBalanceEnabled,
		UsageMaxAge:       usageMaxAge,
	}, nil
}

func autoResetCreditsDecisionTime(reference time.Time) time.Time {
	if reference.IsZero() {
		return time.Now()
	}
	return reference
}

func (h *Handler) queryResetCreditsWithRefresh(ctx context.Context, account *auth.Account) (*proxy.WhamResetCreditsList, error) {
	proxyURL := h.store.ResolveProxyForAccount(account)
	list, resp, err := h.queryResetCreditsUpstream(ctx, account, proxyURL)
	status := upstreamResetStatus(resp)
	if status == http.StatusUnauthorized {
		drainResetResponse(resp)
		if refreshErr := h.refreshAccountForReset(ctx, account.DBID); refreshErr != nil {
			return nil, fmt.Errorf("query refresh after 401: %w", refreshErr)
		}
		proxyURL = h.store.ResolveProxyForAccount(account)
		list, resp, err = h.queryResetCreditsUpstream(ctx, account, proxyURL)
		status = upstreamResetStatus(resp)
	}
	if err != nil {
		drainResetResponse(resp)
		if status != 0 {
			return nil, fmt.Errorf("query returned status %d", status)
		}
		return nil, fmt.Errorf("query request: %w", err)
	}
	if list == nil {
		return nil, fmt.Errorf("query returned empty response")
	}
	return list, nil
}

func earliestAutoResetCredit(credits []proxy.WhamResetCreditItem, now time.Time, lead time.Duration) (proxy.WhamResetCreditItem, time.Time, bool) {
	if lead <= 0 {
		return proxy.WhamResetCreditItem{}, time.Time{}, false
	}
	deadline := now.Add(lead)
	var selected proxy.WhamResetCreditItem
	var selectedAt time.Time
	found := false
	for _, credit := range credits {
		raw := credit.EffectiveConsumableUntil()
		expiresAt, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || !expiresAt.After(now) || expiresAt.After(deadline) {
			continue
		}
		if !found || expiresAt.Before(selectedAt) || (expiresAt.Equal(selectedAt) && credit.ID < selected.ID) {
			selected = credit
			selectedAt = expiresAt
			found = true
		}
	}
	return selected, selectedAt, found
}

func earliestAvailableAutoResetCredit(credits []proxy.WhamResetCreditItem, now time.Time) (proxy.WhamResetCreditItem, time.Time, bool) {
	var selected proxy.WhamResetCreditItem
	var selectedAt time.Time
	found := false
	for _, credit := range credits {
		expiresAt, err := time.Parse(time.RFC3339Nano, credit.EffectiveConsumableUntil())
		if err != nil || !expiresAt.After(now) {
			continue
		}
		if !found || expiresAt.Before(selectedAt) || (expiresAt.Equal(selectedAt) && credit.ID < selected.ID) {
			selected = credit
			selectedAt = expiresAt
			found = true
		}
	}
	return selected, selectedAt, found
}

func selectAutoResetCredit(credits []proxy.WhamResetCreditItem, now time.Time, settings autoResetCreditsConfig, lowBalanceEligible bool) (proxy.WhamResetCreditItem, time.Time, string, bool) {
	if settings.LowBalanceEnabled && lowBalanceEligible {
		credit, expiresAt, ok := earliestAvailableAutoResetCredit(credits, now)
		return credit, expiresAt, "low_balance", ok
	}
	if settings.ExpiryEnabled {
		credit, expiresAt, ok := earliestAutoResetCredit(credits, now, time.Duration(settings.BeforeExpiryMin)*time.Minute)
		return credit, expiresAt, "expiring", ok
	}
	return proxy.WhamResetCreditItem{}, time.Time{}, "", false
}

func autoResetCreditsLowBalance(account *auth.Account, now time.Time, maxAge time.Duration) bool {
	return account != nil && usableAutoResetCreditsLowBalance(account.GetUsageSnapshot7d(), now, maxAge)
}

func usableAutoResetCreditsLowBalance(snapshot auth.UsageSnapshot7d, now time.Time, maxAge time.Duration) bool {
	if !snapshot.Valid || snapshot.UpdatedAt.IsZero() || snapshot.Percent < auth.AutoResetLowBalanceThreshold || maxAge <= 0 {
		return false
	}
	if now.After(snapshot.UpdatedAt) && now.Sub(snapshot.UpdatedAt) > maxAge {
		return false
	}
	return snapshot.ResetAt.IsZero() || snapshot.ResetAt.After(now) || !snapshot.UpdatedAt.Before(snapshot.ResetAt)
}

type autoResetLowBalanceDecision struct {
	Eligible bool
	Episode  string
}

func (h *Handler) autoResetLowBalanceDecision(ctx context.Context, account *auth.Account, now time.Time, maxAge time.Duration) (autoResetLowBalanceDecision, error) {
	if account == nil {
		return autoResetLowBalanceDecision{}, nil
	}
	snapshot := account.GetUsageSnapshot7d()
	if !usableAutoResetCreditsLowBalance(snapshot, now, maxAge) {
		return autoResetLowBalanceDecision{}, nil
	}
	state, err := h.loadAutoResetLowBalanceState(ctx, account)
	if err != nil {
		return autoResetLowBalanceDecision{}, err
	}
	if !state.ConsumedAt.IsZero() && !state.RecoveredAt.After(state.ConsumedAt) {
		return autoResetLowBalanceDecision{}, nil
	}
	if !state.RecoveredAt.IsZero() && !snapshot.UpdatedAt.After(state.RecoveredAt) {
		return autoResetLowBalanceDecision{}, nil
	}
	episode := "initial"
	if !state.RecoveredAt.IsZero() {
		episode = state.RecoveredAt.UTC().Format(time.RFC3339Nano)
	}
	return autoResetLowBalanceDecision{Eligible: true, Episode: episode}, nil
}

func stableAutoResetCreditRequestID(account *auth.Account, credit proxy.WhamResetCreditItem) string {
	identity := ""
	if account != nil {
		identity = strings.TrimSpace(account.EffectiveAccountID())
		if identity == "" {
			identity = "db:" + strconv.FormatInt(account.DBID, 10)
		}
	}
	trigger := strings.TrimSpace(credit.ID)
	if trigger == "" {
		trigger = credit.EffectiveConsumableUntil()
	}
	key := "codex2api:auto-reset-credit:" + identity + ":" + trigger
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(key)).String()
}

func stableAutoResetLowBalanceRequestID(account *auth.Account, episode string) string {
	key := "codex2api:auto-reset-credit:" + resetCreditLockKey(account) + ":low_balance:" + strings.TrimSpace(episode)
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(key)).String()
}

func isAutoResetCreditsPlan(plan string) bool {
	switch auth.NormalizePlanType(plan) {
	case "plus", "pro":
		return true
	default:
		return false
	}
}

func autoResetCreditFailureError(failure *resetCreditConsumeFailure) error {
	if failure == nil {
		return nil
	}
	if failure.RefreshErr != nil {
		return fmt.Errorf("consume refresh after 401: %w", failure.RefreshErr)
	}
	if failure.Status != 0 {
		return fmt.Errorf("consume returned status %d", failure.Status)
	}
	if failure.RequestErr != nil {
		return fmt.Errorf("consume request: %w", failure.RequestErr)
	}
	return fmt.Errorf("consume failed")
}
