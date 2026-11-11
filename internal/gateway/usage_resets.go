package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"time"
)

// Contracts verified against openai/codex 0b863c69f50335acd92164aab971cb58d298c2fe
// backend-client/src/client/rate_limit_resets.rs and Claude Code 2.1.322.
// These are native subscription protocols, not paid credit purchase endpoints.
// Claude answers a client version that predates a kind of reset with
// eligible:false, so this follows the release the contract was read from.
const claudeResetUserAgent = "claude-cli/2.1.322 (external, cli)"

// claudeFableLimit is Claude's name for the weekly allowance of the Fable
// models, the ones a plan includes before usage credits apply.
const claudeFableLimit = "seven_day_overage_included"

// The kinds of Claude reset, in the order Claude Code offers them. A session
// reset refills the 5-hour limit, a weekly reset the weekly limits, and a
// full reset both. A Fable reset gives back the weekly usage the Fable models
// spent and refills the 5-hour limit.
const (
	resetSession = "session"
	resetWeekly  = "weekly"
	resetFable   = "fable"
	resetFull    = "full"
	// resetCredit is a Codex reset credit.
	resetCredit = "credit"
)

var resetKinds = []string{resetSession, resetWeekly, resetFable, resetFull} //nolint:gochecknoglobals // fixed order

type resetGrant struct {
	ID       string     `json:"id"`
	Label    string     `json:"label"`
	Left     int        `json:"resets_left"`
	Starts   *time.Time `json:"starts_at"`
	Ends     *time.Time `json:"ends_at"`
	Clears   limitKeys  `json:"clears"`
	Blocking limitKeys  `json:"blocking"`
	Paused   bool       `json:"paused"`
	Usable   bool       `json:"usable_now"`
	// NeedsLimit is unset or true for a grant that only works at a limit.
	NeedsLimit *bool  `json:"use_requires_limit"`
	Type       string `json:"type"`
	// GivesBackWeekly marks a Fable reset.
	GivesBackWeekly bool `json:"gives_back_weekly"`
	// Unavailable is Claude's own reason the grant cannot be used now.
	Unavailable string `json:"unavailable_reason"`
}

// kind names what the grant refills, as Claude Code derives it.
func (g resetGrant) kind() string {
	if g.GivesBackWeekly {
		return resetFable
	}
	if g.Type == resetSession || g.Type == resetWeekly || g.Type == resetFull {
		return g.Type
	}
	session := slices.Contains(g.Clears, "five_hour")
	other := slices.ContainsFunc(g.Clears, func(key string) bool { return key != "five_hour" })
	switch {
	case session && !other:
		return resetSession
	case other && !session:
		return resetWeekly
	}
	return resetFull
}

type resetStatus struct {
	Available int
	Eligible  bool
	Next      string
	Cooldown  *time.Time
	Grants    []resetGrant
	// AtLimit and Exhausted are the limits Claude says the account has
	// reached.
	AtLimit   bool
	Exhausted []string
}

// limitKeys is a list of limit names. Like Claude Code it skips entries that
// are not names instead of refusing the whole status.
type limitKeys []string

func (k *limitKeys) UnmarshalJSON(raw []byte) error {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil //nolint:nilerr // a malformed list counts as empty, as in Claude Code
	}
	*k = (*k)[:0]
	for _, entry := range entries {
		var key string
		if json.Unmarshal(entry, &key) == nil && key != "" {
			*k = append(*k, key)
		}
	}
	return nil
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
			Eligible  *bool             `json:"eligible"`
			Next      string            `json:"next_grant_id"`
			Cooldown  *time.Time        `json:"cooldown_until"`
			Grants    []json.RawMessage `json:"grants"`
			AtLimit   bool              `json:"at_limit"`
			Exhausted limitKeys         `json:"exhausted"`
		} `json:"cedar_ember"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Status == nil || data.Status.Eligible == nil || !*data.Status.Eligible || data.Status.Grants == nil {
		return nil
	}
	v := data.Status
	status := &resetStatus{Eligible: *v.Eligible, Next: v.Next, Cooldown: v.Cooldown, AtLimit: v.AtLimit, Exhausted: v.Exhausted}
	now := time.Now()
	for _, rawGrant := range v.Grants {
		var g resetGrant
		if json.Unmarshal(rawGrant, &g) != nil || !claudeGrantID.MatchString(g.ID) || g.Left < 0 {
			continue
		}
		status.Grants = append(status.Grants, g)
		if !g.Paused && (g.Ends == nil || g.Ends.After(now)) {
			status.Available += g.Left
		}
	}
	return status
}

var claudeGrantID = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

// usableAt reports whether Claude offers the grant now and it is inside its
// validity window.
func (g resetGrant) usableAt(now time.Time) bool {
	return !g.Paused && g.Usable && (g.Starts == nil || !now.Before(*g.Starts)) && (g.Ends == nil || g.Ends.After(now))
}

// grantFor picks the grant to spend on an account that blockers stop. Like
// Claude Code it takes the first usable grant of the kind that fits the limit
// best. Unlike Claude Code, the grant must refill every blocker. A Fable reset
// only gives usage back to a spent weekly limit and may leave the account
// stopped, so it is spent on its own for the Fable limit alone.
func (r *resetStatus) grantFor(blockers []string, now time.Time) (string, bool) {
	fableOnly := slices.Contains(blockers, claudeFableLimit) && !slices.ContainsFunc(blockers, func(key string) bool { return key != claudeFableLimit && key != "five_hour" })
	best, bestRank := "", 0
	for _, g := range r.Grants {
		if !g.usableAt(now) || len(g.Blocking) > 0 || slices.ContainsFunc(blockers, func(key string) bool { return !slices.Contains(g.Clears, key) }) {
			continue
		}
		kind := g.kind()
		rank := 2 * (slices.Index(resetKinds, kind) + 1)
		if fableOnly && kind == resetFable {
			rank = 0
		}
		// Of two grants of one kind, the one Claude names goes first.
		if g.ID != r.Next {
			rank++
		}
		if best == "" || rank < bestRank {
			best, bestRank = g.ID, rank
		}
	}
	return best, best != ""
}

// Only an exhausted weekly allowance justifies spending a reset, the Fable
// one included. Its natural reset time determines priority; 5-hour and
// unrelated limits never do.
func (q quotaCache) weeklyResetAt(blockers []string, now time.Time) time.Time {
	var latest time.Time
	for _, w := range q.Windows {
		var key string
		switch w.ID {
		case "weekly":
			key = "seven_day"
		case "opus", "sonnet":
			key = "seven_day_" + w.ID
		case fableWindow:
			key = claudeFableLimit
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
	if len(blockers) == 0 || r == nil || !r.Eligible || (r.Cooldown != nil && now.Before(*r.Cooldown)) {
		return "", false
	}
	if a.Provider == "codex" {
		// Codex selects its next available credit.
		return "", r.Available > 0
	}
	return r.grantFor(blockers, now)
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
	// Manual marks a reset a user asked for on the Accounts page. Only the
	// user finishes it: the automatic path neither replays nor clears it.
	Manual bool `json:"manual,omitempty"`
}

// manualResetHold is how long an unconfirmed reset a user asked for keeps
// its request ID, as Claude Code keeps an unsettled claim.
const manualResetHold = 10 * time.Minute

// held reports whether a reset a user asked for is still unconfirmed and
// keeps its request ID.
func (a resetAttempt) held(now time.Time) bool {
	return a.Manual && !a.Completed && !a.Rejected && now.Sub(a.LastTry) < manualResetHold
}

// resetProbeAge is how old a usage report may be when a reset is spent on
// it.
const resetProbeAge = time.Minute

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
	// The cached reports come first. A pool that stays exhausted with no
	// reset to spend must not ask the usage endpoint for every request it
	// turns away.
	quotas := s.poolQuotas(ctx, pool, s.nativeQuota)
	recovered, spend := s.resetVerdict(pool, quotas, model)
	if !spend {
		return recovered
	}
	// A reset is only spent on reports no older than resetProbeAge.
	quotas = s.poolQuotas(ctx, pool, s.confirmedQuota)
	if recovered, spend = s.resetVerdict(pool, quotas, model); !spend {
		return recovered
	}
	attempts := s.store.snapshot().ResetAttempts
	pendingID, _ := resetPending(pool, attempts)
	for _, i := range resetOrder(pool, quotas, model) {
		a := pool[i]
		if pendingID != "" && a.ID != pendingID {
			continue
		}
		attempt, ok := s.prepareResetAttempt(ctx, a, quotas[i], model, attempts)
		if !ok {
			continue
		}
		if recovered, stop := s.redeemReset(ctx, a, model, attempt); stop {
			return recovered
		}
	}
	return nil
}

// resetVerdict judges a pool by its usage reports. It returns the accounts
// known to be usable again, or spend when every account is blocked and one
// of them could redeem a reset now.
func (s *server) resetVerdict(pool []storedAccount, quotas []quotaCache, model string) ([]storedAccount, bool) {
	if recovered := s.recoverResetPool(pool, quotas, model); len(recovered) > 0 {
		return recovered, false
	}
	now := time.Now()
	for i, a := range pool {
		if len(quotaBlockers(a, quotas[i], model, now)) == 0 {
			return nil, false
		}
	}
	// An unconfirmed attempt owns the pool until it is reconciled. Do not
	// spend another account's credit just because this response was delayed.
	attempts := s.store.snapshot().ResetAttempts
	pendingID, busy := resetPending(pool, attempts)
	if busy {
		return nil, false
	}
	for i, a := range pool {
		if pendingID != "" && a.ID != pendingID {
			continue
		}
		if _, ok := planReset(a, quotas[i], model, attempts, now); ok {
			return nil, true
		}
	}
	return nil, false
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
		// Usable quota says nothing about a reset the user asked for, which
		// works before any limit is reached. It keeps its request ID.
		if attempt, ok := s.store.snapshot().ResetAttempts[a.ID]; ok && !attempt.held(time.Now()) {
			_ = s.saveResetAttempt(a.ID, nil)
		}
	}
	return recovered
}

// resetPending returns the account whose stale attempt must be retried first.
// busy reports a recent or completed attempt that blocks any new reset. A
// reset a user asked for is neither: the fresh usage read shows what it did.
func resetPending(pool []storedAccount, attempts map[string]resetAttempt) (string, bool) {
	for _, a := range pool {
		attempt, ok := attempts[a.ID]
		if !ok || attempt.Rejected || attempt.Manual {
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

// planReset decides, from what is already known, whether an account's
// exhausted weekly allowance can be reset now. It returns the unconfirmed
// automatic attempt to send again, or a new attempt that names only its
// grant. ok is false when the account cannot be reset now.
func planReset(a storedAccount, q quotaCache, model string, attempts map[string]resetAttempt, now time.Time) (resetAttempt, bool) {
	blockers := quotaBlockers(a, q, model, now)
	if q.weeklyResetAt(blockers, now).IsZero() {
		return resetAttempt{}, false
	}
	attempt, exists := attempts[a.ID]
	if attempt.held(now) {
		// The user finishes a reset they asked for. Sending it again here
		// could spend it on a limit it was never meant for.
		return resetAttempt{}, false
	}
	exists = exists && !attempt.Manual
	if exists && (attempt.Completed || now.Sub(attempt.LastTry) < time.Minute) {
		return resetAttempt{}, false
	}
	if exists && !attempt.Rejected {
		// An unconfirmed attempt goes out again under its request ID, but
		// only while its grant still refills every limit in the way. When it
		// does not, the attempt waits and nothing else is spent for it.
		return attempt, q.resetClears(a, attempt.GrantID, blockers)
	}
	grant, ok := q.resetGrant(a, blockers, now)
	return resetAttempt{GrantID: grant}, ok
}

// resetClears reports whether a grant the account holds refills every
// blocker. Codex has no grants and resets what it finds.
func (q quotaCache) resetClears(a storedAccount, grantID string, blockers []string) bool {
	if a.Provider == "codex" {
		return true
	}
	if q.Resets == nil {
		return false
	}
	for _, g := range q.Resets.Grants {
		if g.ID == grantID {
			return !slices.ContainsFunc(blockers, func(key string) bool { return !slices.Contains(g.Clears, key) })
		}
	}
	return false
}

// prepareResetAttempt returns the attempt to redeem for an exhausted weekly
// allowance, reusing an unconfirmed attempt's request ID. ok is false when
// the account cannot be reset now.
func (s *server) prepareResetAttempt(ctx context.Context, a storedAccount, q quotaCache, model string, attempts map[string]resetAttempt) (resetAttempt, bool) {
	attempt, ok := planReset(a, q, model, attempts, time.Now())
	if !ok || attempt.RequestID != "" {
		return attempt, ok
	}
	attempt.RequestID = randomToken()
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
	outcome, detail, err := s.consumeUsageReset(ctx, a, attempt)
	slog.Info("usage reset spent for an exhausted pool", "account", a.ID, "provider", a.Provider, "grant", attempt.GrantID, "outcome", outcome.String(), "detail", detail, "error", err)
	if err == nil && outcome != resetUnconfirmed {
		attempt.Completed = outcome == resetConfirmed
		attempt.Rejected = outcome == resetRejected
		if s.saveResetAttempt(a.ID, &attempt) != nil {
			return nil, true
		}
	}
	// Re-read even on a timeout: the provider may have applied the reset.
	// What responses said about the windows before it no longer holds.
	s.forgetHeaderWindows(a.ID)
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

func (o resetOutcome) String() string {
	switch o {
	case resetConfirmed:
		return "confirmed"
	case resetRejected:
		return "rejected"
	case resetUnconfirmed:
	}
	return "unconfirmed"
}

// resetAuthRefused is the detail of a redemption the provider answered with
// 401 or 403.
const resetAuthRefused = "auth"

// errResetNotSent marks a redemption that failed before it reached the
// provider, so nothing is left to confirm.
var errResetNotSent = errors.New("reset not sent")

// consumeUsageReset redeems one attempt. The detail is the provider's result
// or reason code, for the message of a reset a user asked for.
func (s *server) consumeUsageReset(ctx context.Context, a storedAccount, attempt resetAttempt) (resetOutcome, string, error) {
	req, err := s.usageResetRequest(ctx, a, attempt)
	if err != nil {
		return resetUnconfirmed, "", fmt.Errorf("%w: %w", errResetNotSent, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return resetUnconfirmed, "", errors.New("reset result unconfirmed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resetRejected, resetAuthRefused, nil
	}
	if resp.StatusCode != http.StatusOK {
		return resetUnconfirmed, "", providerHTTPError(resp)
	}
	var result struct {
		Code   string `json:"code"`
		Result string `json:"result"`
		Reason string `json:"reason"`
	}
	if readBoundedJSON(resp.Body, &result) != nil {
		return resetUnconfirmed, "", errors.New("reset result unconfirmed")
	}
	detail := firstNonEmpty(result.Code, result.Result)
	// Claude adds why it refused, such as an expired grant.
	if result.Result == "ineligible" && resetReason.MatchString(result.Reason) {
		detail = result.Reason
	}
	if !resetReason.MatchString(detail) {
		detail = ""
	}
	return usageResetOutcome(a.Provider, result.Code, result.Result), detail, nil
}

// usageResetRequest builds the redemption of one attempt with the account's
// current credentials.
func (s *server) usageResetRequest(ctx context.Context, a storedAccount, attempt resetAttempt) (*http.Request, error) {
	current, err := s.accessAccount(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	if current.Provider != a.Provider || current.AuthMode != a.AuthMode || current.AccountID != a.AccountID || current.Subject != a.Subject {
		return nil, errors.New("account changed")
	}
	target, payload, err := usageResetTarget(a, attempt)
	if err != nil {
		return nil, err
	}
	rawPayload := make(map[string]json.RawMessage, len(payload))
	for key, value := range payload {
		rawPayload[key], _ = json.Marshal(value)
	}
	req, err := requestJSON(ctx, target, rawPayload)
	if err != nil {
		return nil, err
	}
	providerHeaders(req, current)
	return req, nil
}

// resetReason matches the result and reason codes a provider answers with.
var resetReason = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

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
