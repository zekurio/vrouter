package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// claudeAccountFixture is a saved Claude OAuth record with a reliable UUID
// identity. Tests override fields to model legacy imports.
func claudeAccountFixture(id string) storedAccount {
	return storedAccount{
		ID: id, Provider: "claude", AuthMode: "oauth", ClientID: claudeClientID,
		Label: "Work Claude", Email: "User@Example.invalid", AccountID: "uuid-1",
		AccessToken: "old-access", RefreshToken: "old-refresh",
		ExpiresAt: time.Now().Add(-time.Minute), CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// oauthStartProvider starts a sign-in for any provider. Claude's callback
// listener uses a fixed port, so a listener from a session that just finished
// may still be closing; that transient 502 is retried.
func oauthStartProvider(t *testing.T, s *server, provider, body string) (string, *url.URL) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := oauthCall(s, "POST", "/api/oauth/"+provider, body)
		if w.Code == 200 {
			var response struct{ ID, URL string }
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(response.URL)
			if err != nil {
				t.Fatal(err)
			}
			return response.ID, u
		}
		if provider == "claude" && w.Code == http.StatusBadGateway && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("start %s: %d %s", provider, w.Code, w.Body)
	}
}

// claudeTokenReply serves a Claude authorization-code exchange carrying the
// given account identity. It asserts the exchange uses the shared Claude
// client and a PKCE verifier.
func claudeTokenReply(t *testing.T, s *server, uuid, email string, calls *atomic.Int32) {
	t.Helper()
	s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != claudeTokenURL {
			t.Errorf("unexpected outbound request: %s", r.URL)
			return oauthReply(500, nil), nil
		}
		calls.Add(1)
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error("Claude token request was not JSON")
		}
		if body["client_id"] != claudeClientID || body["grant_type"] != "authorization_code" || body["code"] == "" || body["code_verifier"] == "" || body["state"] == "" {
			t.Errorf("wrong Claude token exchange: %v", body)
		}
		return oauthReply(200, map[string]any{
			"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600,
			"token_type": "Bearer", "scope": claudeScope,
			"account": map[string]string{"uuid": uuid, "email_address": email},
		}), nil
	})
}

// oauthSubmit posts a valid redirect URL for a pending session so the
// authorization code exchange runs.
func oauthSubmit(t *testing.T, s *server, id string, session oauthSession) *httptest.ResponseRecorder {
	t.Helper()
	callback := session.RedirectURI + "?" + url.Values{"code": {"claude-code"}, "state": {session.State}}.Encode()
	body, _ := json.Marshal(map[string]string{"redirectUrl": callback})
	return oauthCall(s, "POST", "/api/oauth/sessions/"+id+"/callback", string(body))
}

func TestNativeClaudeSelectedReconnectRenewsInPlace(t *testing.T) {
	s := oauthServer(t)
	old := claudeAccountFixture("claude-record")
	other := storedAccount{ID: "codex-record", Provider: "codex", AuthMode: "chatgpt", ClientID: "oaiapp_test", AccountID: "subject", AccessToken: "codex-access"}
	nativeSeed(t, s, old, other)

	id, u := oauthStartProvider(t, s, "claude", `{"accountId":"claude-record"}`)
	q := u.Query()
	if u.Host != "claude.ai" || u.Path != "/oauth/authorize" || q.Get("client_id") != claudeClientID || q.Get("login_hint") != old.Email || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("bad Claude authorization contract: %s", u)
	}
	if !strings.HasPrefix(u.Query().Get("redirect_uri"), "http://localhost:54545/callback") {
		t.Fatalf("Claude reconnect did not use the exact loopback callback: %s", u)
	}
	for _, hint := range []string{"id_token_hint", "resource", "nonce", "ext_agent_host_id", "agent_name_hint", "prompt"} {
		if q.Has(hint) {
			t.Fatalf("Claude authorization carried ChatGPT hint %q: %s", hint, u)
		}
	}
	session := s.oauth[id]
	if session.AccountID != old.ID || session.ClientID != claudeClientID {
		t.Fatalf("session = %+v; want selected %q with the shared Claude client", session, old.ID)
	}
	// Stale catalog and usage caches must be dropped with the old credential.
	s.catalogMu.Lock()
	s.catalogs[old.ID] = catalogCache{}
	s.catalogMu.Unlock()
	s.quotaMu.Lock()
	s.quotas[old.ID] = quotaCache{}
	s.quotaMu.Unlock()

	var calls atomic.Int32
	claudeTokenReply(t, s, "uuid-1", "user@example.invalid", &calls)
	if w := oauthSubmit(t, s, id, session); w.Code != 200 {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("token exchange calls = %d; want 1", calls.Load())
	}

	accounts := s.store.snapshot().Accounts
	if len(accounts) != 2 {
		t.Fatalf("stored accounts = %d; want 2 records without a duplicate", len(accounts))
	}
	got := accounts[0]
	if got.ID != old.ID {
		t.Fatalf("selected record was replaced: %+v", got)
	}
	if !got.CreatedAt.Equal(old.CreatedAt) || got.Label != old.Label || got.Disabled != old.Disabled {
		t.Fatalf("reconnect did not preserve record metadata: %+v", got)
	}
	if got.ClientID != claudeClientID || got.AccountID != "uuid-1" || got.Email != "user@example.invalid" {
		t.Fatalf("reconnect did not store the verified identity: %+v", got)
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Fatalf("reconnect did not store the new tokens: %+v", got)
	}
	if accounts[1].AccessToken != "codex-access" || accounts[1].ID != other.ID {
		t.Fatal("reconnect modified an unrelated account")
	}
	s.catalogMu.Lock()
	_, cachedCatalog := s.catalogs[old.ID]
	s.catalogMu.Unlock()
	s.quotaMu.Lock()
	_, cachedQuota := s.quotas[old.ID]
	s.quotaMu.Unlock()
	if cachedCatalog || cachedQuota {
		t.Fatal("reconnect kept caches for the previous credential")
	}
}

func TestNativeClaudeReconnectRejectsUnverifiedIdentity(t *testing.T) {
	uuidCase := claudeAccountFixture("claude-tokenless")
	uuidCase.AccountID = ""
	cases := []struct {
		name       string
		stored     storedAccount
		tokenUUID  string
		tokenEmail string
	}{
		{"different uuid", claudeAccountFixture("claude-record"), "uuid-2", "user@example.invalid"},
		{"missing uuid", claudeAccountFixture("claude-record"), "", "user@example.invalid"},
		{"legacy email mismatch", storedAccount{ID: "claude-legacy", Provider: "claude", AuthMode: "oauth", Email: "User@Example.invalid", AccessToken: "old", RefreshToken: "old"}, "", "other@example.invalid"},
		{"legacy missing stored identity", storedAccount{ID: "claude-blank", Provider: "claude", AuthMode: "oauth", AccessToken: "old", RefreshToken: "old"}, "", "user@example.invalid"},
		{"legacy missing token identity", uuidCase, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := oauthServer(t)
			nativeSeed(t, s, tc.stored)
			before := s.store.snapshot()

			id, _ := oauthStartProvider(t, s, "claude", `{"accountId":"`+tc.stored.ID+`"}`)
			session := s.oauth[id]
			var calls atomic.Int32
			claudeTokenReply(t, s, tc.tokenUUID, tc.tokenEmail, &calls)
			w := oauthSubmit(t, s, id, session)
			if w.Code == 200 {
				t.Fatalf("unverified identity was accepted: %s", w.Body)
			}
			if calls.Load() != 1 {
				t.Fatalf("token exchange calls = %d; want 1", calls.Load())
			}
			if after := s.store.snapshot(); !reflect.DeepEqual(before, after) {
				t.Fatalf("failed reconnect changed the store:\nbefore: %+v\nafter:  %+v", before, after)
			}
		})
	}
}

func TestNativeClaudeLegacyClientIDReconnect(t *testing.T) {
	t.Run("empty stored client renews via email", func(t *testing.T) {
		s := oauthServer(t)
		old := claudeAccountFixture("claude-legacy")
		old.ClientID = ""
		old.AccountID = ""
		old.Label = "Imported Claude"
		nativeSeed(t, s, old)

		id, u := oauthStartProvider(t, s, "claude", `{"accountId":"claude-legacy"}`)
		if u.Query().Get("client_id") != claudeClientID {
			t.Fatalf("legacy reconnect did not use the shared Claude client: %s", u)
		}
		session := s.oauth[id]
		if session.ClientID != claudeClientID {
			t.Fatalf("session client = %q; want %q", session.ClientID, claudeClientID)
		}
		var calls atomic.Int32
		claudeTokenReply(t, s, "uuid-new", "user@example.invalid", &calls)
		if w := oauthSubmit(t, s, id, session); w.Code != 200 {
			t.Fatalf("callback: %d %s", w.Code, w.Body)
		}
		accounts := s.store.snapshot().Accounts
		if len(accounts) != 1 {
			t.Fatalf("stored accounts = %d; want 1 without a duplicate", len(accounts))
		}
		got := accounts[0]
		if got.ID != old.ID || got.ClientID != claudeClientID || got.AccountID != "uuid-new" || got.Email != "user@example.invalid" || got.Label != "Imported Claude" || got.AccessToken != "new-access" {
			t.Fatalf("legacy reconnect stored %+v", got)
		}
	})

	t.Run("incompatible stored client is refused", func(t *testing.T) {
		s := oauthServer(t)
		old := claudeAccountFixture("claude-foreign")
		old.ClientID = "claude_foreign_client"
		nativeSeed(t, s, old)
		before := s.store.snapshot()

		w := oauthCall(s, "POST", "/api/oauth/claude", `{"accountId":"claude-foreign"}`)
		if w.Code != 400 {
			t.Fatalf("start with a foreign client = %d %s; want 400", w.Code, w.Body)
		}
		if len(s.oauth) != 0 || len(s.callbacks) != 0 {
			t.Fatal("refused registration still opened a sign-in session")
		}
		if !reflect.DeepEqual(before, s.store.snapshot()) {
			t.Fatal("refused registration changed the store")
		}
	})
}

func TestNativeClaudeReconnectRejectsCrossProviderSelection(t *testing.T) {
	cases := []struct {
		name, provider string
		stored         storedAccount
	}{
		{"codex record with claude", "claude", storedAccount{ID: "codex-record", Provider: "codex", AuthMode: "chatgpt", ClientID: "oaiapp_test", AccountID: "subject", AccessToken: "codex-access"}},
		{"claude record with codex", "codex", claudeAccountFixture("claude-record")},
		{"api key record with claude", "claude", storedAccount{ID: "api-key", Provider: "claude", AuthMode: "api_key", AccessToken: "key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := oauthServer(t)
			nativeSeed(t, s, tc.stored)
			before := s.store.snapshot()

			w := oauthCall(s, "POST", "/api/oauth/"+tc.provider, `{"accountId":"`+tc.stored.ID+`"}`)
			if w.Code != 404 {
				t.Fatalf("cross-provider reconnect = %d %s; want 404", w.Code, w.Body)
			}
			if len(s.oauth) != 0 {
				t.Fatal("refused selection opened a sign-in session")
			}
			if !reflect.DeepEqual(before, s.store.snapshot()) {
				t.Fatal("refused selection changed the store")
			}
		})
	}
}

func TestNativeClaudeReconnectPreservesDisabled(t *testing.T) {
	s := oauthServer(t)
	old := claudeAccountFixture("claude-disabled")
	old.Disabled = true
	nativeSeed(t, s, old)

	id, _ := oauthStartProvider(t, s, "claude", `{"accountId":"claude-disabled"}`)
	session := s.oauth[id]
	var calls atomic.Int32
	claudeTokenReply(t, s, "uuid-1", "user@example.invalid", &calls)
	if w := oauthSubmit(t, s, id, session); w.Code != 200 {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	accounts := s.store.snapshot().Accounts
	if len(accounts) != 1 || !accounts[0].Disabled || accounts[0].AccessToken != "new-access" {
		t.Fatalf("disabled state was not preserved: %+v", accounts)
	}
}

func TestNativeClaudeReconnectRefusesChangedSelection(t *testing.T) {
	t.Run("removed", func(t *testing.T) {
		s := oauthServer(t)
		nativeSeed(t, s, claudeAccountFixture("claude-record"))
		id, _ := oauthStartProvider(t, s, "claude", `{"accountId":"claude-record"}`)
		session := s.oauth[id]
		if err := s.store.update(func(d *diskState) error { d.Accounts = nil; return nil }); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		claudeTokenReply(t, s, "uuid-1", "user@example.invalid", &calls)
		if w := oauthSubmit(t, s, id, session); w.Code == 200 {
			t.Fatal("removed selection was replaced")
		}
		if accounts := s.store.snapshot().Accounts; len(accounts) != 0 {
			t.Fatalf("removed selection was recreated: %+v", accounts)
		}
	})

	t.Run("identity changed", func(t *testing.T) {
		s := oauthServer(t)
		nativeSeed(t, s, claudeAccountFixture("claude-record"))
		id, _ := oauthStartProvider(t, s, "claude", `{"accountId":"claude-record"}`)
		session := s.oauth[id]
		if err := s.store.update(func(d *diskState) error { d.Accounts[0].AccountID = "uuid-other"; return nil }); err != nil {
			t.Fatal(err)
		}
		changed := s.store.snapshot()
		var calls atomic.Int32
		claudeTokenReply(t, s, "uuid-1", "user@example.invalid", &calls)
		if w := oauthSubmit(t, s, id, session); w.Code == 200 {
			t.Fatal("changed selection was replaced")
		}
		if !reflect.DeepEqual(changed, s.store.snapshot()) {
			t.Fatal("failed reconnect wrote to the store")
		}
	})
}

func TestNativeClaudeUnselectedSignInDeduplicates(t *testing.T) {
	t.Run("same identity renews record", func(t *testing.T) {
		s := oauthServer(t)
		old := claudeAccountFixture("claude-record")
		nativeSeed(t, s, old)
		id, _ := oauthStartProvider(t, s, "claude", "")
		session := s.oauth[id]
		var calls atomic.Int32
		claudeTokenReply(t, s, "uuid-1", "USER@example.invalid", &calls)
		if w := oauthSubmit(t, s, id, session); w.Code != 200 {
			t.Fatalf("callback: %d %s", w.Code, w.Body)
		}
		accounts := s.store.snapshot().Accounts
		if len(accounts) != 1 || accounts[0].ID != old.ID || accounts[0].AccessToken != "new-access" {
			t.Fatalf("same-identity sign-in duplicated the account: %+v", accounts)
		}
	})

	t.Run("different identity appends", func(t *testing.T) {
		s := oauthServer(t)
		nativeSeed(t, s, claudeAccountFixture("claude-record"))
		id, _ := oauthStartProvider(t, s, "claude", "")
		session := s.oauth[id]
		var calls atomic.Int32
		claudeTokenReply(t, s, "uuid-2", "other@example.invalid", &calls)
		if w := oauthSubmit(t, s, id, session); w.Code != 200 {
			t.Fatalf("callback: %d %s", w.Code, w.Body)
		}
		if accounts := s.store.snapshot().Accounts; len(accounts) != 2 {
			t.Fatalf("different identity = %d accounts; want 2", len(accounts))
		}
	})
}

func TestNativeClaudeAccountsReconnectable(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s,
		storedAccount{ID: "uuid-only", Provider: "claude", AuthMode: "oauth", AccountID: "uuid-1", AccessToken: "access", Disabled: true},
		storedAccount{ID: "email-only", Provider: "claude", AuthMode: "oauth", Email: "user@example.invalid", AccessToken: "access", Disabled: true},
		storedAccount{ID: "no-identity", Provider: "claude", AuthMode: "oauth", AccessToken: "access", Disabled: true},
		storedAccount{ID: "api-key", Provider: "claude", AuthMode: "api_key", AccessToken: "access", Disabled: true},
		storedAccount{ID: "chatgpt", Provider: "codex", AuthMode: "chatgpt", ClientID: "oaiapp_test", AccountID: "subject", AccessToken: "access", Disabled: true},
	)
	byID := map[string]Account{}
	for _, a := range s.accounts(context.Background()) {
		byID[a.ID] = a
	}
	for id, want := range map[string]bool{"uuid-only": true, "email-only": true, "no-identity": false, "api-key": false, "chatgpt": false} {
		if got := byID[id].Reconnectable; got != want {
			t.Errorf("%s reconnectable = %v; want %v", id, got, want)
		}
	}
}
