package gateway

import (
	"context"
	"strings"
	"sync"
	"time"
)

func nativeUsage(a storedAccount) bool {
	return a.Provider == "codex" && a.AuthMode == "codex" || a.Provider == "claude" && a.AuthMode == "oauth"
}

func quotaFresh(q quotaCache, now time.Time) bool {
	if q.ObservedAt.IsZero() || now.Sub(q.ObservedAt) >= time.Minute {
		return false
	}
	for _, w := range q.Windows {
		if w.ResetAt != nil && w.ResetAt.After(q.ObservedAt) && !now.Before(*w.ResetAt) {
			return false
		}
	}
	return true
}

func currentQuota(q quotaCache, now time.Time) quotaCache {
	windows := make([]QuotaWindow, 0, len(q.Windows))
	for _, w := range q.Windows {
		if w.ResetAt == nil || now.Before(*w.ResetAt) {
			windows = append(windows, w)
		} else {
			// A previous window cannot authorize an automatic redemption.
			q.Allowed = nil
		}
	}
	q.Windows = windows
	return q
}

// Only limits whose scope we understand affect routing. Review, Cowork and
// unknown scoped limits are still displayed, but do not disable unrelated work.
func quotaBlockers(a storedAccount, q quotaCache, model string, now time.Time) []string {
	if q.Error != "" {
		return nil
	}
	q = currentQuota(q, now)
	if a.Provider == "codex" && q.Allowed != nil && *q.Allowed {
		return nil // The provider's permission takes precedence over rounded percentages.
	}
	var blocked []string
	for _, w := range q.Windows {
		exhausted := w.Remaining <= 0
		if w.used != nil {
			exhausted = *w.used >= 100
		}
		if !exhausted {
			continue
		}
		key := quotaWindowKey(a, w, model)
		if key != "" {
			blocked = append(blocked, key)
		}
	}
	if a.Provider == "codex" && q.Allowed != nil && !*q.Allowed && len(blocked) == 0 {
		blocked = append(blocked, "codex_rate_limits")
	}
	return blocked
}

func quotaWindowKey(a storedAccount, w QuotaWindow, model string) string {
	switch w.ID {
	case "five-hour":
		return "five_hour"
	case "weekly":
		return "seven_day"
	case "monthly", "window-1", "window-2":
		if a.Provider == "codex" {
			return w.ID
		}
	case "opus", "sonnet":
		if a.Provider == "claude" && strings.Contains(strings.ToLower(model), w.ID) {
			return "seven_day_" + w.ID
		}
	}
	return ""
}

func quotaKnownUsable(a storedAccount, q quotaCache, model string, now time.Time) bool {
	q = currentQuota(q, now)
	if q.Error != "" || len(quotaBlockers(a, q, model, now)) > 0 {
		return false
	}
	if a.Provider == "codex" && q.Allowed != nil {
		return *q.Allowed
	}
	for _, w := range q.Windows {
		if quotaWindowKey(a, w, model) != "" {
			return true
		}
	}
	return false
}

func (s *server) freshNativeQuota(ctx context.Context, a storedAccount) quotaCache {
	// Let an older probe finish before invalidating it. Otherwise it could
	// repopulate the cache after a successful reset.
	for {
		s.quotaMu.Lock()
		pending := s.quotaPending[a.ID]
		if pending == nil {
			delete(s.quotas, a.ID)
		}
		s.quotaMu.Unlock()
		if pending == nil {
			return s.nativeQuota(ctx, a)
		}
		select {
		case <-pending:
		case <-ctx.Done():
			return quotaCache{Error: "Usage request timed out"}
		}
	}
}

func (s *server) poolQuotas(ctx context.Context, pool []storedAccount, fresh bool) []quotaCache {
	quotas := make([]quotaCache, len(pool))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i, a := range pool {
		if !nativeUsage(a) {
			continue
		}
		wg.Add(1)
		go func(i int, a storedAccount) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-limit }()
			if fresh {
				quotas[i] = s.freshNativeQuota(ctx, a)
			} else {
				quotas[i] = s.nativeQuota(ctx, a)
			}
		}(i, a)
	}
	wg.Wait()
	return quotas
}

func (s *server) usableAccounts(ctx context.Context, pool []storedAccount, model string) []storedAccount {
	quotas := s.poolQuotas(ctx, pool, false)
	usable := make([]storedAccount, 0, len(pool))
	for i, a := range pool {
		if len(quotaBlockers(a, quotas[i], model, time.Now())) == 0 {
			usable = append(usable, a)
			// A delayed reset can first become visible on a later normal request.
			// Only a probe newer than the attempt can reconcile it.
			if quotaKnownUsable(a, quotas[i], "opus sonnet", time.Now()) {
				s.resetMu.Lock()
				if attempt, ok := s.store.snapshot().ResetAttempts[a.ID]; ok && quotas[i].ObservedAt.After(attempt.LastTry) {
					_ = s.saveResetAttempt(a.ID, nil)
				}
				s.resetMu.Unlock()
			}
		}
	}
	return usable
}
