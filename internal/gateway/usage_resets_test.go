package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type resetTestQuota struct {
	weekly, fiveHour float64
	reset            *time.Time
	credits          int
	missing          bool
	sonnet, opus     float64
	clears           []string
	cooldown         *time.Time
}

type resetTestProvider struct {
	mu        sync.Mutex
	quotas    map[string]*resetTestQuota
	redeemed  []string
	requestID []string
	uncertain bool
}

func setupResetTest(t *testing.T, provider string) (*server, *resetTestProvider) {
	t.Helper()
	s := testServer(t, Config{ExternalAuth: true})
	now := time.Now().UTC()
	f := &resetTestProvider{quotas: map[string]*resetTestQuota{
		"near": {weekly: 100, reset: ptr(now.Add(24 * time.Hour)), credits: 3},
		"far":  {weekly: 100, reset: ptr(now.Add(5 * 24 * time.Hour)), credits: 1},
		"none": {weekly: 100, reset: ptr(now.Add(6 * 24 * time.Hour))},
	}}
	err := s.store.update(func(d *diskState) error {
		for _, id := range []string{"near", "far", "none"} {
			mode := "oauth"
			if provider == "codex" {
				mode = "codex"
			}
			d.Accounts = append(d.Accounts, storedAccount{ID: id, Provider: provider, AuthMode: mode, AccountID: id, AccessToken: id, ExpiresAt: now.Add(time.Hour)})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		q := f.quotas[id]
		if q == nil {
			t.Errorf("unexpected account %q", id)
			return jsonResponse(`{}`, 401), nil
		}
		var body any
		switch r.URL.Path {
		case "/api/oauth/profile":
			body = map[string]any{"account": map[string]string{"uuid": id}, "organization": map[string]string{"uuid": "org-" + id}}
		case "/api/oauth/usage", "/backend-api/wham/usage":
			if q.missing {
				return jsonResponse(`{}`, 503), nil
			}
			if provider == "claude" {
				clears := q.clears
				if clears == nil {
					clears = []string{"five_hour", "seven_day", "seven_day_sonnet", "seven_day_opus"}
				}
				body = map[string]any{
					"five_hour":        map[string]any{"utilization": q.fiveHour, "resets_at": now.Add(time.Hour)},
					"seven_day":        map[string]any{"utilization": q.weekly, "resets_at": q.reset},
					"seven_day_sonnet": map[string]any{"utilization": q.sonnet, "resets_at": q.reset},
					"seven_day_opus":   map[string]any{"utilization": q.opus, "resets_at": q.reset},
					"cedar_ember": map[string]any{"eligible": true, "next_grant_id": "included", "cooldown_until": q.cooldown,
						"grants": []map[string]any{{"id": "included", "resets_left": q.credits, "usable_now": true, "clears": clears}}},
				}
			} else {
				var reset int64
				if q.reset != nil {
					reset = q.reset.Unix()
				}
				body = map[string]any{
					"rate_limit": map[string]any{"allowed": q.weekly < 100 && q.fiveHour < 100,
						"primary_window":   map[string]any{"used_percent": q.fiveHour, "limit_window_seconds": 18000, "reset_at": now.Add(time.Hour).Unix()},
						"secondary_window": map[string]any{"used_percent": q.weekly, "limit_window_seconds": 604800, "reset_at": reset}},
					"rate_limit_reset_credits": map[string]int{"available_count": q.credits},
				}
			}
		case "/api/organizations/org-" + id + "/reset_rate_limits", "/backend-api/wham/rate-limit-reset-credits/consume":
			var payload map[string]string
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&payload) != nil {
				t.Error("invalid redemption request")
			}
			requestID := payload["redeem_request_id"]
			if provider == "claude" {
				requestID = payload["request_id"]
				if payload["grant_id"] != "included" || payload["program"] != "cedar_ember" {
					t.Errorf("unexpected grant: %v", payload)
				}
			}
			if requestID == "" {
				t.Error("missing idempotency key")
			}
			f.redeemed = append(f.redeemed, id)
			f.requestID = append(f.requestID, requestID)
			if f.uncertain {
				return nil, errors.New("connection lost")
			}
			q.weekly, q.fiveHour, q.sonnet, q.opus = 0, 0, 0, 0
			body = map[string]string{"code": "reset", "result": "reset"}
		default:
			t.Errorf("unexpected provider path %s", r.URL.Path)
			return jsonResponse(`{}`, 404), nil
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Error(err)
		}
		return jsonResponse(string(raw), 200), nil
	})
	return s, f
}

func TestResetSelectsFurthestEligibleWeeklyAccount(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, scenario := range []string{"latest-weekly", "no-credit", "five-hour-only", "missing-date", "expired-date", "rounded-usage", "usable-account", "unknown-usage", "disabled-account", "both-windows", "sonnet-weekly", "unrelated-weekly", "grant-cannot-clear", "grant-cooldown"} {
			if provider == "codex" && slices.Contains([]string{"sonnet-weekly", "unrelated-weekly", "grant-cannot-clear", "grant-cooldown"}, scenario) {
				continue
			}
			t.Run(provider+"/"+scenario, func(t *testing.T) {
				s, f := setupResetTest(t, provider)
				want, recovered := "far", "far"
				q := f.quotas["far"]
				switch scenario {
				case "no-credit":
					q.credits = 0
					want, recovered = "near", "near"
				case "five-hour-only":
					for _, q := range f.quotas {
						q.weekly, q.fiveHour = 20, 100
					}
					want, recovered = "", ""
				case "missing-date":
					q.reset = nil
					want, recovered = "near", "near"
				case "expired-date":
					q.reset, q.fiveHour = ptr(time.Now().Add(-time.Hour)), 100
					want, recovered = "near", "near"
				case "rounded-usage":
					q.weekly, q.fiveHour = 99.96, 100
					want, recovered = "near", "near"
				case "usable-account":
					q.weekly = 20
					want = ""
				case "unknown-usage":
					q.missing = true
					want, recovered = "", ""
				case "disabled-account":
					if err := s.store.update(func(d *diskState) error { d.Accounts[1].Disabled = true; return nil }); err != nil {
						t.Fatal(err)
					}
					want, recovered = "near", "near"
				case "both-windows":
					q.fiveHour = 100
				case "sonnet-weekly":
					q.weekly, q.sonnet = 20, 100
				case "unrelated-weekly":
					q.weekly, q.fiveHour, q.opus = 20, 100, 100
					want, recovered = "near", "near"
				case "grant-cannot-clear":
					q.clears = []string{"five_hour"}
					want, recovered = "near", "near"
				case "grant-cooldown":
					q.cooldown = ptr(time.Now().Add(time.Hour))
					want, recovered = "near", "near"
				}
				got := s.resetExhaustedPool(context.Background(), s.store.snapshot().Accounts, "claude-sonnet-test")
				if want == "" && len(f.redeemed) != 0 || want != "" && !slices.Equal(f.redeemed, []string{want}) {
					t.Fatalf("redeemed %v, want %q", f.redeemed, want)
				}
				if recovered == "" && len(got) != 0 || recovered != "" && (len(got) != 1 || got[0].ID != recovered) {
					t.Fatalf("recovered %v, want %q", got, recovered)
				}
				if len(s.store.snapshot().ResetAttempts) != 0 {
					t.Fatal("confirmed usable account retained a reset attempt")
				}
			})
		}
	}
}

func TestWeeklyResetRetriesKeepOriginalAccountAndRequestID(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			s, f := setupResetTest(t, provider)
			f.uncertain = true
			pool := s.store.snapshot().Accounts
			if got := s.resetExhaustedPool(context.Background(), pool, "claude-sonnet-test"); len(got) != 0 {
				t.Fatal("unconfirmed reset authorized inference")
			}
			if !slices.Equal(f.redeemed, []string{"far"}) {
				t.Fatalf("redeemed %v", f.redeemed)
			}
			// A newly preferred account must not steal an unresolved attempt.
			f.quotas["near"].reset = ptr(time.Now().Add(7 * 24 * time.Hour))
			f.quotas["far"].credits = 0
			s.resetExhaustedPool(context.Background(), pool, "claude-sonnet-test")
			if len(f.redeemed) != 1 {
				t.Fatal("cooldown allowed another reset")
			}
			attempt := s.store.snapshot().ResetAttempts["far"]
			attempt.LastTry = time.Now().Add(-2 * time.Minute)
			if err := s.saveResetAttempt("far", &attempt); err != nil {
				t.Fatal(err)
			}
			// Reopen the store to verify that the retry uses persisted identity.
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			h, err := New(s.cfg, testAssets())
			if err != nil {
				t.Fatal(err)
			}
			reopened := h.(*server)
			defer reopened.Close()
			reopened.client.Transport = s.client.Transport
			f.uncertain = false
			got := reopened.resetExhaustedPool(context.Background(), pool, "claude-sonnet-test")
			if len(got) != 1 || got[0].ID != "far" || !slices.Equal(f.redeemed, []string{"far", "far"}) || f.requestID[0] != f.requestID[1] {
				t.Fatalf("retry changed account or request ID: accounts=%v IDs=%v", f.redeemed, f.requestID)
			}
		})
	}
}

func TestPendingResetDoesNotRedeemForFiveHourOnly(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			s, f := setupResetTest(t, provider)
			for _, q := range f.quotas {
				q.weekly, q.fiveHour = 20, 100
			}
			attempt := resetAttempt{RequestID: "previous-request", GrantID: "included", OrganizationID: "org-far", LastTry: time.Now().Add(-2 * time.Minute)}
			if err := s.saveResetAttempt("far", &attempt); err != nil {
				t.Fatal(err)
			}
			got := s.resetExhaustedPool(context.Background(), s.store.snapshot().Accounts, "claude-sonnet-test")
			if len(got) != 0 || len(f.redeemed) != 0 {
				t.Fatal("pending attempt spent a reset without an exhausted weekly allowance")
			}
			if s.store.snapshot().ResetAttempts["far"].RequestID != attempt.RequestID {
				t.Fatal("unresolved attempt was discarded")
			}
		})
	}
}

func TestRoutingReconcilesResetOnlyWhenEveryModelIsUsable(t *testing.T) {
	s, f := setupResetTest(t, "claude")
	for _, q := range f.quotas {
		q.weekly = 20
	}
	f.quotas["far"].opus = 100
	attempt := resetAttempt{RequestID: "opus-request", GrantID: "included", OrganizationID: "org-far", LastTry: time.Now().Add(-2 * time.Minute)}
	if err := s.saveResetAttempt("far", &attempt); err != nil {
		t.Fatal(err)
	}
	pool := s.store.snapshot().Accounts
	if got := s.usableAccounts(context.Background(), pool, "claude-sonnet-test"); len(got) != 3 {
		t.Fatalf("sonnet routing used %d accounts", len(got))
	}
	if s.store.snapshot().ResetAttempts["far"].RequestID != attempt.RequestID {
		t.Fatal("a sonnet request discarded the unresolved opus reset")
	}
	if got := s.usableAccounts(context.Background(), pool, "claude-opus-test"); len(got) != 2 {
		t.Fatalf("opus routing used %d accounts", len(got))
	}
	f.quotas["far"].opus = 0
	s.quotaMu.Lock()
	clear(s.quotas)
	s.quotaMu.Unlock()
	s.usableAccounts(context.Background(), pool, "claude-sonnet-test")
	if len(s.store.snapshot().ResetAttempts) != 0 {
		t.Fatal("a fully usable account retained its reset attempt")
	}
}
