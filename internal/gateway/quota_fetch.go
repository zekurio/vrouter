package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	// Claude's usage endpoint rate limits a token that asks every minute, so
	// it is read rarely. Inference responses keep the main windows current in
	// between.
	claudeQuotaTTL = 5 * time.Minute
	quotaTTL       = time.Minute
	// quotaRefusedRetry is the wait after the usage endpoint rate limits or
	// refuses a read, as Claude Code waits. quotaRetryMax bounds a longer
	// Retry-After.
	quotaRefusedRetry = 5 * time.Minute
	quotaRetryMax     = time.Hour
	// quotaStaleAfter is how long the last good report stands in for a
	// failed read. It covers one missed read and its retry.
	quotaStaleAfter = 15 * time.Minute
	// claudePlanTTL is how long a Claude plan is trusted before the profile
	// is read again.
	claudePlanTTL = time.Hour
)

// Retry only the read, never a usage-reset redemption. A token the provider
// refuses before it expires is refreshed once.
func (s *server) providerUsageJSON(ctx context.Context, a storedAccount, target string, out any) error {
	current, err := s.accessAccount(ctx, a.ID)
	if err != nil {
		return fmt.Errorf("account credentials unavailable: %w", err)
	}
	refreshed, retried := false, false
	for {
		err = s.readProviderJSON(ctx, current, target, out)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if !refreshed {
			if next, ok := s.refreshRefused(ctx, current, err); ok {
				refreshed, current = true, next
				continue
			}
		}
		delay, retry := usageRetryDelay(err)
		if retried || !retry {
			return err
		}
		retried = true
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// refreshRefused answers a 401 with a refreshed token. ok is false for any
// other error, and when the account has no newer token to offer.
func (s *server) refreshRefused(ctx context.Context, current storedAccount, err error) (storedAccount, bool) {
	httpError, isHTTP := errors.AsType[*providerError](err)
	if !isHTTP || httpError.Status != http.StatusUnauthorized || !s.rejectToken(current) {
		return current, false
	}
	next, accessErr := s.accessAccount(ctx, current.ID)
	return next, accessErr == nil && next.AccessToken != current.AccessToken
}

// usageRetryDelay says whether a failed usage read is worth one more try, and
// how long to wait first. Long cooldowns belong in the cache, not in a
// waiting dashboard request.
func usageRetryDelay(err error) (time.Duration, bool) {
	retry := errors.Is(err, errProviderUnavailable) || errors.Is(err, errProviderTimeout)
	delay := 250 * time.Millisecond
	if httpError, ok := errors.AsType[*providerError](err); ok {
		retry = httpError.Status == 502 || httpError.Status == 503 || httpError.Status == 504
		delay = max(delay, time.Until(httpError.RetryAt))
	}
	return delay, retry && delay <= time.Second
}

// rejectToken marks an access token the provider refused, so the next use
// refreshes it. It reports whether the account can refresh.
func (s *server) rejectToken(a storedAccount) bool {
	if a.AuthMode == "api_key" || a.RefreshToken == "" {
		return false
	}
	s.troubleMu.Lock()
	s.rejected[a.ID] = a.ExpiresAt
	s.troubleMu.Unlock()
	return true
}

func quotaFailure(err error, now time.Time) (string, time.Time) {
	retryAt := now.Add(30 * time.Second)
	if httpError, ok := errors.AsType[*providerError](err); ok {
		if httpError.Status == 429 || httpError.Status == 401 || httpError.Status == 403 {
			retryAt = now.Add(quotaRefusedRetry)
		}
		if httpError.RetryAt.After(retryAt) {
			retryAt = httpError.RetryAt
		}
		if limit := now.Add(quotaRetryMax); retryAt.After(limit) {
			retryAt = limit
		}
		switch httpError.Status {
		case 429:
			return "The provider is rate limiting usage requests (HTTP 429).", retryAt
		case 401:
			return "Provider rejected this account's credentials (HTTP 401). Reconnect this account if it persists.", retryAt
		case 403:
			return "Provider denied access to usage (HTTP 403).", retryAt
		default:
			return fmt.Sprintf("Provider usage unavailable (HTTP %d).", httpError.Status), retryAt
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errProviderTimeout) {
		return "Provider usage request timed out.", retryAt
	}
	if errors.Is(err, context.Canceled) {
		return "Provider usage request was canceled.", retryAt
	}
	return "Provider usage unavailable. Refresh to retry.", retryAt
}

// observeQuotaHeaders folds the usage windows of a Claude inference response
// into the account's cached quota, so routing and the dashboard see a limit
// as soon as the provider reports it.
func (s *server) observeQuotaHeaders(ctx context.Context, a storedAccount, resp *http.Response) {
	if a.Provider != "claude" || a.AuthMode != "oauth" || resp == nil {
		return
	}
	now := time.Now().UTC()
	windows := claudeHeaderWindows(resp.Header, resp.StatusCode, now)
	if len(windows) == 0 {
		return
	}
	s.quotaMu.Lock()
	if q, ok := s.quotas[a.ID]; ok && q.Error == "" {
		s.quotas[a.ID] = q.merged(windows, now)
	}
	last := s.lastQuotas[a.ID].merged(windows, now)
	// A busy account answers many times a second. One point a minute is
	// enough for the history.
	record := now.Sub(last.recordedAt) >= time.Minute
	if record {
		last.recordedAt = now
	}
	s.lastQuotas[a.ID] = last
	s.quotaMu.Unlock()
	if record {
		s.recordQuota(ctx, a.ID, quotaCache{Windows: windows, ObservedAt: now})
	}
}

// forgetQuota drops what vrouter read about an account's usage.
func (s *server) forgetQuota(id string) {
	s.quotaMu.Lock()
	delete(s.quotas, id)
	delete(s.lastQuotas, id)
	delete(s.planChecked, id)
	s.quotaMu.Unlock()
}

// knownQuota returns an account's last good report, with the windows that
// have not reset since. ok is false when none is left.
func (s *server) knownQuota(id string, now time.Time) (quotaCache, bool) {
	s.quotaMu.Lock()
	last, ok := s.lastQuotas[id]
	s.quotaMu.Unlock()
	if !ok {
		return quotaCache{}, false
	}
	last = currentQuota(last, now)
	return last, len(last.Windows) > 0
}

// routingQuota is the quota routing decides on. When a read fails, what was
// confirmed lately stands in: an exhausted window stays exhausted until it
// resets, and a usable one costs at most a request the next account takes
// over.
func (s *server) routingQuota(ctx context.Context, a storedAccount) quotaCache {
	q := s.nativeQuota(ctx, a)
	if q.Error == "" {
		return q
	}
	now := time.Now()
	if last, ok := s.knownQuota(a.ID, now); ok {
		if last = last.recent(now); len(last.Windows) > 0 {
			return last
		}
	}
	return q
}

// forgetHeaderWindows drops what inference responses said about an account's
// windows before a reset was redeemed, so the usage read that follows stands
// alone.
func (s *server) forgetHeaderWindows(id string) {
	s.quotaMu.Lock()
	if last, ok := s.lastQuotas[id]; ok {
		s.lastQuotas[id] = last.withoutHeaderWindows()
	}
	s.quotaMu.Unlock()
}
