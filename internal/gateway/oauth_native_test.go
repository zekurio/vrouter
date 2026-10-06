package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// nativeCodexStart starts a native Codex sign-in. The flow binds a fixed
// callback port, so a listener from a session that just finished may still be
// closing; that transient 502 is retried.
func nativeCodexStart(t *testing.T, s *server, body string) (string, *url.URL) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := oauthCall(s, "POST", "/api/oauth/codex", body)
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
		if w.Code == http.StatusBadGateway && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("start native codex: %d %s", w.Code, w.Body)
	}
}

// nativeCodexIdentity describes the claims of a mocked native ID token.
type nativeCodexIdentity struct {
	Subject   string
	Workspace string
	Plan      string
	Email     string
	Nonce     string // empty omits the nonce claim
}

func nativeCodexIDToken(t *testing.T, key *rsa.PrivateKey, identity nativeCodexIdentity) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss":   openAIIssuer,
		"sub":   identity.Subject,
		"aud":   codexNativeClientID,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"email": identity.Email,
	}
	if identity.Nonce != "" {
		claims["nonce"] = identity.Nonce
	}
	if identity.Workspace != "" || identity.Plan != "" {
		claims["https://api.openai.com/auth"] = map[string]any{
			"chatgpt_account_id": identity.Workspace,
			"chatgpt_plan_type":  identity.Plan,
		}
	}
	return identitySign(t, key, identityHeader(identityTestKid, "RS256"), claims)
}

// nativeCodexExchange installs a mock transport for one native authorization
// code exchange. It asserts the native contract (static public client, PKCE,
// exact redirect, no SIWC parameters) and answers JWKS with the matching key.
func nativeCodexExchange(t *testing.T, s *server, key *rsa.PrivateKey, session oauthSession, code string, identity nativeCodexIdentity, calls *atomic.Int32) {
	t.Helper()
	jwks := map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": identityTestKid, "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}}
	s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case codexNativeTokenURL:
			calls.Add(1)
			r.ParseForm()
			form := r.Form
			if form.Get("grant_type") != "authorization_code" || form.Get("client_id") != codexNativeClientID || form.Get("code") != code || form.Get("code_verifier") != session.Verifier || form.Get("redirect_uri") != session.RedirectURI {
				t.Error("wrong native token exchange")
			}
			if form.Has("resource") || form.Has("scope") {
				t.Error("native exchange carried SIWC parameters")
			}
			return oauthReply(200, map[string]any{
				"access_token": "provider-access", "refresh_token": "provider-refresh",
				"id_token":   nativeCodexIDToken(t, key, identity),
				"expires_in": 3600, "token_type": "Bearer", "scope": codexNativeScope,
			}), nil
		case openAIJWKSURL:
			return oauthReply(200, jwks), nil
		default:
			t.Errorf("unexpected outbound: %s", r.URL)
			return oauthReply(500, nil), nil
		}
	})
}

// nativeCodexSubmit posts a callback for a native session. An empty clientID
// models the upstream CLI callback, which is not required to name the static
// client.
func nativeCodexSubmit(s *server, id string, session oauthSession, code, clientID string) *httptest.ResponseRecorder {
	q := url.Values{"code": {code}, "state": {session.State}}
	if clientID != "" {
		q.Set("client_id", clientID)
	}
	body, _ := json.Marshal(map[string]string{"redirectUrl": session.RedirectURI + "?" + q.Encode()})
	return oauthCall(s, "POST", "/api/oauth/sessions/"+id+"/callback", string(body))
}

func nativeCodexKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestNativeCodexStartContract(t *testing.T) {
	s := oauthServer(t)
	id, u := nativeCodexStart(t, s, `{"authMode":"codex"}`)
	q := u.Query()
	session := s.oauth[id]
	if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != "/oauth/authorize" {
		t.Fatalf("bad native authorize endpoint: %s", u)
	}
	if session.Provider != "codex" || session.AuthMode != "codex" || session.ClientID != codexNativeClientID {
		t.Fatalf("bad native session: %+v", session)
	}
	if session.RedirectURI != "http://127.0.0.1:1455/auth/callback" {
		t.Fatalf("native callback = %q", session.RedirectURI)
	}
	if q.Get("client_id") != codexNativeClientID || q.Get("response_type") != "code" || q.Get("scope") != codexNativeScope ||
		q.Get("code_challenge_method") != "S256" || q.Get("state") != session.State || q.Get("redirect_uri") != session.RedirectURI {
		t.Fatalf("bad native authorization contract: %s", u)
	}
	if q.Get("nonce") != session.Nonce || q.Get("id_token_add_organizations") != "true" ||
		q.Get("codex_cli_simplified_flow") != "true" || q.Get("originator") != codexNativeOriginator {
		t.Fatalf("missing native extension parameters: %s", u)
	}
	digest := sha256.Sum256([]byte(session.Verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(digest[:]) {
		t.Fatal("wrong PKCE challenge")
	}
	for _, rejected := range []string{"resource", "ext_agent_host_id", "agent_name_hint", "prompt", "login_hint", "id_token_hint"} {
		if q.Has(rejected) {
			t.Fatalf("native authorization carried %q: %s", rejected, u)
		}
	}
}

func TestNativeCodexModeSelection(t *testing.T) {
	t.Run("omitted mode defaults to native Codex", func(t *testing.T) {
		s := oauthServer(t)
		id, u := nativeCodexStart(t, s, "")
		session := s.oauth[id]
		q := u.Query()
		if session.AuthMode != "codex" || session.ClientID != codexNativeClientID || u.Path != "/oauth/authorize" {
			t.Fatalf("omitted mode did not default to native Codex: %+v %s", session, u)
		}
		if session.RedirectURI != "http://127.0.0.1:1455/auth/callback" || q.Get("codex_cli_simplified_flow") != "true" || q.Has("agent_name_hint") || q.Has("resource") {
			t.Fatalf("default registration is not the native flow: %s", u)
		}
	})
	t.Run("experimental ChatGPT is refused", func(t *testing.T) {
		s := oauthServer(t)
		w := oauthCall(s, "POST", "/api/oauth/codex", `{"authMode":"chatgpt"}`)
		if w.Code != 400 || len(s.oauth) != 0 || len(s.callbacks) != 0 {
			t.Fatalf("removed login accepted: %d", w.Code)
		}
	})
	t.Run("unsupported mode is refused", func(t *testing.T) {
		s := oauthServer(t)
		if w := oauthCall(s, "POST", "/api/oauth/codex", `{"authMode":"oauth"}`); w.Code != 400 {
			t.Fatalf("unsupported mode = %d %s; want 400", w.Code, w.Body)
		}
		if len(s.oauth) != 0 {
			t.Fatal("refused mode opened a sign-in session")
		}
	})
	t.Run("mode is refused for Claude", func(t *testing.T) {
		s := oauthServer(t)
		if w := oauthCall(s, "POST", "/api/oauth/claude", `{"authMode":"codex"}`); w.Code != 400 {
			t.Fatalf("Claude mode = %d %s; want 400", w.Code, w.Body)
		}
		if len(s.oauth) != 0 {
			t.Fatal("refused Claude mode opened a sign-in session")
		}
	})
}

func TestNativeCodexExchangeStoresVerifiedIdentity(t *testing.T) {
	s := oauthServer(t)
	key := nativeCodexKey(t)
	id, _ := nativeCodexStart(t, s, `{"authMode":"codex"}`)
	session := s.oauth[id]
	var calls atomic.Int32
	nativeCodexExchange(t, s, key, session, "native-code", nativeCodexIdentity{
		Subject: "user-1", Workspace: "ws-1", Plan: "pro", Email: "dev@example.invalid", Nonce: session.Nonce,
	}, &calls)

	w := nativeCodexSubmit(s, id, session, "native-code", "")
	if w.Code != 200 {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("token exchange calls = %d; want 1", calls.Load())
	}
	accounts := s.store.snapshot().Accounts
	if len(accounts) != 1 {
		t.Fatalf("stored accounts = %d; want 1", len(accounts))
	}
	got := accounts[0]
	if got.Provider != "codex" || got.AuthMode != "codex" || got.ClientID != codexNativeClientID {
		t.Fatalf("native record not stored as Codex: %+v", got)
	}
	if got.AccountID != "ws-1" || got.Subject != "user-1" || got.Plan != "pro" || got.Email != "dev@example.invalid" {
		t.Fatalf("native identity not stored: %+v", got)
	}
	if got.AccessToken != "provider-access" || got.RefreshToken != "provider-refresh" || got.IDToken == "" {
		t.Fatalf("native tokens not stored: %+v", got)
	}
	if w = oauthCall(s, "GET", "/api/oauth/sessions/"+id, ""); !strings.Contains(w.Body.String(), "connected") {
		t.Fatal(w.Body.String())
	}
	// The authorization code cannot be replayed.
	if w = nativeCodexSubmit(s, id, session, "native-code", ""); w.Code == 200 || calls.Load() != 1 {
		t.Fatal("replayed native code")
	}
}

func TestNativeCodexIdentityRegressions(t *testing.T) {
	t.Run("missing workspace is refused", func(t *testing.T) {
		s := oauthServer(t)
		key := nativeCodexKey(t)
		id, _ := nativeCodexStart(t, s, `{"authMode":"codex"}`)
		session := s.oauth[id]
		var calls atomic.Int32
		nativeCodexExchange(t, s, key, session, "native-code", nativeCodexIdentity{
			Subject: "user-1", Plan: "pro", Email: "dev@example.invalid", Nonce: session.Nonce,
		}, &calls)
		if w := nativeCodexSubmit(s, id, session, "native-code", ""); w.Code == 200 {
			t.Fatalf("workspace-less identity accepted: %s", w.Body)
		}
		if calls.Load() != 1 || len(s.store.snapshot().Accounts) != 0 {
			t.Fatal("workspace-less identity was stored")
		}
	})

	t.Run("unverified access token is not identity", func(t *testing.T) {
		s := oauthServer(t)
		key := nativeCodexKey(t)
		id, _ := nativeCodexStart(t, s, `{"authMode":"codex"}`)
		session := s.oauth[id]
		now := time.Now()
		access := identitySign(t, key, identityHeader(identityTestKid, "RS256"), map[string]any{
			"iss": openAIIssuer, "sub": "user-1", "aud": codexNativeClientID,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "ws-1", "chatgpt_plan_type": "pro"},
		})
		s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
			switch r.URL.String() {
			case codexNativeTokenURL:
				return oauthReply(200, map[string]any{"access_token": access, "refresh_token": "provider-refresh", "expires_in": 3600}), nil
			default:
				t.Errorf("unexpected outbound: %s", r.URL)
				return oauthReply(500, nil), nil
			}
		})
		if w := nativeCodexSubmit(s, id, session, "native-code", ""); w.Code == 200 {
			t.Fatalf("unverified access token mistaken for identity: %s", w.Body)
		}
		if len(s.store.snapshot().Accounts) != 0 {
			t.Fatal("unverified access token identity was stored")
		}
	})

	t.Run("mismatched nonce is refused", func(t *testing.T) {
		s := oauthServer(t)
		key := nativeCodexKey(t)
		id, _ := nativeCodexStart(t, s, `{"authMode":"codex"}`)
		session := s.oauth[id]
		var calls atomic.Int32
		nativeCodexExchange(t, s, key, session, "native-code", nativeCodexIdentity{
			Subject: "user-1", Workspace: "ws-1", Nonce: "a-different-nonce",
		}, &calls)
		if w := nativeCodexSubmit(s, id, session, "native-code", ""); w.Code == 200 {
			t.Fatalf("mismatched nonce accepted: %s", w.Body)
		}
		if calls.Load() != 1 || len(s.store.snapshot().Accounts) != 0 {
			t.Fatal("mismatched nonce was stored")
		}
	})

	t.Run("missing nonce is refused", func(t *testing.T) {
		s := oauthServer(t)
		key := nativeCodexKey(t)
		id, _ := nativeCodexStart(t, s, `{"authMode":"codex"}`)
		session := s.oauth[id]
		var calls atomic.Int32
		nativeCodexExchange(t, s, key, session, "native-code", nativeCodexIdentity{
			Subject: "user-1", Workspace: "ws-1",
		}, &calls)
		if w := nativeCodexSubmit(s, id, session, "native-code", ""); w.Code == 200 {
			t.Fatalf("nonce-less native token accepted: %s", w.Body)
		}
		if calls.Load() != 1 || len(s.store.snapshot().Accounts) != 0 {
			t.Fatal("nonce-less native identity was stored")
		}
	})
}

func TestNativeCodexCallbackRegressions(t *testing.T) {
	t.Run("callback cannot substitute a client", func(t *testing.T) {
		s := oauthServer(t)
		id, _ := nativeCodexStart(t, s, `{"authMode":"codex"}`)
		session := s.oauth[id]
		s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
			t.Error("client substitution reached the network")
			return oauthReply(500, nil), nil
		})
		w := nativeCodexSubmit(s, id, session, "native-code", "oaiapp_attacker")
		if w.Code == 200 {
			t.Fatalf("substituted client accepted: %s", w.Body)
		}
		if len(s.store.snapshot().Accounts) != 0 {
			t.Fatal("substituted client stored an account")
		}
	})

	t.Run("wrong callback address is refused", func(t *testing.T) {
		s := oauthServer(t)
		id, _ := nativeCodexStart(t, s, `{"authMode":"codex"}`)
		session := s.oauth[id]
		s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
			t.Error("wrong callback reached the network")
			return oauthReply(500, nil), nil
		})
		for _, target := range []string{
			"http://localhost:1455/auth/callback",
			"http://127.0.0.1:1455/wrong",
			"http://127.0.0.1:1456/auth/callback",
		} {
			body, _ := json.Marshal(map[string]string{"redirectUrl": target + "?" + url.Values{"code": {"code"}, "state": {session.State}}.Encode()})
			if w := oauthCall(s, "POST", "/api/oauth/sessions/"+id+"/callback", string(body)); w.Code == 200 {
				t.Fatalf("wrong callback %q accepted", target)
			}
		}
		if len(s.store.snapshot().Accounts) != 0 {
			t.Fatal("wrong callback stored an account")
		}
	})
}

func TestNativeCodexFixedCallbackPort(t *testing.T) {
	s := oauthServer(t)
	id, _ := nativeCodexStart(t, s, `{"authMode":"codex"}`)
	session := s.oauth[id]
	// A second native sign-in cannot share the fixed callback port.
	w := oauthCall(s, "POST", "/api/oauth/codex", `{"authMode":"codex"}`)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "1455") {
		t.Fatalf("port-busy start = %d %s; want a helpful 502", w.Code, w.Body)
	}
	if len(s.oauth) != 1 {
		t.Fatal("port-busy start opened another session")
	}
	// The pending session rejects a callback that does not match its exact
	// registered loopback address.
	s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		t.Error("mismatched callback reached the network")
		return oauthReply(500, nil), nil
	})
	body, _ := json.Marshal(map[string]string{"redirectUrl": "http://localhost:1455/auth/callback?" + url.Values{"code": {"code"}, "state": {session.State}}.Encode()})
	if w = oauthCall(s, "POST", "/api/oauth/sessions/"+id+"/callback", string(body)); w.Code == 200 {
		t.Fatalf("host-mismatched callback accepted: %s", w.Body)
	}
}

func TestNativeCodexSelectedReconnectRenewsInPlace(t *testing.T) {
	s := oauthServer(t)
	key := nativeCodexKey(t)
	old := storedAccount{
		ID: "native-record", Provider: "codex", AuthMode: "codex", ClientID: codexNativeClientID,
		Label: "Work Codex", Email: "old@example.invalid", AccountID: "ws-1", Subject: "user-1",
		AccessToken: "old-access", RefreshToken: "old-refresh",
		CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Disabled: true,
	}
	nativeSeed(t, s, old)
	s.catalogMu.Lock()
	s.catalogs[old.ID] = catalogCache{}
	s.catalogMu.Unlock()
	s.quotaMu.Lock()
	s.quotas[old.ID] = quotaCache{}
	s.quotaMu.Unlock()

	// No authMode field: the saved account's method is used.
	id, u := nativeCodexStart(t, s, `{"accountId":"native-record"}`)
	q := u.Query()
	session := s.oauth[id]
	if session.AccountID != old.ID || session.AuthMode != "codex" || session.ClientID != codexNativeClientID {
		t.Fatalf("native reconnect session = %+v", session)
	}
	if q.Get("client_id") != codexNativeClientID || q.Get("nonce") == "" || q.Get("originator") != codexNativeOriginator {
		t.Fatalf("native reconnect authorization = %s", u)
	}
	for _, rejected := range []string{"resource", "id_token_hint", "ext_agent_host_id", "agent_name_hint", "prompt"} {
		if q.Has(rejected) {
			t.Fatalf("native reconnect carried %q: %s", rejected, u)
		}
	}

	var calls atomic.Int32
	nativeCodexExchange(t, s, key, session, "native-code", nativeCodexIdentity{
		Subject: "user-1", Workspace: "ws-1", Plan: "plus", Email: "new@example.invalid", Nonce: session.Nonce,
	}, &calls)
	if w := nativeCodexSubmit(s, id, session, "native-code", ""); w.Code != 200 {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("token exchange calls = %d; want 1", calls.Load())
	}
	accounts := s.store.snapshot().Accounts
	if len(accounts) != 1 {
		t.Fatalf("stored accounts = %d; want 1 without a duplicate", len(accounts))
	}
	got := accounts[0]
	if got.ID != old.ID || !got.CreatedAt.Equal(old.CreatedAt) || got.Label != old.Label || !got.Disabled {
		t.Fatalf("reconnect did not preserve record metadata: %+v", got)
	}
	if got.ClientID != codexNativeClientID || got.AccountID != "ws-1" || got.Subject != "user-1" || got.AccessToken != "provider-access" || got.RefreshToken != "provider-refresh" {
		t.Fatalf("reconnect did not store the fresh credential: %+v", got)
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

func TestNativeCodexLegacyReconnect(t *testing.T) {
	// An imported record has a workspace but no verified user subject. It must
	// never be rebound to whichever user signs in next, even the same one.
	for _, freshUser := range []string{"user-1", "user-2"} {
		t.Run("workspace-only legacy record refuses "+freshUser, func(t *testing.T) {
			s := oauthServer(t)
			old := storedAccount{
				ID: "legacy-record", Provider: "codex", AuthMode: "codex",
				Label: "Imported Codex", Email: "old@example.invalid", AccountID: "ws-1",
				AccessToken: "old-access", RefreshToken: "old-refresh",
				CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			}
			nativeSeed(t, s, old)
			before := s.store.snapshot()
			w := oauthCall(s, "POST", "/api/oauth/codex", `{"accountId":"legacy-record"}`)
			if w.Code != 400 || !strings.Contains(w.Body.String(), "Add the account again") {
				t.Fatalf("unbound legacy reconnect = %d %s; want 400 instructing to add again", w.Code, w.Body)
			}
			if len(s.oauth) != 0 || len(s.callbacks) != 0 {
				t.Fatal("refused legacy reconnect opened a sign-in session")
			}
			if !reflect.DeepEqual(before, s.store.snapshot()) {
				t.Fatal("refused legacy reconnect changed the store")
			}
		})
	}

	t.Run("foreign client is refused", func(t *testing.T) {
		s := oauthServer(t)
		old := storedAccount{ID: "foreign-record", Provider: "codex", AuthMode: "codex", ClientID: "oaiapp_foreign", AccountID: "ws-1", AccessToken: "old", RefreshToken: "old"}
		nativeSeed(t, s, old)
		before := s.store.snapshot()
		if w := oauthCall(s, "POST", "/api/oauth/codex", `{"accountId":"foreign-record"}`); w.Code != 400 {
			t.Fatalf("foreign client reconnect = %d %s; want 400", w.Code, w.Body)
		}
		if len(s.oauth) != 0 || len(s.callbacks) != 0 {
			t.Fatal("refused registration opened a sign-in session")
		}
		if !reflect.DeepEqual(before, s.store.snapshot()) {
			t.Fatal("refused registration changed the store")
		}
	})
}

func TestNativeCodexReconnectRejectsIdentityMismatch(t *testing.T) {
	cases := []struct {
		name   string
		stored nativeCodexIdentity
		fresh  nativeCodexIdentity
	}{
		{"different workspace", nativeCodexIdentity{Subject: "user-1", Workspace: "ws-1"}, nativeCodexIdentity{Subject: "user-1", Workspace: "ws-2"}},
		{"different user in the same workspace", nativeCodexIdentity{Subject: "user-1", Workspace: "ws-1"}, nativeCodexIdentity{Subject: "user-2", Workspace: "ws-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := oauthServer(t)
			key := nativeCodexKey(t)
			old := storedAccount{
				ID: "native-record", Provider: "codex", AuthMode: "codex", ClientID: codexNativeClientID,
				Label: "Work Codex", AccountID: tc.stored.Workspace, Subject: tc.stored.Subject,
				AccessToken: "old-access", RefreshToken: "old-refresh",
				CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
			}
			nativeSeed(t, s, old)
			before := s.store.snapshot()

			id, _ := nativeCodexStart(t, s, `{"accountId":"native-record"}`)
			session := s.oauth[id]
			var calls atomic.Int32
			fresh := tc.fresh
			fresh.Nonce = session.Nonce
			nativeCodexExchange(t, s, key, session, "native-code", fresh, &calls)
			if w := nativeCodexSubmit(s, id, session, "native-code", ""); w.Code == 200 {
				t.Fatalf("mismatched identity was accepted: %s", w.Body)
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

func TestNativeCodexReconnectRejectsModeSwitch(t *testing.T) {
	cases := []struct {
		name     string
		stored   storedAccount
		authMode string
	}{
		{
			"native record switched to chatgpt",
			storedAccount{ID: "native-record", Provider: "codex", AuthMode: "codex", ClientID: codexNativeClientID, AccountID: "ws-1", Subject: "user-1", AccessToken: "old", RefreshToken: "old"},
			"chatgpt",
		},
		{
			"chatgpt record switched to codex",
			storedAccount{ID: "chatgpt-record", Provider: "codex", AuthMode: "chatgpt", ClientID: "oaiapp_test", AccountID: "subject", AccessToken: "old", RefreshToken: "old"},
			"codex",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := oauthServer(t)
			nativeSeed(t, s, tc.stored)
			before := s.store.snapshot()
			body, _ := json.Marshal(map[string]string{"accountId": tc.stored.ID, "authMode": tc.authMode})
			if w := oauthCall(s, "POST", "/api/oauth/codex", string(body)); w.Code != 400 {
				t.Fatalf("mode switch = %d %s; want 400", w.Code, w.Body)
			}
			if len(s.oauth) != 0 {
				t.Fatal("refused mode switch opened a sign-in session")
			}
			if !reflect.DeepEqual(before, s.store.snapshot()) {
				t.Fatal("refused mode switch changed the store")
			}
		})
	}
}

func TestNativeCodexUnselectedDedupe(t *testing.T) {
	signIn := func(old storedAccount) []storedAccount {
		s := oauthServer(t)
		key := nativeCodexKey(t)
		nativeSeed(t, s, old)
		id, _ := nativeCodexStart(t, s, `{"authMode":"codex"}`)
		session := s.oauth[id]
		var calls atomic.Int32
		nativeCodexExchange(t, s, key, session, "native-code", nativeCodexIdentity{
			Subject: "user-1", Workspace: "ws-1", Plan: "pro", Email: "shared@example.invalid", Nonce: session.Nonce,
		}, &calls)
		if w := nativeCodexSubmit(s, id, session, "native-code", ""); w.Code != 200 {
			t.Fatalf("callback: %d %s", w.Code, w.Body)
		}
		return s.store.snapshot().Accounts
	}

	t.Run("same workspace and subject renews the record", func(t *testing.T) {
		old := storedAccount{
			ID: "native-record", Provider: "codex", AuthMode: "codex", ClientID: codexNativeClientID,
			Label: "Work Codex", AccountID: "ws-1", Subject: "user-1",
			AccessToken: "old-access", RefreshToken: "old-refresh",
			CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		}
		accounts := signIn(old)
		if len(accounts) != 1 {
			t.Fatalf("same-identity sign-in stored %d accounts; want 1", len(accounts))
		}
		got := accounts[0]
		if got.ID != old.ID || got.Label != old.Label || !got.CreatedAt.Equal(old.CreatedAt) || got.AccessToken != "provider-access" {
			t.Fatalf("same-identity sign-in did not renew the record: %+v", got)
		}
	})

	t.Run("legacy record without a subject appends", func(t *testing.T) {
		old := storedAccount{
			ID: "legacy-record", Provider: "codex", AuthMode: "codex", ClientID: codexNativeClientID,
			Label: "Imported Codex", AccountID: "ws-1", AccessToken: "old-access", RefreshToken: "old-refresh",
		}
		accounts := signIn(old)
		if len(accounts) != 2 {
			t.Fatalf("legacy sign-in stored %d accounts; want 2 without a silent user switch", len(accounts))
		}
		if accounts[0].ID != old.ID || accounts[0].AccessToken != "old-access" || accounts[0].Subject != "" {
			t.Fatalf("legacy record was replaced instead of kept: %+v", accounts[0])
		}
		fresh := accounts[1]
		if fresh.ID == old.ID || fresh.Subject != "user-1" || fresh.AccountID != "ws-1" || fresh.AccessToken != "provider-access" {
			t.Fatalf("fresh native identity was not appended: %+v", fresh)
		}
	})
}

func TestNativeCodexUnselectedDifferentUserAppends(t *testing.T) {
	cases := []struct {
		name  string
		fresh nativeCodexIdentity
	}{
		{"different user in the same workspace", nativeCodexIdentity{Subject: "user-2", Workspace: "ws-1", Email: "shared@example.invalid"}},
		{"same email in a different workspace", nativeCodexIdentity{Subject: "user-1", Workspace: "ws-9", Email: "shared@example.invalid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := oauthServer(t)
			key := nativeCodexKey(t)
			nativeSeed(t, s, storedAccount{
				ID: "native-record", Provider: "codex", AuthMode: "codex", ClientID: codexNativeClientID,
				Email: "shared@example.invalid", AccountID: "ws-1", Subject: "user-1",
				AccessToken: "old-access", RefreshToken: "old-refresh",
			})
			id, _ := nativeCodexStart(t, s, `{"authMode":"codex"}`)
			session := s.oauth[id]
			var calls atomic.Int32
			fresh := tc.fresh
			fresh.Nonce = session.Nonce
			nativeCodexExchange(t, s, key, session, "native-code", fresh, &calls)
			if w := nativeCodexSubmit(s, id, session, "native-code", ""); w.Code != 200 {
				t.Fatalf("callback: %d %s", w.Code, w.Body)
			}
			if accounts := s.store.snapshot().Accounts; len(accounts) != 2 {
				t.Fatalf("different identity = %d accounts; want 2", len(accounts))
			}
		})
	}
}

func TestNativeCodexAccountsExposeAuthModeAndReconnectable(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s,
		storedAccount{ID: "native-full", Provider: "codex", AuthMode: "codex", AccountID: "ws-1", Subject: "user-1", AccessToken: "access", Disabled: true},
		storedAccount{ID: "native-workspace", Provider: "codex", AuthMode: "codex", AccountID: "ws-1", AccessToken: "access", Disabled: true},
		storedAccount{ID: "native-subject", Provider: "codex", AuthMode: "codex", Subject: "user-1", AccessToken: "access", Disabled: true},
		storedAccount{ID: "native-blank", Provider: "codex", AuthMode: "codex", AccessToken: "access", Disabled: true},
		storedAccount{ID: "chatgpt", Provider: "codex", AuthMode: "chatgpt", ClientID: "oaiapp_test", AccountID: "subject", AccessToken: "access", Disabled: true},
		storedAccount{ID: "api-key", Provider: "claude", AuthMode: "api_key", AccessToken: "key", Disabled: true},
	)
	s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		t.Errorf("disabled accounts queried the provider: %s", r.URL)
		return oauthReply(500, nil), nil
	})
	byID := map[string]Account{}
	for _, a := range s.accounts(context.Background()) {
		byID[a.ID] = a
	}
	want := map[string]struct {
		mode          string
		reconnectable bool
	}{
		"native-full":      {"codex", true},
		"native-workspace": {"codex", false},
		"native-subject":   {"codex", false},
		"native-blank":     {"codex", false},
		"chatgpt":          {"chatgpt", false},
		"api-key":          {"api_key", false},
	}
	for id, expected := range want {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("account %s missing from the state response", id)
		}
		if got.AuthMode != expected.mode || got.Reconnectable != expected.reconnectable {
			t.Errorf("%s authMode/reconnectable = %q/%v; want %q/%v", id, got.AuthMode, got.Reconnectable, expected.mode, expected.reconnectable)
		}
	}
}

// TestVerifyNativeWorkspaceClaims pins that the workspace identity is read
// from the verified ID token only.
func TestVerifyNativeWorkspaceClaims(t *testing.T) {
	fixture := newIdentityFixture(t)
	claims := identityClaims(codexNativeClientID, identityTestNonce)
	claims["https://api.openai.com/auth"] = map[string]any{"chatgpt_account_id": " ws-1 ", "chatgpt_plan_type": " pro "}
	token := identitySign(t, fixture.key, identityHeader(fixture.kid, "RS256"), claims)
	identity, err := identityServer(identityJWKSClient(fixture.jwks)).verifyIDToken(context.Background(), token, codexNativeClientID, identityTestNonce)
	if err != nil {
		t.Fatalf("native ID token rejected: %v", err)
	}
	if identity.WorkspaceID != "ws-1" || identity.PlanType != "pro" || identity.Subject != identityTestSubject {
		t.Fatalf("workspace identity = %+v", identity)
	}
}

// TestNativeNonceIsStrictForEveryFlow pins the policy shared by both modes:
// vrouter requests a nonce, so an omitted claim is refused rather than
// accepted on the assumption the provider dropped it. The native flow rejects
// a missing nonce end to end in TestNativeCodexIdentityRegressions.
func TestNativeNonceIsStrictForEveryFlow(t *testing.T) {
	fixture := newIdentityFixture(t)
	verifier := identityServer(identityJWKSClient(fixture.jwks))
	claims := identityClaims(identityTestClientID, identityTestNonce)
	delete(claims, "nonce")
	token := identitySign(t, fixture.key, identityHeader(fixture.kid, "RS256"), claims)
	if _, err := verifier.verifyIDToken(context.Background(), token, identityTestClientID, identityTestNonce); err == nil {
		t.Fatal("token without a nonce was accepted")
	}
	claims["nonce"] = "a-different-nonce"
	token = identitySign(t, fixture.key, identityHeader(fixture.kid, "RS256"), claims)
	if _, err := verifier.verifyIDToken(context.Background(), token, identityTestClientID, identityTestNonce); err == nil {
		t.Fatal("mismatched nonce was accepted")
	}
}
