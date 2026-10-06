package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeProfilePlan(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"pro", `{"organization":{"organization_type":"claude_pro","rate_limit_tier":"default_claude_ai"}}`, "Pro"},
		{"max5", `{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_5x"}}`, "Max 5x"},
		{"max20", `{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`, "Max 20x"},
		{"maxUnknownMultiplier", `{"organization":{"organization_type":"claude_max","rate_limit_tier":"future_tier"}}`, "Max"},
		{"team", `{"organization":{"organization_type":"claude_team"}}`, "Team"},
		{"enterprise", `{"organization":{"organization_type":"claude_enterprise"}}`, "Enterprise"},
		{"accountFlags", `{"account":{"has_claude_max":true,"has_claude_pro":true}}`, "Max"},
		{"unknown", `{"organization":{"organization_type":"future_plan","billing_type":"stripe_subscription"}}`, ""},
		{"empty", `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p claudeProfile
			if err := json.Unmarshal([]byte(tc.body), &p); err != nil {
				t.Fatal(err)
			}
			if got := p.plan(); got != tc.want {
				t.Fatalf("plan = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClaudePlanAndQuotaFailIndependently(t *testing.T) {
	for _, tc := range []struct {
		name          string
		profileStatus int
		usageStatus   int
		profileID     string
		wantPlan      string
	}{
		{"success", 200, 200, "account", "Pro"},
		{"profileFailure", 503, 200, "account", "Saved plan"},
		{"wrongIdentity", 200, 200, "other-account", "Saved plan"},
		{"missingIdentity", 200, 200, "", "Saved plan"},
		{"usageFailure", 200, 503, "account", "Pro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := oauthServer(t)
			a := storedAccount{ID: "claude", Provider: "claude", AuthMode: "oauth", AccountID: "account", AccessToken: "provider-secret", Plan: "Saved plan", ExpiresAt: time.Now().Add(time.Hour)}
			if err := s.store.update(func(d *diskState) error { d.Accounts = []storedAccount{a}; return nil }); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
					t.Error("profile/usage did not use the saved provider credential")
				}
				switch r.URL.String() {
				case claudeProfileURL:
					return oauthReply(tc.profileStatus, map[string]any{"account": map[string]any{"uuid": tc.profileID}, "organization": map[string]any{"organization_type": "claude_pro"}}), nil
				case "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1":
					return oauthReply(tc.usageStatus, map[string]any{"five_hour": map[string]any{"utilization": 25}}), nil
				default:
					t.Errorf("unexpected provider request: %s", r.URL.Path)
					return oauthReply(404, nil), nil
				}
			})
			for range 2 {
				got := s.accounts(context.Background())[0]
				if got.Plan != tc.wantPlan {
					t.Fatalf("plan = %q, want %q", got.Plan, tc.wantPlan)
				}
				if tc.usageStatus == 200 {
					if len(got.Windows) != 1 || got.Windows[0].Remaining != 75 || got.QuotaError != "" {
						t.Fatal("profile response lost valid usage")
					}
				} else if got.QuotaError == "" || len(got.Windows) != 0 {
					t.Fatal("failed usage was reported as valid")
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("profile and quota not cached: %d calls", calls.Load())
			}
			if err := s.store.update(func(d *diskState) error { d.Accounts[0].Disabled = true; return nil }); err != nil {
				t.Fatal(err)
			}
			if got := s.accounts(context.Background())[0]; got.Plan != tc.wantPlan || calls.Load() != 2 {
				t.Fatal("disabled account lost saved plan or fetched metadata")
			}
		})
	}
}
