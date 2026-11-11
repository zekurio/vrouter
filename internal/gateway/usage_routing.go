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

func quotaFresh(a storedAccount, q quotaCache, now time.Time) bool {
	if q.Error != "" {
		return now.Before(q.RetryAt)
	}
	ttl := quotaTTL
	if a.Provider == "claude" {
		ttl = claudeQuotaTTL
	}
	if q.ObservedAt.IsZero() || now.Sub(q.ObservedAt) >= ttl {
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
		switch {
		case w.ResetAt != nil && !now.Before(*w.ResetAt):
			// A previous window cannot authorize an automatic redemption.
			q.Allowed = nil
		case !w.seenAt.IsZero() && w.seenAt.Before(q.ObservedAt) && now.Sub(w.seenAt) >= quotaStaleAfter:
			// Only responses report this window, and none has for a while.
			// The usage read that carried it over did not confirm it.
		default:
			windows = append(windows, w)
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
		if !windowExhausted(w) {
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

func windowExhausted(w QuotaWindow) bool {
	if w.used != nil {
		return *w.used >= 100
	}
	return w.Remaining <= 0
}

// quotaWindowScope returns the key of the routing limit a window enforces, and
// the model family for Claude windows that only limit one family.
func quotaWindowScope(a storedAccount, w QuotaWindow) (string, string) {
	switch w.ID {
	case "five-hour":
		return "five_hour", ""
	case "weekly":
		return "seven_day", ""
	case "monthly", "window-1", "window-2":
		if a.Provider == "codex" {
			return w.ID, ""
		}
	case "opus", "sonnet":
		if a.Provider == "claude" {
			return "seven_day_" + w.ID, w.ID
		}
	case fableWindow:
		if a.Provider == "claude" {
			return claudeFableLimit, fableWindow
		}
	}
	return "", ""
}

func quotaWindowKey(a storedAccount, w QuotaWindow, model string) string {
	key, family := quotaWindowScope(a, w)
	if family != "" && !strings.Contains(strings.ToLower(model), family) {
		return ""
	}
	return key
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

// quotaKnownUsableForAllModels is quotaKnownUsable across every model family.
// A reset attempt may have been made for a different model than the one now
// being routed, so only this can prove the attempt is no longer needed.
func quotaKnownUsableForAllModels(a storedAccount, q quotaCache, now time.Time) bool {
	q = currentQuota(q, now)
	if q.Error != "" {
		return false
	}
	if a.Provider == "codex" && q.Allowed != nil {
		return *q.Allowed
	}
	known := false
	for _, w := range q.Windows {
		if key, _ := quotaWindowScope(a, w); key != "" {
			if windowExhausted(w) {
				return false
			}
			known = true
		}
	}
	return known
}

func (s *server) freshNativeQuota(ctx context.Context, a storedAccount) quotaCache {
	// Let an older probe finish before invalidating it. Otherwise it could
	// repopulate the cache after a successful reset.
	for {
		s.quotaMu.Lock()
		pending := s.quotaPending[a.ID]
		if pending == nil {
			// A forced read must still respect a failed probe's cooldown,
			// especially Retry-After from the provider's usage endpoint.
			if q, ok := s.quotas[a.ID]; ok && q.Error != "" && quotaFresh(a, q, time.Now()) {
				s.quotaMu.Unlock()
				return q
			}
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

// confirmedQuota is the quota a reset is spent on: a report no older than
// resetProbeAge, or a new read.
func (s *server) confirmedQuota(ctx context.Context, a storedAccount) quotaCache {
	s.quotaMu.Lock()
	q, ok := s.quotas[a.ID]
	s.quotaMu.Unlock()
	if now := time.Now(); ok && q.Error == "" && now.Sub(q.ObservedAt) < resetProbeAge && quotaFresh(a, q, now) {
		return q
	}
	return s.freshNativeQuota(ctx, a)
}

// poolQuotas reads the quota of every subscription account in a pool with
// read, a few at a time.
func (s *server) poolQuotas(ctx context.Context, pool []storedAccount, read func(context.Context, storedAccount) quotaCache) []quotaCache {
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
			quotas[i] = read(ctx, a)
		}(i, a)
	}
	wg.Wait()
	return quotas
}

func (s *server) usableAccounts(ctx context.Context, pool []storedAccount, model string) []storedAccount {
	quotas := s.poolQuotas(ctx, pool, s.routingQuota)
	usable := make([]storedAccount, 0, len(pool))
	for i, a := range pool {
		if len(quotaBlockers(a, quotas[i], model, time.Now())) == 0 {
			usable = append(usable, a)
			// A delayed reset can first become visible on a later normal request.
			// Only a probe newer than the attempt can reconcile it. A reset the
			// user asked for works before any limit is reached, so usable quota
			// does not settle it and it keeps its request ID.
			if quotaKnownUsableForAllModels(a, quotas[i], time.Now()) {
				s.resetMu.Lock()
				if attempt, ok := s.store.snapshot().ResetAttempts[a.ID]; ok && !attempt.held(time.Now()) && quotas[i].ObservedAt.After(attempt.LastTry) {
					_ = s.saveResetAttempt(a.ID, nil)
				}
				s.resetMu.Unlock()
			}
		}
	}
	return usable
}
