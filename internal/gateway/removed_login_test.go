package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRemovedChatGPTConnectionsNeverContactProvider(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid token", true: "expired token"}[expired], func(t *testing.T) {
			s := oauthServer(t)
			a := storedAccount{ID: "old", Provider: "codex", AuthMode: "chatgpt", ClientID: "old-client", AccountID: "old-user", AccessToken: "old-access", RefreshToken: "old-refresh", Scopes: []string{"chatgpt.tokens.use.direct"}, ExpiresAt: time.Now().Add(time.Hour)}
			if expired {
				a.ExpiresAt = time.Now().Add(-time.Hour)
			}
			nativeSeed(t, s, a)
			before := s.store.snapshot()
			nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
				t.Errorf("removed credential contacted provider: %s", r.URL.Path)
				return oauthReply(500, nil), nil
			}))
			ctx := context.Background()
			if _, err := s.accessAccount(ctx, a.ID); err == nil {
				t.Fatal("removed credential remained accessible")
			}
			if _, err := s.accountModels(ctx, a); err == nil {
				t.Fatal("removed credential remained eligible for discovery")
			}
			accounts := s.accounts(ctx)
			if len(accounts) != 1 || accounts[0].Status != "unavailable" || accounts[0].Reconnectable || !strings.Contains(accounts[0].StatusMessage, "Codex sign-in") || len(accounts[0].Windows) != 0 {
				t.Fatal("removed account must remain visible, unavailable and non-reconnectable")
			}
			w := nativeCall(s, "GET", "/v1/models", "client", "")
			var catalog struct{ Data []json.RawMessage }
			if json.Unmarshal(w.Body.Bytes(), &catalog) != nil || w.Code != 200 || len(catalog.Data) != 0 {
				t.Fatal("removed account advertised models")
			}
			if w = nativeCall(s, "POST", "/v1/responses", "client", `{"model":"model","stream":true}`); w.Code != 404 {
				t.Fatalf("removed account remained routable: %d", w.Code)
			}
			for _, body := range []string{`{"accountId":"old"}`, `{"accountId":"old","authMode":"codex"}`, `{"accountId":"old","authMode":"chatgpt"}`} {
				if w = oauthCall(s, "POST", "/api/oauth/codex", body); w.Code != 400 {
					t.Fatalf("removed account could reconnect: %d", w.Code)
				}
			}
			if len(s.oauth) != 0 || len(s.callbacks) != 0 || !reflect.DeepEqual(before, s.store.snapshot()) {
				t.Fatal("rejected flow changed state or opened a listener")
			}
			if w = oauthCall(s, "DELETE", "/api/accounts", `{"id":"old"}`); w.Code != 200 || len(s.store.snapshot().Accounts) != 0 {
				t.Fatal("old connection cannot be removed")
			}
		})
	}
}
