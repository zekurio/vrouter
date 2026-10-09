package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
	for _, entry := range strings.Split(list, ",") {
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
		for _, supported := range model.Reasoning {
			if wanted == supported {
				return wanted
			}
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
			if errors.Is(err, errNoWindow) {
				// Not a failure; look again later in case the plan changes.
				watch.next, err = time.Now().Add(time.Hour), nil
			} else if err != nil {
				watch = windowWatch{next: time.Now().Add(windowRetryDelay), lastErr: err.Error()}
			} else {
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
	if window.ResetAt != nil && time.Now().Before(*window.ResetAt) {
		return window.ResetAt, nil
	}
	models, err := s.accountModels(ctx, account)
	if err != nil {
		return nil, errors.New("could not load the account's models")
	}
	// Honor model exclusions, including for gateway-generated inference.
	enabled := []Model{}
	for _, model := range models {
		if blocked, _ := excludedModel(s.store.snapshot().Policy, account.Provider, model.ID); !blocked {
			enabled = append(enabled, model)
		}
	}
	model, err := windowStartModel(enabled, account.Provider)
	if err != nil {
		return nil, err
	}
	if len(quotaBlockers(account, before, model.ID, time.Now())) > 0 {
		return nil, errors.New("another provider allowance is exhausted; no trigger was sent")
	}
	target := "https://api.anthropic.com/v1/messages"
	if account.Provider == "codex" {
		target = "https://chatgpt.com/backend-api/codex/responses"
	}
	values := map[string]json.RawMessage{}
	for k, v := range windowStartPayload(model, account.Provider) {
		values[k], _ = json.Marshal(v)
	}
	req, err := providerInferenceRequest(ctx, target, values, account)
	if err != nil {
		return nil, errors.New("could not prepare trigger")
	}
	if account.Provider == "codex" {
		req.Header.Set("Accept", "text/event-stream")
	}
	s.refreshMu.Lock()
	err = s.store.update(func(d *diskState) error {
		for i := range d.Accounts {
			if d.Accounts[i].ID == account.ID {
				if d.Accounts[i].Disabled {
					return errors.New("account is disabled")
				}
				if time.Since(d.Accounts[i].WindowTriggerAt) < windowRetryDelay {
					return errors.New("a trigger was recently sent")
				}
				d.Accounts[i].WindowTriggerAt = time.Now().UTC()
				return nil
			}
		}
		return errors.New("account was removed")
	})
	s.refreshMu.Unlock()
	if err != nil {
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
	// Consume the response so the request completes and can start the window.
	// Bound both duration (ctx above) and output; never log generated content.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	complete := readErr == nil && len(raw) <= 1<<20
	if account.Provider == "codex" {
		var parser usageParser
		parser.begin(true)
		parser.observe(raw)
		parser.complete()
		complete = complete && parser.terminal && !parser.terminalFailure
	}
	if account.Provider == "claude" {
		var parser usageParser
		parser.begin(false)
		parser.observe(raw)
		parser.complete()
		complete = complete && parser.totals().Known
	}
	after := s.freshNativeQuota(ctx, account)
	if next := observedWindow(after, "five-hour"); complete && after.Error == "" && next != nil && next.ResetAt != nil && time.Now().Before(*next.ResetAt) {
		slog.Info("5-hour window started", "gateway", s.gatewayID, "account", account.ID, "model", model.ID, "reset_at", *next.ResetAt)
		return next.ResetAt, nil
	}
	return nil, errors.New("trigger sent, but a new 5-hour reset time is not confirmed")
}

func observedWindow(q quotaCache, id string) *QuotaWindow {
	for i := range q.ReportedWindows {
		if q.ReportedWindows[i].ID == id {
			return &q.ReportedWindows[i]
		}
	}
	return nil
}
