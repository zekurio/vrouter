package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"time"
)

// Contracts verified against openai/codex 0b863c69f50335acd92164aab971cb58d298c2fe
// backend-client/src/client/rate_limit_resets.rs and Claude Code 2.1.291.
// These are native subscription protocols, not paid credit purchase endpoints.
const claudeResetUserAgent = "claude-cli/2.1.291 (external, cli)"

type resetGrant struct {
	ID       string     `json:"id"`
	Left     int        `json:"resets_left"`
	Starts   *time.Time `json:"starts_at"`
	Ends     *time.Time `json:"ends_at"`
	Clears   []string   `json:"clears"`
	Blocking []string   `json:"blocking"`
	Paused   bool       `json:"paused"`
	Usable   bool       `json:"usable_now"`
}

type resetStatus struct {
	Available int
	Eligible  bool
	Next      string
	Cooldown  *time.Time
	Grants    []resetGrant
}

func parseResetStatus(provider string, raw []byte) (*bool, *resetStatus) {
	if provider == "codex" {
		return parseCodexResetStatus(raw)
	}
	return nil, parseClaudeResetStatus(raw)
}

func parseCodexResetStatus(raw []byte) (*bool, *resetStatus) {
	var data struct {
		Limit struct {
			Allowed *bool `json:"allowed"`
		} `json:"rate_limit"`
		Credits *struct {
			Count *int `json:"available_count"`
		} `json:"rate_limit_reset_credits"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil, nil
	}
	if data.Credits == nil || data.Credits.Count == nil || *data.Credits.Count < 0 {
		return data.Limit.Allowed, nil
	}
	return data.Limit.Allowed, &resetStatus{Available: *data.Credits.Count, Eligible: true}
}

func parseClaudeResetStatus(raw []byte) *resetStatus {
	var data struct {
		Status *struct {
			Eligible *bool             `json:"eligible"`
			Next     string            `json:"next_grant_id"`
			Cooldown *time.Time        `json:"cooldown_until"`
			Grants   []json.RawMessage `json:"grants"`
		} `json:"cedar_ember"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Status == nil || data.Status.Eligible == nil || !*data.Status.Eligible || data.Status.Grants == nil {
		return nil
	}
	v := data.Status
	status := &resetStatus{Eligible: *v.Eligible, Next: v.Next, Cooldown: v.Cooldown}
	now := time.Now()
	nextAvailable := false
	for _, rawGrant := range v.Grants {
		var g resetGrant
		if json.Unmarshal(rawGrant, &g) != nil || !claudeGrantID.MatchString(g.ID) || g.Left < 0 {
			continue
		}
		status.Grants = append(status.Grants, g)
		if g.activeAt(now) {
			status.Available += g.Left
			if g.ID == status.Next {
				nextAvailable = true
			}
		}
	}
	if !nextAvailable {
		status.Available = 0
	}
	return status
}

var claudeGrantID = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

// activeAt reports whether the grant is enabled and inside its validity window.
func (g resetGrant) activeAt(now time.Time) bool {
	return !g.Paused && g.Usable && (g.Starts == nil || !now.Before(*g.Starts)) && (g.Ends == nil || g.Ends.After(now))
}

// Only an exhausted weekly allowance justifies spending a reset. Its natural
// reset time determines priority; 5-hour and unrelated limits never do.
func (q quotaCache) weeklyResetAt(blockers []string, now time.Time) time.Time {
	var latest time.Time
	for _, w := range q.Windows {
		var key string
		switch w.ID {
		case "weekly":
			key = "seven_day"
		case "opus", "sonnet":
			key = "seven_day_" + w.ID
		default:
			continue
		}
		if !slices.Contains(blockers, key) {
			continue
		}
		// Without a current boundary we cannot rank this account reliably.
		if w.ResetAt == nil || !w.ResetAt.After(now) {
			return time.Time{}
		}
		if w.ResetAt.After(latest) {
			latest = *w.ResetAt
		}
	}
	return latest
}

func (q quotaCache) resetGrant(a storedAccount, blockers []string, now time.Time) (string, bool) {
	r := q.Resets
	if len(blockers) == 0 || r == nil || !r.Eligible || r.Available <= 0 || (r.Cooldown != nil && now.Before(*r.Cooldown)) {
		return "", false
	}
	if a.Provider == "codex" {
		return "", true
	} // Codex selects its next available credit.
	for _, g := range r.Grants {
		if g.ID != r.Next || g.Left <= 0 || len(g.Blocking) > 0 || !g.activeAt(now) {
			continue
		}
		for _, key := range blockers {
			if !slices.Contains(g.Clears, key) {
				return "", false
			}
		}
		return g.ID, true
	}
	return "", false
}

// Persist before sending. An ambiguous HTTP result must reuse this request ID,
// including after a process restart. Completed attempts wait for healthy usage
// before another reset can be spent on that account.
type resetAttempt struct {
	RequestID      string    `json:"request_id"`
	GrantID        string    `json:"grant_id,omitempty"`
	OrganizationID string    `json:"organization_id,omitempty"`
	LastTry        time.Time `json:"last_try"`
	Completed      bool      `json:"completed"`
	Rejected       bool      `json:"rejected,omitempty"`
}

func (s *server) saveResetAttempt(id string, attempt *resetAttempt) error {
	return s.store.update(func(d *diskState) error {
		if attempt == nil {
			delete(d.ResetAttempts, id)
			return nil
		}
		if d.ResetAttempts == nil {
			d.ResetAttempts = map[string]resetAttempt{}
		}
		d.ResetAttempts[id] = *attempt
		return nil
	})
}

// This is called only after every usable account has been tried or the usage
// probes say the pool is exhausted. The lock covers rechecking the entire pool,
// selecting a reset and confirming recovery, so competing requests share it.
func (s *server) resetExhaustedPool(ctx context.Context, pool []storedAccount, model string) []storedAccount {
	if len(pool) == 0 || !nativeUsage(pool[0]) {
		return nil
	}
	s.resetMu.Lock()
	defer s.resetMu.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	// Account management may have changed since the request's catalog read.
	pool = currentResetPool(pool, s.store.snapshot().Accounts)
	quotas := s.poolQuotas(ctx, pool, true)
	if recovered := s.recoverResetPool(pool, quotas, model); len(recovered) > 0 {
		return recovered
	}
	for i, a := range pool {
		if len(quotaBlockers(a, quotas[i], model, time.Now())) == 0 {
			return nil
		}
	}
	// An unconfirmed attempt owns the pool until it is reconciled. Do not
	// spend another account's credit just because this response was delayed.
	pendingID, busy := resetPending(pool, s.store.snapshot().ResetAttempts)
	if busy {
		return nil
	}
	for _, i := range resetOrder(pool, quotas, model) {
		a, q := pool[i], quotas[i]
		if pendingID != "" && a.ID != pendingID {
			continue
		}
		attempt, ok := s.prepareResetAttempt(ctx, a, q, model, pendingID)
		if !ok {
			continue
		}
		if recovered, stop := s.redeemReset(ctx, a, model, attempt); stop {
			return recovered
		}
	}
	return nil
}

// currentResetPool returns the saved records of pool accounts that are still
// enabled and bound to the same identity.
func currentResetPool(pool, current []storedAccount) []storedAccount {
	enabled := make([]storedAccount, 0, len(pool))
	for _, a := range pool {
		for _, saved := range current {
			if saved.ID == a.ID && !saved.Disabled && saved.Provider == a.Provider && saved.AuthMode == a.AuthMode && saved.AccountID == a.AccountID && saved.Subject == a.Subject {
				enabled = append(enabled, saved)
				break
			}
		}
	}
	return enabled
}

// recoverResetPool returns the accounts whose fresh usage is known to be
// usable, and clears their reset attempts.
func (s *server) recoverResetPool(pool []storedAccount, quotas []quotaCache, model string) []storedAccount {
	var recovered []storedAccount
	for i, a := range pool {
		// Unknown usage may be tried normally, but cannot justify spending.
		if len(quotaBlockers(a, quotas[i], model, time.Now())) != 0 || !quotaKnownUsable(a, quotas[i], model, time.Now()) {
			continue
		}
		recovered = append(recovered, a)
		if _, ok := s.store.snapshot().ResetAttempts[a.ID]; ok {
			_ = s.saveResetAttempt(a.ID, nil)
		}
	}
	return recovered
}

// resetPending returns the account whose stale attempt must be retried first.
// busy reports a recent or completed attempt that blocks any new reset.
func resetPending(pool []storedAccount, attempts map[string]resetAttempt) (string, bool) {
	for _, a := range pool {
		attempt, ok := attempts[a.ID]
		if !ok || attempt.Rejected {
			continue
		}
		if attempt.Completed || time.Since(attempt.LastTry) < time.Minute {
			return "", true
		}
		return a.ID, false
	}
	return "", false
}

// resetOrder ranks the pool by the latest natural weekly reset first.
func resetOrder(pool []storedAccount, quotas []quotaCache, model string) []int {
	order := make([]int, len(pool))
	deadlines := make([]time.Time, len(pool))
	now := time.Now()
	for i := range order {
		order[i] = i
		deadlines[i] = quotas[i].weeklyResetAt(quotaBlockers(pool[i], quotas[i], model, now), now)
	}
	sort.SliceStable(order, func(i, j int) bool {
		return deadlines[order[i]].After(deadlines[order[j]])
	})
	return order
}

// prepareResetAttempt returns the attempt to redeem for an exhausted weekly
// allowance, reusing an unconfirmed attempt's request ID. ok is false when
// the account cannot be reset now.
func (s *server) prepareResetAttempt(ctx context.Context, a storedAccount, q quotaCache, model, pendingID string) (resetAttempt, bool) {
	now := time.Now()
	blockers := quotaBlockers(a, q, model, now)
	if q.weeklyResetAt(blockers, now).IsZero() {
		return resetAttempt{}, false
	}
	grant, ok := q.resetGrant(a, blockers, now)
	if !ok && pendingID == "" {
		return resetAttempt{}, false
	}
	attempt, exists := s.store.snapshot().ResetAttempts[a.ID]
	if exists && (attempt.Completed || time.Since(attempt.LastTry) < time.Minute) {
		return resetAttempt{}, false
	}
	if attempt.Rejected {
		exists = false
	}
	if exists {
		return attempt, true
	}
	attempt = resetAttempt{RequestID: randomToken(), GrantID: grant}
	if a.Provider == "claude" {
		organization, ok := s.claudeResetOrganization(ctx, a)
		if !ok {
			return resetAttempt{}, false
		}
		attempt.OrganizationID = organization
	}
	return attempt, true
}

// claudeResetOrganization returns the organization that owns a Claude
// account's reset grants, after confirming the profile is the same account.
func (s *server) claudeResetOrganization(ctx context.Context, a storedAccount) (string, bool) {
	var profile struct {
		Account struct {
			UUID string `json:"uuid"`
		} `json:"account"`
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if s.providerJSON(ctx, a, claudeProfileURL, &profile) != nil || profile.Organization.UUID == "" || a.AccountID == "" || profile.Account.UUID != a.AccountID {
		return "", false
	}
	return profile.Organization.UUID, true
}

// redeemReset persists and spends one reset attempt, then rereads usage.
// stop reports that the pool search ends with the returned accounts.
func (s *server) redeemReset(ctx context.Context, a storedAccount, model string, attempt resetAttempt) ([]storedAccount, bool) {
	attempt.LastTry = time.Now().UTC()
	if s.saveResetAttempt(a.ID, &attempt) != nil {
		return nil, true
	}
	outcome, err := s.consumeUsageReset(ctx, a, attempt)
	if err == nil && outcome != resetUnconfirmed {
		attempt.Completed = outcome == resetConfirmed
		attempt.Rejected = outcome == resetRejected
		if s.saveResetAttempt(a.ID, &attempt) != nil {
			return nil, true
		}
	}
	// Re-read even on a timeout: the provider may have applied the reset.
	q := s.freshNativeQuota(ctx, a)
	if quotaKnownUsable(a, q, model, time.Now()) {
		if s.saveResetAttempt(a.ID, nil) != nil {
			return nil, true
		}
		return []storedAccount{a}, true
	}
	// At most one redemption per request; no speculative chain of resets.
	return nil, !attempt.Rejected
}

type resetOutcome int

const (
	resetUnconfirmed resetOutcome = iota
	resetConfirmed
	resetRejected
)

func (s *server) consumeUsageReset(ctx context.Context, a storedAccount, attempt resetAttempt) (resetOutcome, error) {
	current, err := s.accessAccount(ctx, a.ID)
	if err != nil {
		return resetUnconfirmed, err
	}
	if current.Provider != a.Provider || current.AuthMode != a.AuthMode || current.AccountID != a.AccountID || current.Subject != a.Subject {
		return resetUnconfirmed, errors.New("account changed")
	}
	target, payload, err := usageResetTarget(a, attempt)
	if err != nil {
		return resetUnconfirmed, err
	}
	rawPayload := make(map[string]json.RawMessage, len(payload))
	for key, value := range payload {
		rawPayload[key], _ = json.Marshal(value)
	}
	req, err := requestJSON(ctx, target, rawPayload)
	if err != nil {
		return resetUnconfirmed, err
	}
	providerHeaders(req, current)
	resp, err := s.client.Do(req)
	if err != nil {
		return resetUnconfirmed, errors.New("reset result unconfirmed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resetRejected, nil
	}
	if resp.StatusCode != http.StatusOK {
		return resetUnconfirmed, providerHTTPError(resp)
	}
	var result struct {
		Code   string `json:"code"`
		Result string `json:"result"`
	}
	if readBoundedJSON(resp.Body, &result) != nil {
		return resetUnconfirmed, errors.New("reset result unconfirmed")
	}
	return usageResetOutcome(a.Provider, result.Code, result.Result), nil
}

// usageResetTarget returns the provider endpoint and payload that redeem one
// reset attempt.
func usageResetTarget(a storedAccount, attempt resetAttempt) (string, map[string]string, error) {
	if a.Provider != "claude" {
		return "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", map[string]string{"redeem_request_id": attempt.RequestID}, nil
	}
	if attempt.OrganizationID == "" || !claudeGrantID.MatchString(attempt.GrantID) {
		return "", nil, errors.New("reset identity unavailable")
	}
	target := "https://api.anthropic.com/api/organizations/" + url.PathEscape(attempt.OrganizationID) + "/reset_rate_limits"
	return target, map[string]string{"program": "cedar_ember", "grant_id": attempt.GrantID, "request_id": attempt.RequestID}, nil
}

// usageResetOutcome interprets the provider's answer to a redemption.
func usageResetOutcome(provider, code, result string) resetOutcome {
	if provider == "claude" {
		switch result {
		case "reset", "already_used":
			return resetConfirmed
		case "ineligible", "not_limited", "cooldown":
			return resetRejected
		}
		return resetUnconfirmed
	}
	switch code {
	case "reset", "already_redeemed":
		return resetConfirmed
	case "no_credit", "nothing_to_reset":
		return resetRejected
	}
	return resetUnconfirmed
}
