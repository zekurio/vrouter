package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake provider implements usage, profile, reset and inference independently.
// It lets the tests exercise the public inference route without spending a reset.
type resetProvider struct {
	mu                sync.Mutex
	provider          string
	used              map[string]float64
	credits           map[string]int
	resetAccounts     []string
	requestIDs        []string
	inferenceAccounts []string
	failReset         bool
	delayRecovery     bool
	rejectReset       string
	unknown           string
}

func resetFixture(t *testing.T, provider string) (*server, *resetProvider, []storedAccount) {
	t.Helper()
	s := oauthServer(t)
	p := &resetProvider{provider: provider, used: map[string]float64{"a": 100, "b": 100}, credits: map[string]int{"a": 1, "b": 3}}
	accounts := []storedAccount{nativeAccount("a"), nativeAccount("b")}
	for i := range accounts {
		accounts[i].AccountID = accounts[i].ID
		if provider == "claude" {
			accounts[i].Provider = "claude"
			accounts[i].AuthMode = "oauth"
		}
		s.catalogs[accounts[i].ID] = catalogCache{Models: []Model{{ID: "model"}}, Expires: time.Now().Add(time.Hour)}
	}
	nativeSeed(t, s, accounts...)
	nativeMock(s, oauthTransport(p.respond))
	return s, p, accounts
}

func (p *resetProvider) respond(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer provider-")
	if p.provider == "claude" && (r.URL.Path == "/api/oauth/usage" || strings.HasSuffix(r.URL.Path, "/reset_rate_limits")) && r.UserAgent() != "claude-cli/2.1.291 (external, cli)" {
		return oauthReply(200, map[string]any{"cedar_ember": map[string]any{"eligible": false, "ineligible_reason": "surface", "grants": []any{}}}), nil
	}
	if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/usage") {
		if id == p.unknown {
			return oauthReply(503, nil), nil
		}
		if p.provider == "codex" {
			return oauthReply(200, map[string]any{
				"rate_limit":               map[string]any{"allowed": p.used[id] < 100, "primary_window": map[string]any{"used_percent": p.used[id], "limit_window_seconds": 18000, "reset_at": time.Now().Add(time.Hour).Unix()}},
				"rate_limit_reset_credits": map[string]any{"available_count": p.credits[id]},
			}), nil
		}
		return oauthReply(200, map[string]any{
			"five_hour":   map[string]any{"utilization": p.used[id], "resets_at": time.Now().Add(time.Hour)},
			"cedar_ember": map[string]any{"eligible": true, "next_grant_id": "gift", "grants": []any{map[string]any{"id": "gift", "resets_left": p.credits[id], "usable_now": true, "clears": []string{"five_hour", "seven_day"}}}},
		}), nil
	}
	if r.URL.String() == claudeProfileURL {
		return oauthReply(200, map[string]any{"account": map[string]string{"uuid": id}, "organization": map[string]string{"uuid": "org-" + id}}), nil
	}
	if strings.HasSuffix(r.URL.Path, "/consume") || strings.HasSuffix(r.URL.Path, "/reset_rate_limits") {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		requestID := body["redeem_request_id"]
		if p.provider == "claude" {
			requestID = body["request_id"]
			if body["program"] != "cedar_ember" || body["grant_id"] != "gift" || r.URL.Path != "/api/organizations/org-"+id+"/reset_rate_limits" {
				return nil, errors.New("invalid Claude reset contract")
			}
		}
		if requestID == "" {
			return nil, errors.New("missing idempotency key")
		}
		p.resetAccounts = append(p.resetAccounts, id)
		p.requestIDs = append(p.requestIDs, requestID)
		if id == p.rejectReset {
			return oauthReply(200, map[string]any{"code": "no_credit", "result": "ineligible"}), nil
		}
		if p.failReset {
			return nil, errors.New("connection lost")
		}
		if !p.delayRecovery {
			p.used[id] = 0
		}
		p.credits[id]--
		return oauthReply(200, map[string]any{"code": "reset", "result": "reset"}), nil
	}
	if r.Method == "POST" && (r.URL.Path == "/backend-api/codex/responses" || r.URL.Path == "/v1/messages") {
		p.inferenceAccounts = append(p.inferenceAccounts, id)
		if p.used[id] >= 100 {
			return oauthReply(429, nil), nil
		}
		return nativeSSE("data: done\n\n"), nil
	}
	return nil, errors.New("unexpected provider request")
}

func callResetInference(s *server, provider string) int {
	path := "/v1/responses"
	if provider == "claude" {
		path = "/v1/messages"
	}
	return nativeCall(s, "POST", path, "client", `{"model":"model","stream":true}`).Code
}

func TestUsageRotationBeforeSpendingReset(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			s, p, _ := resetFixture(t, provider)
			p.used["a"] = 20
			for range 3 {
				if code := callResetInference(s, provider); code != 200 {
					t.Fatalf("inference: %d", code)
				}
			}
			if len(p.resetAccounts) != 0 || strings.Join(p.inferenceAccounts, ",") != "a,a,a" {
				t.Fatal("did not exhaust usable accounts first")
			}
			p.used["a"] = 100
			s.quotaMu.Lock()
			s.quotas = map[string]quotaCache{}
			s.quotaMu.Unlock()
			if code := callResetInference(s, provider); code != 200 {
				t.Fatalf("reset inference: %d", code)
			}
			if strings.Join(p.resetAccounts, ",") != "b" || p.inferenceAccounts[3] != "b" {
				t.Fatal("did not redeem account with most resets")
			}
			if p.credits["b"] != 2 || len(s.store.snapshot().ResetAttempts) != 0 {
				t.Fatal("reset not reconciled")
			}
		})
	}
}

func TestConcurrentRequestsSpendOneReset(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			s, p, _ := resetFixture(t, provider)
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if code := callResetInference(s, provider); code != 200 {
						t.Errorf("inference: %d", code)
					}
				}()
			}
			wg.Wait()
			if strings.Join(p.resetAccounts, ",") != "b" {
				t.Fatalf("spent multiple resets: %v", p.resetAccounts)
			}
		})
	}
}

func TestAmbiguousResetReusesPersistedRequestAndBlocksOtherAccounts(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			s, p, _ := resetFixture(t, provider)
			p.failReset = true
			if code := callResetInference(s, provider); code != 429 {
				t.Fatalf("unconfirmed reset: %d", code)
			}
			if code := callResetInference(s, provider); code != 429 {
				t.Fatalf("pending reset: %d", code)
			}
			if len(p.resetAccounts) != 1 {
				t.Fatal("spent another reset while result was unknown")
			}
			attempt := s.store.snapshot().ResetAttempts["b"]
			if attempt.RequestID != p.requestIDs[0] {
				t.Fatal("idempotency key was not persisted")
			}
			// Restart from the saved disk state, with the retry delay elapsed.
			attempt.LastTry = time.Now().Add(-2 * time.Minute)
			if err := s.saveResetAttempt("b", &attempt); err != nil {
				t.Fatal(err)
			}
			dir := s.store.dir
			if err := s.store.close(); err != nil {
				t.Fatal(err)
			}
			var err error
			s.store, err = openStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			p.failReset = false
			if code := callResetInference(s, provider); code != 200 {
				t.Fatalf("retry: %d", code)
			}
			if len(p.requestIDs) != 2 || p.requestIDs[0] != p.requestIDs[1] || strings.Join(p.resetAccounts, ",") != "b,b" {
				t.Fatal("retry did not reuse persisted request")
			}
		})
	}
}

func TestUnknownUsageNeverSpendsReset(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			s, p, _ := resetFixture(t, provider)
			p.unknown = "a"
			if code := callResetInference(s, provider); code != 429 {
				t.Fatalf("provider limit: %d", code)
			}
			if len(p.resetAccounts) != 0 {
				t.Fatal("unknown usage authorized reset")
			}
		})
	}
}

func TestDelayedResetRecoveryAllowsFutureResets(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			s, p, _ := resetFixture(t, provider)
			p.delayRecovery = true
			if code := callResetInference(s, provider); code != 429 {
				t.Fatalf("delayed reset: %d", code)
			}
			if code := callResetInference(s, provider); code != 429 {
				t.Fatalf("unconfirmed recovery: %d", code)
			}
			if len(p.resetAccounts) != 1 {
				t.Fatal("spent another reset before recovery")
			}
			// A later request observes the refill without entering the reset path.
			p.used["b"] = 10
			s.quotaMu.Lock()
			s.quotas = map[string]quotaCache{}
			s.quotaMu.Unlock()
			if code := callResetInference(s, provider); code != 200 {
				t.Fatalf("recovered: %d", code)
			}
			if len(s.store.snapshot().ResetAttempts) != 0 {
				t.Fatal("later recovery did not reconcile completed attempt")
			}
			p.used["b"] = 100
			p.delayRecovery = false
			s.quotaMu.Lock()
			s.quotas = map[string]quotaCache{}
			s.quotaMu.Unlock()
			if code := callResetInference(s, provider); code != 200 {
				t.Fatalf("next exhaustion: %d", code)
			}
			if len(p.requestIDs) != 2 || p.requestIDs[0] == p.requestIDs[1] {
				t.Fatal("distinct exhaustion did not get a new reset")
			}
		})
	}
}

func TestRejectedResetTriesNextEligibleAccount(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			s, p, _ := resetFixture(t, provider)
			p.rejectReset = "b"
			if code := callResetInference(s, provider); code != 200 {
				t.Fatalf("fallback: %d", code)
			}
			if strings.Join(p.resetAccounts, ",") != "b,a" || p.credits["b"] != 3 || p.credits["a"] != 0 {
				t.Fatal("did not fall back after definitive rejection")
			}
		})
	}
}

func TestResetReadinessNeverSpendsOnManagementRefresh(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			s, p, _ := resetFixture(t, provider)
			accounts := s.accounts(context.Background())
			if len(accounts) != 2 || accounts[1].AvailableResets == nil || *accounts[1].AvailableResets != 3 {
				t.Fatal("available reset count missing from management")
			}
			if len(p.resetAccounts) != 0 || len(p.inferenceAccounts) != 0 {
				t.Fatal("management refresh spent usage")
			}
		})
	}
}

func TestUsageLimitFromInferenceRefreshesBeforeReset(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			s, p, accounts := resetFixture(t, provider)
			// The cached usage is behind the provider. Both inference attempts hit 429.
			for _, a := range accounts {
				s.quotas[a.ID] = quotaCache{ObservedAt: time.Now(), Windows: []QuotaWindow{{ID: "five-hour", Remaining: 30}}}
			}
			if code := callResetInference(s, provider); code != 200 {
				t.Fatalf("429 recovery: %d", code)
			}
			if strings.Join(p.inferenceAccounts, ",") != "a,b,b" || strings.Join(p.resetAccounts, ",") != "b" {
				t.Fatal("did not exhaust pool and recheck before reset")
			}
		})
	}
}

func TestUsageResetKeepsPaidAccountsOutOfSubscriptionPool(t *testing.T) {
	s, p, accounts := resetFixture(t, "codex")
	paid := nativeAccount("paid")
	paid.AuthMode = "api_key"
	nativeSeed(t, s, append(accounts, paid)...)
	s.catalogs[paid.ID] = catalogCache{Models: []Model{{ID: "model"}}, Expires: time.Now().Add(time.Hour)}
	p.credits["a"] = 0
	p.credits["b"] = 0
	if code := callResetInference(s, "codex"); code != 429 {
		t.Fatalf("exhausted pool: %d", code)
	}
	if len(p.inferenceAccounts) != 0 || len(p.resetAccounts) != 0 {
		t.Fatal("paid fallback or reset without credits")
	}
}

func TestNativeQuotaRefreshesAtResetBoundary(t *testing.T) {
	s, p, accounts := resetFixture(t, "codex")
	p.used["a"] = 10
	now := time.Now()
	past := now.Add(-time.Second)
	s.quotas["a"] = quotaCache{ObservedAt: now.Add(-2 * time.Second), Windows: []QuotaWindow{{ID: "five-hour", Remaining: 0, ResetAt: &past}}}
	q := s.nativeQuota(context.Background(), accounts[0])
	if len(q.Windows) != 1 || q.Windows[0].Remaining != 90 {
		t.Fatal("cached exhausted window survived reset boundary")
	}
}

func TestResetEligibilityAndQuotaScope(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second)
	a := storedAccount{Provider: "claude", AuthMode: "oauth"}
	q := quotaCache{Windows: []QuotaWindow{{ID: "opus", Remaining: 0}, {ID: "weekly", Remaining: 40}, {ID: "cowork", Remaining: 0}}, Resets: &resetStatus{Eligible: true, Available: 2, Next: "gift", Grants: []resetGrant{{ID: "gift", Left: 2, Usable: true, Clears: []string{"seven_day_opus"}}}}}
	if len(quotaBlockers(a, q, "claude-sonnet", now)) != 0 {
		t.Fatal("scoped limit blocked another model")
	}
	blockers := quotaBlockers(a, q, "claude-opus", now)
	if _, ok := q.resetGrant(a, blockers, now); !ok {
		t.Fatal("valid grant rejected")
	}
	q.Resets.Grants[0].Ends = &past
	if _, ok := q.resetGrant(a, blockers, now); ok {
		t.Fatal("expired grant accepted")
	}
	q.Resets.Grants[0].Ends = nil
	if _, ok := q.resetGrant(a, append(blockers, "five_hour"), now); ok {
		t.Fatal("reset that leaves account blocked accepted")
	}
	q.Resets.Grants[0].Paused = true
	if _, ok := q.resetGrant(a, blockers, now); ok {
		t.Fatal("paused grant accepted")
	}
	q.Resets.Grants[0].Paused = false
	q.Resets.Next = "different"
	if _, ok := q.resetGrant(a, blockers, now); ok {
		t.Fatal("out-of-order grant accepted")
	}
	windows, _, err := parseQuota("codex", []byte(`{"rate_limit":{"primary_window":{"used_percent":99.99,"limit_window_seconds":18000}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(quotaBlockers(storedAccount{Provider: "codex"}, quotaCache{Windows: windows}, "model", now)) != 0 {
		t.Fatal("rounded display percent treated as exhaustion")
	}
	allowed := true
	if len(quotaBlockers(storedAccount{Provider: "codex"}, quotaCache{Allowed: &allowed, Windows: []QuotaWindow{{ID: "weekly", Remaining: 0}}}, "model", now)) != 0 {
		t.Fatal("provider permission overridden by percentage")
	}
}

func TestClaudeResetEligibilityMatchesT3Code(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		known      bool
		count      int
	}{
		{"unsupported client is unknown", `{"cedar_ember":{"eligible":false,"ineligible_reason":"surface","grants":[],"next_grant_id":null}}`, false, 0},
		{"native client sees launch reset", `{"cedar_ember":{"eligible":true,"next_grant_id":"launch-reset","grants":[{"id":"launch-reset","resets_total":1,"resets_left":1,"starts_at":"2026-09-22T16:00:00+00:00","ends_at":null,"clears":["five_hour","seven_day","seven_day_overage_included"],"paused":false,"usable_now":true,"use_requires_limit":false,"blocking":[]}]}}`, true, 1},
		{"eligible with no grants", `{"cedar_ember":{"eligible":true,"grants":[]}}`, true, 0},
		{"paused grant excluded", `{"cedar_ember":{"eligible":true,"next_grant_id":"next","grants":[{"id":"next","resets_left":1,"usable_now":true},{"id":"paused","resets_left":4,"usable_now":true,"paused":true}]}}`, true, 1},
		{"unusable next grant", `{"cedar_ember":{"eligible":true,"next_grant_id":"next","grants":[{"id":"next","resets_left":1,"usable_now":false}]}}`, true, 0},
		{"expired grant", `{"cedar_ember":{"eligible":true,"next_grant_id":"next","grants":[{"id":"next","resets_left":1,"usable_now":true,"ends_at":"2000-01-01T00:00:00Z"}]}}`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, status := parseResetStatus("claude", []byte(tc.body))
			if (status != nil) != tc.known {
				t.Fatalf("known = %v, want %v", status != nil, tc.known)
			}
			if status != nil && status.Available != tc.count {
				t.Fatalf("resets = %d, want %d", status.Available, tc.count)
			}
		})
	}
}

func TestClaudeResetClientHeaderIsScoped(t *testing.T) {
	for _, tc := range []struct{ path, mode, want string }{
		{"/api/oauth/usage?cedar_ember=1&skip_spend=1", "oauth", "claude-cli/2.1.291 (external, cli)"},
		{"/api/organizations/org/reset_rate_limits", "oauth", "claude-cli/2.1.291 (external, cli)"},
		{"/api/oauth/profile", "oauth", "vrouter/0.2"},
		{"/v1/messages", "oauth", "vrouter/0.2"},
		{"/api/oauth/usage", "api_key", "vrouter/0.2"},
	} {
		req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com"+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		providerHeaders(req, storedAccount{Provider: "claude", AuthMode: tc.mode, AccessToken: "test-token"})
		if req.UserAgent() != tc.want {
			t.Errorf("%s %s: %q", tc.mode, tc.path, req.UserAgent())
		}
	}
}
