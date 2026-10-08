package gateway

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

// Percentages are a share of the provider's complete pool, not its remaining
// allowance. Each enabled subscription account contributes one equal share.
// Missing limits are unlimited; an explicit zero disables this window.
type providerPercentQuota struct {
	FiveHour *float64 `json:"fiveHour,omitempty"`
	SevenDay *float64 `json:"sevenDay,omitempty"`
}

type percentCharge struct {
	Provider  string    `json:"provider"`
	AccountID string    `json:"accountId"`
	Window    string    `json:"window"`
	ResetAt   time.Time `json:"resetAt"`
	Percent   float64   `json:"percent"`
}

type percentSummary struct {
	FiveHour  float64 `json:"fiveHour"`
	SevenDay  float64 `json:"sevenDay"`
	Uncertain bool    `json:"uncertain"`
}

func quotaLimit(q providerPercentQuota, window string) *float64 {
	if window == "five-hour" {
		return q.FiveHour
	}
	return q.SevenDay
}
func hasPercentLimit(q providerPercentQuota) bool { return q.FiveHour != nil || q.SevenDay != nil }

func validatePercentQuotas(quotas map[string]providerPercentQuota) error {
	for provider, quota := range quotas {
		if provider != "claude" && provider != "codex" {
			return errors.New("Choose Claude or Codex for a provider quota")
		}
		for _, value := range []*float64{quota.FiveHour, quota.SevenDay} {
			if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 100) {
				return errors.New("Percentage limits must be between 0 and 100, or omitted for no limit")
			}
		}
	}
	return nil
}

func keyPercentUsage(key keyRecord, now time.Time) map[string]percentSummary {
	result := map[string]percentSummary{}
	for _, provider := range []string{"claude", "codex"} {
		summary := percentSummary{Uncertain: key.PercentUncertain[provider]}
		for _, charge := range key.PercentCharges {
			if charge.Provider != provider || !now.Before(charge.ResetAt) {
				continue
			}
			if charge.Window == "five-hour" {
				summary.FiveHour += charge.Percent
			} else {
				summary.SevenDay += charge.Percent
			}
		}
		result[provider] = summary
	}
	return result
}

func percentAdmission(key keyRecord, provider string, now time.Time) string {
	quota := key.ProviderQuotas[provider]
	if !hasPercentLimit(quota) {
		return ""
	}
	usage := keyPercentUsage(key, now)[provider]
	if usage.Uncertain {
		return "Provider percentage usage is incomplete. Review and save this key's provider quotas to acknowledge it."
	}
	if quota.FiveHour != nil && usage.FiveHour >= *quota.FiveHour {
		return "This key's " + provider + " 5-hour percentage allowance is reached."
	}
	if quota.SevenDay != nil && usage.SevenDay >= *quota.SevenDay {
		return "This key's " + provider + " 7-day percentage allowance is reached."
	}
	return ""
}

// Every inference request takes a read lock before checking configuration.
// When any key has percentage limits, requests for that provider take the
// write lock, including uncapped keys and administrative window triggers.
// This prevents two keys from being charged for the same utilization change.
func (s *server) providerGate(provider string) *sync.RWMutex {
	if provider == "claude" {
		return &s.claudeUsageGate
	}
	return &s.codexUsageGate
}
func (s *server) lockPercentUsage(provider string) (func(), bool) {
	gate := s.providerGate(provider)
	if !gate.TryRLock() {
		return nil, false
	}
	if s.manager != nil {
		for _, key := range s.manager.registry.snapshot().Keys {
			if key.GatewayID == s.gatewayID && key.RevokedAt == nil && !key.expired(time.Now()) && hasPercentLimit(key.ProviderQuotas[provider]) {
				gate.RUnlock()
				if !gate.TryLock() {
					return nil, false
				}
				return gate.Unlock, true
			}
		}
	}
	return gate.RUnlock, true
}

type percentMeasurement struct {
	account  storedAccount
	before   quotaCache
	quota    providerPercentQuota
	poolSize int
}

func (s *server) beginPercentMeasurement(ctx context.Context, attempt *inferenceAttempt, account storedAccount) error {
	if attempt == nil || s.manager == nil || attempt.principal.KeyID == "" {
		return nil
	}
	registry := s.manager.registry.snapshot()
	key := findKey(&registry, attempt.principal.KeyID)
	if key == nil || key.RevokedAt != nil {
		return errors.New("API key is no longer active")
	}
	quota := key.ProviderQuotas[account.Provider]
	if !hasPercentLimit(quota) {
		return nil
	}
	if !nativeUsage(account) {
		return errors.New("Percentage quotas require subscription accounts; API-key accounts cannot measure these windows")
	}
	settlePercent(key, attempt.percentCharges, attempt.percentUnknown)
	if message := percentAdmission(*key, account.Provider, time.Now()); message != "" {
		return errors.New(message)
	}
	before := s.freshNativeQuota(ctx, account)
	if before.Error != "" {
		return errors.New("Provider usage is unavailable; percentage-limited requests are paused")
	}
	// A provider without a reported window cannot enforce a limit for it.
	for _, window := range []string{"five-hour", "weekly"} {
		if quotaLimit(quota, window) != nil && !reportedWindow(before, window) {
			return errors.New("Provider does not report the requested usage window")
		}
	}
	count := 0
	for _, candidate := range s.store.snapshot().Accounts {
		if candidate.Provider == account.Provider && !candidate.Disabled && nativeUsage(candidate) {
			count++
		}
	}
	if count == 0 {
		return errors.New("No subscription pool is available")
	}
	attempt.percent = &percentMeasurement{account: account, before: before, quota: quota, poolSize: count}
	return nil
}

func reportedWindow(q quotaCache, id string) bool {
	for _, w := range q.ReportedWindows {
		if w.ID == id {
			return true
		}
	}
	return false
}
func observedWindow(q quotaCache, id string) *QuotaWindow {
	for i := range q.ReportedWindows {
		if q.ReportedWindows[i].ID == id {
			return &q.ReportedWindows[i]
		}
	}
	return nil
}

func percentDelta(before, after quotaCache, id string, now time.Time) (float64, time.Time, bool) {
	b, a := observedWindow(before, id), observedWindow(after, id)
	if before.Error != "" || after.Error != "" || b == nil || a == nil || b.used == nil || a.used == nil || a.ResetAt == nil || !now.Before(*a.ResetAt) {
		return 0, time.Time{}, false
	}
	if b.ResetAt == nil || !before.ObservedAt.Before(*b.ResetAt) {
		// An idle/expired window starts at zero, with the new boundary confirmed
		// by the provider. Prior utilization belongs to the old window.
		if b.ResetAt == nil && *b.used != 0 {
			return 0, time.Time{}, false
		}
		return *a.used, *a.ResetAt, true
	}
	if !sameWindowReset(*a.ResetAt, *b.ResetAt) || *a.used < *b.used {
		return 0, time.Time{}, false
	}
	return *a.used - *b.used, *a.ResetAt, true
}

func (s *server) endPercentMeasurement(attempt *inferenceAttempt) {
	if attempt == nil || attempt.percent == nil {
		return
	}
	measurement := attempt.percent
	attempt.percent = nil
	// Finish accounting even when the downstream browser disconnected.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	after := s.freshNativeQuota(ctx, measurement.account)
	if attempt.dispatchFailed || attempt.percentForwarded && !attempt.usageTrusted() {
		if attempt.percentUnknown == nil {
			attempt.percentUnknown = map[string]bool{}
		}
		attempt.percentUnknown[measurement.account.Provider] = true
	}
	attempt.percentForwarded = false
	for _, window := range []string{"five-hour", "weekly"} {
		if quotaLimit(measurement.quota, window) == nil {
			continue
		}
		delta, reset, known := percentDelta(measurement.before, after, window, time.Now())
		if !known {
			if attempt.percentUnknown == nil {
				attempt.percentUnknown = map[string]bool{}
			}
			attempt.percentUnknown[measurement.account.Provider] = true
			continue
		}
		attempt.percentCharges = append(attempt.percentCharges, percentCharge{Provider: measurement.account.Provider, AccountID: measurement.account.ID, Window: window, ResetAt: reset, Percent: delta / float64(measurement.poolSize)})
	}
}

func settlePercent(key *keyRecord, charges []percentCharge, unknown map[string]bool) {
	live := key.PercentCharges[:0]
	now := time.Now()
	for _, charge := range key.PercentCharges {
		if now.Before(charge.ResetAt) {
			live = append(live, charge)
		}
	}
	key.PercentCharges = live
	for _, charge := range charges {
		merged := false
		for i := range key.PercentCharges {
			old := &key.PercentCharges[i]
			if old.AccountID == charge.AccountID && old.Provider == charge.Provider && old.Window == charge.Window && sameWindowReset(old.ResetAt, charge.ResetAt) {
				old.Percent += charge.Percent
				merged = true
				break
			}
		}
		if !merged {
			key.PercentCharges = append(key.PercentCharges, charge)
		}
	}
	if key.PercentUncertain == nil {
		key.PercentUncertain = map[string]bool{}
	}
	for provider, missing := range unknown {
		if missing {
			key.PercentUncertain[provider] = true
		}
	}
}

func clonePercentKey(out *keyRecord, key keyRecord) {
	out.PercentCharges = append([]percentCharge(nil), key.PercentCharges...)
	out.PercentUncertain = map[string]bool{}
	for provider, uncertain := range key.PercentUncertain {
		out.PercentUncertain[provider] = uncertain
	}
	out.ProviderQuotas = map[string]providerPercentQuota{}
	for provider, quota := range key.ProviderQuotas {
		if quota.FiveHour != nil {
			v := *quota.FiveHour
			quota.FiveHour = &v
		}
		if quota.SevenDay != nil {
			v := *quota.SevenDay
			quota.SevenDay = &v
		}
		out.ProviderQuotas[provider] = quota
	}
}

// Relative reset countdowns can differ by a second between observations.
func sameWindowReset(a, b time.Time) bool {
	difference := a.Sub(b)
	return difference >= -2*time.Second && difference <= 2*time.Second
}
