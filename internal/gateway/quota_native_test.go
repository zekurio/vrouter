package gateway

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeQuotaUsesProviderCredentialAndCoalesces(t *testing.T) {
	for _, p := range []string{"codex", "claude"} {
		t.Run(p, func(t *testing.T) {
			s := oauthServer(t)
			mode := "codex"
			target := "https://chatgpt.com/backend-api/wham/usage"
			body := any(map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 25, "limit_window_seconds": 604800}}})
			if p == "claude" {
				mode = "oauth"
				target = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1"
				body = map[string]any{"seven_day": map[string]any{"utilization": 25}}
			}
			a := storedAccount{ID: "quota", Provider: p, AuthMode: mode, AccountID: "account", AccessToken: "provider-token", ExpiresAt: time.Now().Add(time.Hour)}
			if err := s.store.update(func(d *diskState) error { d.Accounts = append(d.Accounts, a); return nil }); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var profileCalls atomic.Int32
			s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
				if p == "claude" && r.URL.String() == claudeProfileURL {
					profileCalls.Add(1)
					return oauthReply(200, map[string]any{"account": map[string]any{"uuid": "account"}, "organization": map[string]any{"organization_type": "claude_pro"}}), nil
				}
				calls.Add(1)
				if r.URL.String() != target || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer provider-token" {
					t.Error("wrong quota request")
				}
				if p == "codex" && r.Header.Get("ChatGPT-Account-ID") != "account" {
					t.Error("missing account identity")
				}
				time.Sleep(10 * time.Millisecond)
				return oauthReply(200, body), nil
			})
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					q := s.nativeQuota(context.Background(), a)
					if q.Error != "" || len(q.Windows) != 1 || q.Windows[0].Remaining != 75 {
						t.Error("unexpected quota")
					}
				}()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("usage fetched %d times", calls.Load())
			}
			if p == "claude" && profileCalls.Load() != 1 {
				t.Fatalf("profile fetched %d times", profileCalls.Load())
			}
		})
	}
}
func TestNativeQuotaDenialStaysUnknown(t *testing.T) {
	s := oauthServer(t)
	a := storedAccount{ID: "denied", Provider: "codex", AuthMode: "codex", AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.store.update(func(d *diskState) error { d.Accounts = append(d.Accounts, a); return nil }); err != nil {
		t.Fatal(err)
	}
	s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		return oauthReply(403, map[string]string{"detail": "private-provider-diagnostic"}), nil
	})
	q := s.nativeQuota(context.Background(), a)
	if q.Error == "" || len(q.Windows) != 0 || q.Error == "private-provider-diagnostic" {
		t.Fatal("denial fabricated or disclosed usage")
	}
}
