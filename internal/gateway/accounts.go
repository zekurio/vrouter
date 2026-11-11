package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// accounts lists the accounts a user can see: the shared ones and their own.
func (s *server) accounts(ctx context.Context, user userRecord) []Account {
	var records []storedAccount
	for _, a := range s.store.snapshot().Accounts {
		if accountVisible(a, user.ID) {
			records = append(records, a)
		}
	}
	accounts := make([]Account, len(records))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i, a := range records {
		accounts[i] = accountView(a, user)
		if a.Disabled || !routableAuth(a) || a.AuthMode == "api_key" {
			continue
		}
		wg.Add(1)
		go func(i int, a storedAccount) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
			}
			applyAccountQuota(&accounts[i], s.dashboardQuota(ctx, a))
		}(i, a)
	}
	wg.Wait()
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return accounts
}

// accountView is the dashboard row of a saved account before its quota, as
// the user sees it.
func accountView(a storedAccount, user userRecord) Account {
	name := a.Label
	if name == "" {
		name = a.Email
	}
	if name == "" {
		name = providerLabel(a.Provider)
	}
	status, note := accountStatus(a)
	// A native Codex record is only safe to renew in place when it already
	// binds a verified workspace and user subject; an unverified import stays
	// usable but is never rebound to a new user. A Claude OAuth record is
	// only safe to renew in place when a prior identity (account UUID or
	// email) can tell it apart from another Claude account.
	manageable := accountManageable(a, user)
	reconnectable := manageable && (a.Provider == "codex" && a.AuthMode == "codex" && a.AccountID != "" && a.Subject != "") ||
		(a.Provider == "claude" && a.AuthMode == "oauth" && (a.AccountID != "" || a.Email != ""))
	return Account{ID: a.ID, Scope: accountScope(a), Manageable: manageable, Name: name, Provider: provider(a.Provider), AuthMode: a.AuthMode, Status: status, StatusMessage: note, Plan: planName(provider(a.Provider), a.Plan), Window: "Allowance not reported", Email: a.Email, CreatedAt: &a.CreatedAt, Reconnectable: reconnectable}
}

// accountStatus returns an account's connection state and its explanation.
func accountStatus(a storedAccount) (string, string) {
	switch {
	case !routableAuth(a):
		return "unavailable", "This sign-in method is no longer supported. Add the account using Codex sign-in."
	case a.Disabled:
		return "disabled", ""
	case a.AccessToken == "" || (!a.ExpiresAt.IsZero() && time.Now().After(a.ExpiresAt) && a.RefreshToken == ""):
		return "unavailable", "Sign in again to renew this connection."
	}
	return "connected", ""
}

func applyAccountQuota(account *Account, q quotaCache) {
	account.Windows = q.Windows
	account.QuotaError = q.Error
	if q.Error != "" && !q.RetryAt.IsZero() {
		account.QuotaRetryAt = &q.RetryAt
	}
	account.Resets = q.Resets.view(account.Provider, time.Now())
	if updated := q.updatedAt(); len(q.Windows) > 0 || (q.Error == "" && !updated.IsZero()) {
		account.QuotaUpdatedAt = &updated
	}
	if q.Plan != "" {
		account.Plan = planName(account.Provider, q.Plan)
	}
	if len(q.Windows) > 0 {
		primary := q.Windows[0]
		for _, window := range q.Windows {
			if window.ID == "weekly" {
				primary = window
				break
			}
		}
		account.Window = primary.Label
		account.Remaining = &primary.Remaining
	}
}

func (s *server) updateAccount(w http.ResponseWriter, r *http.Request) { s.changeAccount(w, r, false) }

func (s *server) removeAccount(w http.ResponseWriter, r *http.Request) { s.changeAccount(w, r, true) }

func (s *server) changeAccount(w http.ResponseWriter, r *http.Request, remove bool) {
	var body struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || body.ID == "" || (!remove && body.Enabled == nil) {
		message := "Choose an account and its enabled state"
		if remove {
			message = "Choose an account to remove"
		}
		writeJSON(w, 400, map[string]string{"error": message})
		return
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	missing := errors.New("account not found")
	user := currentUser(r)
	err := s.store.update(func(d *diskState) error {
		for i, a := range d.Accounts {
			// An account the user may not manage is reported as missing.
			if a.ID == body.ID && accountManageable(a, user) {
				if remove {
					d.Accounts = append(d.Accounts[:i], d.Accounts[i+1:]...)
					delete(d.ResetAttempts, body.ID)
				} else {
					d.Accounts[i].Disabled = !*body.Enabled
				}
				return nil
			}
		}
		return missing
	})
	if err != nil {
		status := 500
		if errors.Is(err, missing) {
			status = 404
		}
		writeJSON(w, status, map[string]string{"error": "Could not update the saved account"})
		return
	}
	s.forgetAccountState(body.ID)
	s.forgetQuota(body.ID)
	if remove {
		if err := s.store.data.usage.forgetQuota(r.Context(), body.ID); err != nil {
			slog.Warn("quota history of a removed account kept", "account", body.ID, "error", err)
		}
		writeJSON(w, 200, map[string]string{"id": body.ID, "status": "removed"})
	} else {
		writeJSON(w, 200, map[string]any{"id": body.ID, "enabled": *body.Enabled})
	}
}

func (s *server) nativeQuota(ctx context.Context, a storedAccount) quotaCache {
	s.quotaMu.Lock()
	if q, ok := s.quotas[a.ID]; ok && quotaFresh(a, q, time.Now()) {
		s.quotaMu.Unlock()
		return q
	}
	if pending, ok := s.quotaPending[a.ID]; ok {
		s.quotaMu.Unlock()
		select {
		case <-pending:
			return s.nativeQuota(ctx, a)
		case <-ctx.Done():
			return quotaCache{Error: "Usage request timed out"}
		}
	}
	if s.quotaPending == nil {
		s.quotaPending = map[string]chan struct{}{}
	}
	pending := make(chan struct{})
	s.quotaPending[a.ID] = pending
	s.quotaMu.Unlock()
	defer func() { s.quotaMu.Lock(); delete(s.quotaPending, a.ID); close(pending); s.quotaMu.Unlock() }()
	q := s.fetchNativeQuota(ctx, a)
	if q.Error != "" && ctx.Err() != nil {
		// A caller that gave up says nothing about the provider; do not
		// cache its failure for everyone else.
		return q
	}
	s.quotaMu.Lock()
	if q.Error == "" {
		q = q.keepingHeaderWindows(s.lastQuotas[a.ID], time.Now())
		s.lastQuotas[a.ID] = q
	}
	s.quotas[a.ID] = q
	s.quotaMu.Unlock()
	if q.Error == "" {
		s.recordQuota(ctx, a.ID, q)
	}
	return q
}

// fetchNativeQuota reads an account's usage windows, reset status, and plan
// from its provider.
func (s *server) fetchNativeQuota(ctx context.Context, a storedAccount) quotaCache {
	q := quotaCache{ObservedAt: time.Now().UTC()}
	var profilePlan chan string
	if a.Provider == "claude" && a.AuthMode == "oauth" {
		if s.claudePlanDue(a) {
			profilePlan = make(chan string, 1)
			go func() { profilePlan <- s.claudePlan(ctx, a) }()
		} else {
			q.Plan = a.Plan
		}
	}
	target := "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
	if a.Provider == "codex" {
		target = "https://chatgpt.com/backend-api/wham/usage"
	}
	var raw json.RawMessage
	err := s.providerUsageJSON(ctx, a, target, &raw)
	if err == nil {
		q.Windows, q.Plan, err = parseQuota(a.Provider, raw, q.ObservedAt)
		q.Allowed, q.Resets = parseResetStatus(a.Provider, raw)
	}
	// A Claude report without windows is an account that has none to show.
	if err == nil && a.Provider == "codex" && len(q.Windows) == 0 && q.Allowed == nil {
		err = errors.New("provider response contained no supported usage windows")
	}
	if err != nil {
		q.Error, q.RetryAt = quotaFailure(err, time.Now())
		q.Windows = nil
		// Errors contain only local descriptions and HTTP status codes, never
		// response bodies or credentials. Ignore clients leaving the page.
		if !errors.Is(ctx.Err(), context.Canceled) {
			slog.Warn("account quota unavailable", "account", a.ID,
				"provider", a.Provider, "error", err, "elapsed", time.Since(q.ObservedAt), "retry_at", q.RetryAt)
		}
	}
	q.ReportedWindows = append([]QuotaWindow(nil), q.Windows...)
	q = currentQuota(q, time.Now())
	if profilePlan != nil {
		q.Plan = <-profilePlan
	}
	return q
}

// dashboardQuota is the quota shown for an account. When a read fails it
// keeps the last reported windows, so a provider hiccup does not blank the
// page. The error joins them only once they are old: one missed read is not
// worth a warning while the next one is already scheduled.
func (s *server) dashboardQuota(ctx context.Context, a storedAccount) quotaCache {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	q := s.nativeQuota(ctx, a)
	if q.Error == "" {
		return q
	}
	now := time.Now()
	last, ok := s.knownQuota(a.ID, now)
	if !ok {
		return q
	}
	// Responses keep the main windows current, but only a usage read
	// renews the rest and the resets. Windows beside an error tell the UI
	// that some of them are old.
	if now.Sub(last.ObservedAt) >= quotaStaleAfter {
		last.Error, last.RetryAt = q.Error, q.RetryAt
	}
	return last
}
