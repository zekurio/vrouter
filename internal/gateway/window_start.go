package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// How often idle subscription accounts are checked for a 5-hour window.
	windowCheckInterval = time.Minute
	// Wait after a trigger or a failed check before trying that account again.
	// It is also the persisted cooldown, so a restart cannot repeat a trigger.
	windowRetryDelay = 5 * time.Minute
)

// Some plans, such as Codex Pro, have no 5-hour window to start.
var errNoWindow = errors.New("account does not report a 5-hour window")

// Codex Pro plans report a five-hour entry without enforcing one, so the
// usage response cannot be trusted to skip them.
const defaultWindowSkipPlans = "codex:pro*"

// windowPlanSkipped reports whether the list names the account's plan. Entries
// are "plan" or "provider:plan", where a trailing * matches any ending, and are
// compared with the provider's plan value or the name shown in the dashboard.
func windowPlanSkipped(list, accountProvider, plan string) bool {
	plan = strings.ToLower(strings.TrimSpace(plan))
	if plan == "" {
		return false
	}
	if strings.TrimSpace(list) == "" {
		list = defaultWindowSkipPlans
	}
	shown := strings.ToLower(planName(provider(accountProvider), plan))
	for entry := range strings.SplitSeq(list, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if scope, rest, ok := strings.Cut(entry, ":"); ok {
			if provider(scope) != provider(accountProvider) {
				continue
			}
			entry = strings.TrimSpace(rest)
		}
		if prefix, wild := strings.CutSuffix(entry, "*"); wild && prefix != "" && (strings.HasPrefix(plan, prefix) || strings.HasPrefix(shown, prefix)) {
			return true
		}
		if entry != "" && (entry == plan || entry == shown) {
			return true
		}
	}
	return false
}

type windowWatch struct {
	next    time.Time
	lastErr string
}

// Cheap model families are explicit: discovery does not supply prices. Never
// fall back to a large model just because no small model is advertised.
func windowStartModel(models []Model, provider string) (Model, error) {
	rank := func(id string) int {
		id = strings.ToLower(id)
		if provider == "claude" {
			if strings.Contains(id, "haiku") {
				return 1
			}
			return 100
		}
		if strings.Contains(id, "nano") {
			return 1
		}
		if strings.Contains(id, "mini") {
			return 2
		}
		return 100
	}
	candidates := append([]Model(nil), models...)
	sort.Slice(candidates, func(i, j int) bool {
		a, b := rank(candidates[i].ID), rank(candidates[j].ID)
		if a != b {
			return a < b
		}
		return candidates[i].ID > candidates[j].ID
	})
	if len(candidates) == 0 || rank(candidates[0].ID) == 100 {
		return Model{}, errors.New("no supported low-cost model is advertised for this account")
	}
	return candidates[0], nil
}

func lowestReasoning(model Model) string {
	for _, wanted := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		if slices.Contains(model.Reasoning, wanted) {
			return wanted
		}
	}
	return ""
}

func windowStartPayload(model Model, provider string) map[string]any {
	if provider == "claude" {
		return map[string]any{"model": model.ID, "max_tokens": 8, "messages": []map[string]string{{"role": "user", "content": "Reply only OK."}}}
	}
	payload := map[string]any{"model": model.ID, "stream": true, "store": false, "instructions": "Reply with only OK. Do not use tools.", "input": []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": "OK?"}}}}}
	if effort := lowestReasoning(model); effort != "" {
		payload["reasoning"] = map[string]string{"effort": effort}
	}
	return payload
}

// watchWindows keeps a 5-hour window running on every subscription account of
// every gateway. Only the root engine runs it.
func (s *server) watchWindows(ctx context.Context) {
	defer close(s.windowsDone)
	ticker := time.NewTicker(windowCheckInterval)
	defer ticker.Stop()
	for {
		for _, gateway := range s.manager.registry.snapshot().Gateways {
			if engine, err := s.manager.engine(gateway.ID); err == nil {
				engine.startIdleWindows(ctx)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// startIdleWindows checks each due account once. An account with an active
// window is not checked again until that window resets.
func (s *server) startIdleWindows(ctx context.Context) {
	now := time.Now()
	var wg sync.WaitGroup
	for _, account := range s.store.snapshot().Accounts {
		if account.Disabled || !nativeUsage(account) {
			continue
		}
		s.windowMu.Lock()
		due := !now.Before(s.windowWatch[account.ID].next)
		s.windowMu.Unlock()
		if !due {
			continue
		}
		// Idle accounts start together where their providers permit it.
		wg.Add(1)
		go func(account storedAccount) {
			defer wg.Done()
			resetAt, err := s.ensureWindow(ctx, account)
			if ctx.Err() != nil {
				return
			}
			watch := windowWatch{}
			switch {
			case errors.Is(err, errNoWindow):
				// Not a failure; look again later in case the plan changes.
				watch.next, err = time.Now().Add(time.Hour), nil
			case err != nil:
				watch = windowWatch{next: time.Now().Add(windowRetryDelay), lastErr: err.Error()}
			default:
				watch.next = *resetAt
			}
			s.windowMu.Lock()
			repeated := s.windowWatch[account.ID].lastErr == watch.lastErr
			s.windowWatch[account.ID] = watch
			s.windowMu.Unlock()
			if err != nil && !repeated {
				slog.Warn("5-hour window not started", "gateway", s.gatewayID, "account", account.ID, "error", err)
			}
		}(account)
	}
	wg.Wait()
}

// ensureWindow returns the reset time of the account's 5-hour window, sending
// one small request first when no window is active.
func (s *server) ensureWindow(ctx context.Context, account storedAccount) (*time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	account, err := s.accessAccount(ctx, account.ID)
	if err != nil {
		return nil, errors.New("account needs sign-in")
	}
	before := s.freshNativeQuota(ctx, account)
	if reset, err := s.existingWindowReset(account, before); reset != nil || err != nil {
		return reset, err
	}
	model, err := s.windowTriggerModel(ctx, account)
	if err != nil {
		return nil, err
	}
	if len(quotaBlockers(account, before, model.ID, time.Now())) > 0 {
		return nil, errors.New("another provider allowance is exhausted; no trigger was sent")
	}
	req, err := windowTriggerRequest(ctx, account, model)
	if err != nil {
		return nil, err
	}
	if err := s.claimWindowTrigger(account.ID); err != nil {
		return nil, err
	}
	resp, err := s.streamClient.Do(req)
	if err != nil {
		return nil, errors.New("trigger result is unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("provider rejected the trigger")
	}
	complete := windowTriggerComplete(resp.Body, account.Provider)
	after := s.freshNativeQuota(ctx, account)
	if next := observedWindow(after, "five-hour"); complete && after.Error == "" && activeWindowReset(next) != nil {
		slog.Info("5-hour window started", "gateway", s.gatewayID, "account", account.ID, "model", model.ID, "reset_at", *next.ResetAt)
		return next.ResetAt, nil
	}
	return nil, errors.New("trigger sent, but a new 5-hour reset time is not confirmed")
}

// existingWindowReset returns the reset time of an active 5-hour window, or
// errNoWindow when the account has none to start. Both nil allow a trigger.
func (s *server) existingWindowReset(account storedAccount, before quotaCache) (*time.Time, error) {
	// The saved plan covers a failed usage check.
	if windowPlanSkipped(s.cfg.WindowSkipPlans, account.Provider, firstNonEmpty(before.Plan, account.Plan)) {
		return nil, errNoWindow
	}
	if before.Error != "" {
		return nil, errors.New("could not check the existing window; no trigger was sent")
	}
	window := observedWindow(before, "five-hour")
	if window == nil {
		return nil, errNoWindow
	}
	return activeWindowReset(window), nil
}

// activeWindowReset returns the window's reset time while it lies ahead.
func activeWindowReset(w *QuotaWindow) *time.Time {
	if w == nil || w.ResetAt == nil || !time.Now().Before(*w.ResetAt) {
		return nil
	}
	return w.ResetAt
}

// windowTriggerModel picks the trigger model from the account's catalog.
// Model exclusions apply to gateway-generated inference too.
func (s *server) windowTriggerModel(ctx context.Context, account storedAccount) (Model, error) {
	models, err := s.accountModels(ctx, account)
	if err != nil {
		return Model{}, errors.New("could not load the account's models")
	}
	enabled := []Model{}
	for _, model := range models {
		if blocked, _ := excludedModel(s.store.snapshot().Policy, account.Provider, model.ID); !blocked {
			enabled = append(enabled, model)
		}
	}
	return windowStartModel(enabled, account.Provider)
}

func windowTriggerRequest(ctx context.Context, account storedAccount, model Model) (*http.Request, error) {
	target := "https://api.anthropic.com/v1/messages"
	if account.Provider == "codex" {
		target = "https://chatgpt.com/backend-api/codex/responses"
	}
	values := map[string]json.RawMessage{}
	for k, v := range windowStartPayload(model, account.Provider) {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, errors.New("could not prepare trigger")
		}
		values[k] = raw
	}
	req, err := providerInferenceRequest(ctx, target, values, account)
	if err != nil {
		return nil, errors.New("could not prepare trigger")
	}
	if account.Provider == "codex" {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
}

// claimWindowTrigger records a trigger on the saved account before it is
// sent, refusing one within windowRetryDelay of the last.
func (s *server) claimWindowTrigger(id string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	return s.store.update(func(d *diskState) error {
		for i := range d.Accounts {
			if d.Accounts[i].ID != id {
				continue
			}
			if d.Accounts[i].Disabled {
				return errors.New("account is disabled")
			}
			if time.Since(d.Accounts[i].WindowTriggerAt) < windowRetryDelay {
				return errors.New("a trigger was recently sent")
			}
			d.Accounts[i].WindowTriggerAt = time.Now().UTC()
			return nil
		}
		return errors.New("account was removed")
	})
}

// windowTriggerComplete consumes the trigger response so the request
// completes and can start the window, and reports whether it finished.
// Output is bounded and never logged.
func windowTriggerComplete(body io.Reader, provider string) bool {
	raw, readErr := io.ReadAll(io.LimitReader(body, (1<<20)+1))
	complete := readErr == nil && len(raw) <= 1<<20
	if provider == "codex" {
		var parser usageParser
		parser.begin(true)
		parser.observe(raw)
		parser.complete()
		complete = complete && parser.terminal && !parser.terminalFailure
	}
	if provider == "claude" {
		var parser usageParser
		parser.begin(false)
		parser.observe(raw)
		parser.complete()
		complete = complete && parser.totals().Known
	}
	return complete
}

func observedWindow(q quotaCache, id string) *QuotaWindow {
	for i := range q.ReportedWindows {
		if q.ReportedWindows[i].ID == id {
			return &q.ReportedWindows[i]
		}
	}
	return nil
}
