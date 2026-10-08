package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"
)

func (s *server) accounts(ctx context.Context) []Account {
	records := s.store.snapshot().Accounts
	accounts := make([]Account, len(records))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i, a := range records {
		name := a.Label
		if name == "" {
			name = a.Email
		}
		if name == "" {
			name = providerLabel(a.Provider)
		}
		status := "connected"
		note := ""
		if !routableAuth(a) {
			status = "unavailable"
			note = "This sign-in method is no longer supported. Add the account using Codex sign-in."
		} else if a.Disabled {
			status = "disabled"
		} else if a.AccessToken == "" || (!a.ExpiresAt.IsZero() && time.Now().After(a.ExpiresAt) && a.RefreshToken == "") {
			status = "unavailable"
			note = "Sign in again to renew this connection."
		}
		// A native Codex record is only safe to renew in place when it already
		// binds a verified workspace and user subject; a legacy import stays
		// usable but is never rebound to a new user. A Claude OAuth record is
		// only safe to renew in place when a prior identity (account UUID or
		// email) can tell it apart from another Claude account.
		reconnectable := (a.Provider == "codex" && a.AuthMode == "codex" && a.AccountID != "" && a.Subject != "") ||
			(a.Provider == "claude" && a.AuthMode == "oauth" && (a.AccountID != "" || a.Email != ""))
		accounts[i] = Account{ID: a.ID, Name: name, Provider: provider(a.Provider), AuthMode: a.AuthMode, Status: status, StatusMessage: note, Plan: planName(provider(a.Provider), a.Plan), Window: "Allowance not reported", Email: a.Email, CreatedAt: &a.CreatedAt, Manageable: true, Reconnectable: reconnectable}
		if a.Disabled || !routableAuth(a) || a.AuthMode == "api_key" {
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
			q := s.nativeQuota(ctx, a)
			applyAccountQuota(&accounts[i], q)
		}(i, a)
	}
	wg.Wait()
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return accounts
}

func applyAccountQuota(account *Account, q quotaCache) {
	account.Windows = q.Windows
	account.QuotaError = q.Error
	if q.Resets != nil {
		account.AvailableResets = &q.Resets.Available
	}
	if q.Error == "" && !q.ObservedAt.IsZero() {
		account.QuotaUpdatedAt = &q.ObservedAt
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
		account.Reset = resetLabel(primary.ResetAt, time.Now())
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
		writeJSON(w, 400, map[string]string{"error": "Choose an account and its enabled state"})
		return
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	missing := errors.New("account not found")
	err := s.store.update(func(d *diskState) error {
		for i, a := range d.Accounts {
			if a.ID == body.ID {
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
	s.catalogMu.Lock()
	delete(s.catalogs, body.ID)
	s.catalogMu.Unlock()
	s.quotaMu.Lock()
	delete(s.quotas, body.ID)
	s.quotaMu.Unlock()
	if remove {
		writeJSON(w, 200, map[string]string{"id": body.ID, "status": "removed"})
	} else {
		writeJSON(w, 200, map[string]any{"id": body.ID, "enabled": *body.Enabled})
	}
}
func (s *server) nativeQuota(ctx context.Context, a storedAccount) quotaCache {
	s.quotaMu.Lock()
	if q, ok := s.quotas[a.ID]; ok && quotaFresh(q, time.Now()) {
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
	q := quotaCache{ObservedAt: time.Now().UTC()}
	var profilePlan chan string
	if a.Provider == "claude" && a.AuthMode == "oauth" {
		profilePlan = make(chan string, 1)
		go func() { profilePlan <- s.claudePlan(ctx, a) }()
	}
	target := "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
	if a.Provider == "codex" {
		target = "https://chatgpt.com/backend-api/wham/usage"
	}
	var raw json.RawMessage
	err := s.providerJSON(ctx, a, target, &raw)
	if err == nil {
		q.Windows, q.Plan, err = parseQuota(a.Provider, raw, q.ObservedAt)
		q.Allowed, q.Resets = parseResetStatus(a.Provider, raw)
	}
	if err != nil || (len(q.Windows) == 0 && q.Allowed == nil) {
		q.Error = "Provider usage unavailable. Refresh or reconnect this account."
		q.Windows = nil
	}
	q.ReportedWindows = append([]QuotaWindow(nil), q.Windows...)
	q = currentQuota(q, time.Now())
	if profilePlan != nil {
		q.Plan = <-profilePlan
	}
	s.quotaMu.Lock()
	s.quotas[a.ID] = q
	s.quotaMu.Unlock()
	return q
}
