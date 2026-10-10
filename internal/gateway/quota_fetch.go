package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Retry only the read, never token rotation or a usage-reset redemption.
func (s *server) providerUsageJSON(ctx context.Context, a storedAccount, target string, out any) error {
	current, err := s.accessAccount(ctx, a.ID)
	if err != nil {
		return fmt.Errorf("account credentials unavailable: %w", err)
	}
	for attempt := 0; ; attempt++ {
		err = s.readProviderJSON(ctx, current, target, out)
		if err == nil || attempt == 1 || ctx.Err() != nil {
			return err
		}
		retry := errors.Is(err, errProviderUnavailable) || errors.Is(err, errProviderTimeout)
		delay := 250 * time.Millisecond
		if httpError, ok := errors.AsType[*providerError](err); ok {
			retry = httpError.Status == 502 || httpError.Status == 503 || httpError.Status == 504
			delay = max(delay, time.Until(httpError.RetryAt))
		}
		// Long cooldowns belong in the cache, not in a waiting dashboard request.
		if !retry || delay > time.Second {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func quotaFailure(err error, now time.Time) (string, time.Time) {
	retryAt := now.Add(10 * time.Second)
	if httpError, ok := errors.AsType[*providerError](err); ok {
		if httpError.Status == 429 || httpError.Status == 401 || httpError.Status == 403 {
			retryAt = now.Add(time.Minute)
		}
		if httpError.RetryAt.After(retryAt) {
			retryAt = httpError.RetryAt
		}
		switch httpError.Status {
		case 429:
			return "Provider usage is rate limited. Retrying after the cooldown.", retryAt
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
