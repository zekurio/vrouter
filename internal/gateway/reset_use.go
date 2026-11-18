package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"
)

// AccountReset is one kind of usage reset an account holds, for the
// dashboard.
type AccountReset struct {
	// ID is the Claude grant to redeem. Codex has one pool of credits and
	// leaves it empty.
	ID string `json:"id"`
	// Kind is session, weekly, fable or full for Claude, and credit for Codex.
	Kind  string `json:"kind"`
	Label string `json:"label,omitempty"`
	Left  int    `json:"left"`
	// ExpiresAt is when an unused reset lapses.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Usable says the provider accepts the reset now. Note says why not.
	Usable bool   `json:"usable"`
	Note   string `json:"note,omitempty"`
}

// view lists the resets an account still holds. Spent and expired grants are
// left out.
func (r *resetStatus) view(provider string, now time.Time) []AccountReset {
	if r == nil || !r.Eligible {
		return nil
	}
	if provider == "codex" {
		if r.Available <= 0 {
			return nil
		}
		return []AccountReset{{Kind: resetCredit, Left: r.Available, Usable: true}}
	}
	cooling := r.Cooldown != nil && now.Before(*r.Cooldown)
	var resets []AccountReset
	for _, g := range r.Grants {
		if (g.Ends != nil && !g.Ends.After(now)) || (g.Left <= 0 && !g.Usable) {
			continue
		}
		reset := AccountReset{ID: g.ID, Kind: g.kind(), Label: boundedText(g.Label, 80), Left: g.Left, ExpiresAt: g.Ends, Usable: g.usableAt(now) && !cooling}
		if reset.Usable {
			// Claude offers a grant it counts as spent when a claim is unsettled.
			reset.Left = max(reset.Left, 1)
		} else {
			reset.Note = r.note(g, now)
		}
		resets = append(resets, reset)
	}
	return resets
}

// note says why a grant cannot be used now, in the order Claude Code checks.
func (r *resetStatus) note(g resetGrant, now time.Time) string {
	reason := g.Unavailable
	if reason == "" {
		reason = r.blockReason(g, now)
	}
	switch reason {
	case "paused":
		return "Claude has paused this reset."
	case "not_started":
		return "Not usable yet."
	case "cooldown":
		return "Usable again in a minute."
	case "blocked_five_hour":
		return "Not usable while the account is at its 5-hour limit."
	case "blocked_seven_day":
		return "Not usable while the account is at its weekly limit."
	case "blocked_opus":
		return "Not usable while the account is at its Opus limit."
	case "blocked_sonnet":
		return "Not usable while the account is at its Sonnet limit."
	case "blocked_other":
		return "Not usable while the account is at another limit it does not refill."
	case "cannot_clear":
		return "Does not refill the limit the account has reached."
	case "needs_limit":
		return "Usable once the account reaches a limit it refills."
	}
	return "Claude does not offer this reset right now."
}

// blockReason derives the reason from the status when Claude gave none.
func (r *resetStatus) blockReason(g resetGrant, now time.Time) string {
	switch {
	case g.Paused:
		return "paused"
	case g.Starts != nil && now.Before(*g.Starts):
		return "not_started"
	case r.Cooldown != nil && now.Before(*r.Cooldown):
		return "cooldown"
	}
	if reason := blockingLimit(g.Blocking); reason != "" {
		return reason
	}
	refills := func(key string) bool {
		return slices.Contains(g.Clears, key) || (key == "seven_day" && g.GivesBackWeekly)
	}
	switch {
	case r.AtLimit && !slices.ContainsFunc(r.Exhausted, refills):
		return "cannot_clear"
	case g.NeedsLimit == nil || *g.NeedsLimit:
		return "needs_limit"
	}
	return "unavailable"
}

// blockingLimit names the first limit that keeps a grant from being used, or
// "" when none does.
func blockingLimit(blocking []string) string {
	for _, key := range blocking {
		switch key {
		case "five_hour":
			return "blocked_five_hour"
		case "seven_day":
			return "blocked_seven_day"
		case "seven_day_opus":
			return "blocked_opus"
		case "seven_day_sonnet":
			return "blocked_sonnet"
		}
	}
	if len(blocking) > 0 {
		return "blocked_other"
	}
	return ""
}

// resetRefusal words a redemption the provider refused.
func resetRefusal(provider, detail string) string {
	switch detail {
	case resetAuthRefused:
		return "The provider rejected this account's credentials. Reconnect the account and try again."
	case "not_limited":
		return "Claude says the account is not at a limit this reset refills."
	case "cooldown":
		return "Claude asks for a minute between resets. Try again shortly."
	case "paused":
		return "Claude has paused this reset."
	case "expired":
		return "This reset has expired."
	case "no_credit":
		return "This account has no reset left."
	case "nothing_to_reset":
		return "ChatGPT says there is nothing to reset on this account."
	case "", "ineligible":
		return providerLabel(provider) + " refused the reset."
	}
	return fmt.Sprintf("%s refused the reset (%s).", providerLabel(provider), detail)
}

// resetRequestTimeout bounds one reset a user asks for. Claude Code waits 25
// seconds for a claim.
const resetRequestTimeout = 40 * time.Second

// useReset spends one usage reset on an account. The
// provider decides whether the reset applies; vrouter only checks that the
// account still holds it.
func (s *server) useReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GrantID string `json:"grantId"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		writeJSON(w, 400, map[string]string{"error": "Choose a reset to use"})
		return
	}
	account, ok := s.resetAccount(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "Account not found"})
		return
	}
	if account.Disabled {
		writeJSON(w, 409, map[string]string{"error": "Enable the account before using a reset."})
		return
	}
	// A reset for an exhausted pool may be in flight for this account.
	if !s.resetMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "vrouter is working on another reset. Try again in a moment."})
		return
	}
	defer s.resetMu.Unlock()
	if message := s.resetOffered(account, body.GrantID, time.Now()); message != "" {
		writeJSON(w, 409, map[string]string{"error": message})
		return
	}
	// A reset cannot be undone, so a closed tab must not cut it short.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), resetRequestTimeout)
	defer cancel()
	status, message := s.spendReset(ctx, account, body.GrantID)
	if status != http.StatusOK {
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	writeJSON(w, 200, map[string]string{"id": account.ID, "status": "reset"})
}

// resetAccount finds a subscription account.
func (s *server) resetAccount(id string) (storedAccount, bool) {
	for _, a := range s.store.snapshot().Accounts {
		if a.ID == id && nativeUsage(a) {
			return a, true
		}
	}
	return storedAccount{}, false
}

// resetOffered checks a requested reset against the status the dashboard
// showed. It returns why the reset cannot be asked for, or "".
func (s *server) resetOffered(a storedAccount, grantID string, now time.Time) string {
	s.quotaMu.Lock()
	status := s.lastQuotas[a.ID].Resets
	s.quotaMu.Unlock()
	const gone = "This account no longer holds that reset. Refresh and try again."
	if status == nil || !status.Eligible {
		return gone
	}
	if a.Provider == "codex" {
		if grantID != "" || status.Available <= 0 {
			return gone
		}
		return ""
	}
	for _, g := range status.Grants {
		if g.ID != grantID {
			continue
		}
		if g.usableAt(now) && (status.Cooldown == nil || !now.Before(*status.Cooldown)) {
			return ""
		}
		return status.note(g, now)
	}
	return gone
}

// spendReset redeems one reset and reads usage again. It returns the HTTP
// status and, unless that is 200, the message for the user. The caller holds
// resetMu.
func (s *server) spendReset(ctx context.Context, a storedAccount, grantID string) (int, string) {
	attempt, status, message := s.requestedResetAttempt(ctx, a, grantID)
	if status != http.StatusOK {
		return status, message
	}
	// An attempt taken over from an earlier try may have reached the provider
	// then, whatever happens to this one.
	first := attempt.LastTry.IsZero()
	attempt.LastTry = time.Now().UTC()
	if s.saveResetAttempt(a.ID, &attempt) != nil {
		return http.StatusInternalServerError, "Could not record the reset, so it was not sent."
	}
	outcome, detail, err := s.consumeUsageReset(ctx, a, attempt)
	slog.Info("usage reset requested", "account", a.ID, "provider", a.Provider, "grant", grantID, "outcome", outcome.String(), "detail", detail, "error", err)
	if outcome != resetUnconfirmed || (first && errors.Is(err, errResetNotSent)) {
		// The answer is final, or nothing was ever sent. Either way no
		// attempt is left to retry or to reconcile.
		if s.saveResetAttempt(a.ID, nil) != nil {
			slog.Warn("settled reset attempt not cleared", "account", a.ID)
		}
	}
	if errors.Is(err, errResetNotSent) {
		return http.StatusBadGateway, "The reset was not sent. Check that the account is still signed in, then try again."
	}
	// Show what the reset changed. The provider's answer stands either way,
	// and what responses said about the windows before it no longer holds.
	s.forgetHeaderWindows(a.ID)
	s.freshNativeQuota(ctx, a)
	switch outcome {
	case resetConfirmed:
		return http.StatusOK, ""
	case resetRejected:
		return http.StatusConflict, resetRefusal(a.Provider, detail)
	case resetUnconfirmed:
	}
	if httpError, ok := errors.AsType[*providerError](err); ok && httpError.Status == http.StatusTooManyRequests {
		return http.StatusBadGateway, providerLabel(a.Provider) + " is rate limiting reset requests. Try again in a few minutes."
	}
	return http.StatusBadGateway, "Could not confirm the reset. Use it again to retry. vrouter repeats the same request, so the reset is not spent twice."
}

// requestedResetAttempt returns the attempt for a reset a user asked for,
// or the status and message that refuse it. An unconfirmed attempt at the
// same reset keeps its request ID, so the provider cannot count it twice.
// Another reset has to wait until that one is settled: its answer may still
// be on the way.
func (s *server) requestedResetAttempt(ctx context.Context, a storedAccount, grantID string) (resetAttempt, int, string) {
	attempt, exists := s.store.snapshot().ResetAttempts[a.ID]
	// An unconfirmed automatic attempt has no time limit of its own.
	unsettled := exists && !attempt.Completed && !attempt.Rejected && (!attempt.Manual || attempt.held(time.Now()))
	if unsettled {
		if attempt.GrantID != grantID {
			return resetAttempt{}, http.StatusConflict, "An earlier reset on this account is not confirmed yet. Use that one again first, or wait ten minutes."
		}
		attempt.Manual = true
		return attempt, http.StatusOK, ""
	}
	attempt = resetAttempt{RequestID: randomToken(), GrantID: grantID, Manual: true}
	if a.Provider == "claude" {
		organization, ok := s.claudeResetOrganization(ctx, a)
		if !ok {
			return resetAttempt{}, http.StatusBadGateway, "Could not confirm which Claude organization this account belongs to. Try again shortly."
		}
		attempt.OrganizationID = organization
	}
	return attempt, http.StatusOK, ""
}
