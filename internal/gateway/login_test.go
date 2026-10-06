package gateway

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/oauth2"
)

// ---------------------------------------------------------------------------
// Mock OIDC issuer

// mockIDP is a minimal OpenID Connect provider: discovery, authorization,
// token and JWKS endpoints. It verifies PKCE, the client secret and the
// authorization code, then signs ID tokens with an RSA key whose public half
// it publishes. Tests can override individual claims to exercise rejection
// paths.
type mockIDP struct {
	t      *testing.T
	server *httptest.Server
	key    *rsa.PrivateKey
	other  *rsa.PrivateKey
	kid    string
	seq    atomic.Uint64

	mu          sync.Mutex
	codes       map[string]mockIDPCode
	claims      map[string]any
	useOtherKey bool
	wantSecret  string
	tokenCalls  int
	lastToken   url.Values
	// issuerOverride replaces the httptest URL as the issuer, so tests can
	// exercise exact issuer spellings such as a trailing slash.
	issuerOverride string
}

type mockIDPCode struct {
	redirectURI string
	nonce       string
	challenge   string
	clientID    string
	claims      map[string]any
}

func newMockIDP(t *testing.T) *mockIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockIDP{t: t, key: key, other: other, kid: "test-key", codes: map[string]mockIDPCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeMockJSON(w, 200, map[string]any{
			"issuer":                                m.issuer(),
			"authorization_endpoint":                m.server.URL + "/authorize",
			"token_endpoint":                        m.server.URL + "/token",
			"jwks_uri":                              m.server.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"scopes_supported":                      []string{"openid", "profile", "email"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})
	mux.HandleFunc("/authorize", m.handleAuthorize)
	mux.HandleFunc("/token", m.handleToken)
	mux.HandleFunc("/keys", m.handleKeys)
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockIDP) issuer() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.issuerOverride != "" {
		return m.issuerOverride
	}
	return m.server.URL
}

func (m *mockIDP) setIssuer(issuer string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.issuerOverride = issuer
}

func (m *mockIDP) setClaims(claims map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claims = claims
}

func (m *mockIDP) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("redirect_uri") == "" {
		writeMockJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	code := fmt.Sprintf("code-%d", m.seq.Add(1))
	claims := map[string]any{}
	m.mu.Lock()
	for name, value := range m.claims {
		claims[name] = value
	}
	m.codes[code] = mockIDPCode{
		redirectURI: query.Get("redirect_uri"),
		nonce:       query.Get("nonce"),
		challenge:   query.Get("code_challenge"),
		clientID:    query.Get("client_id"),
		claims:      claims,
	}
	m.mu.Unlock()
	callback := query.Get("redirect_uri")
	separator := "?"
	if strings.Contains(callback, "?") {
		separator = "&"
	}
	http.Redirect(w, r, callback+separator+"code="+url.QueryEscape(code)+"&state="+url.QueryEscape(query.Get("state")), http.StatusFound)
}

func (m *mockIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeMockJSON(w, 400, map[string]string{"error": "invalid_request"})
		return
	}
	m.mu.Lock()
	m.tokenCalls++
	m.lastToken = r.PostForm
	useOther := m.useOtherKey
	wantSecret := m.wantSecret
	m.mu.Unlock()

	clientID, secret, _ := r.BasicAuth()
	if clientID == "" {
		clientID = r.PostForm.Get("client_id")
		secret = r.PostForm.Get("client_secret")
	}
	if wantSecret != "" && secret != wantSecret {
		writeMockJSON(w, 401, map[string]string{"error": "invalid_client"})
		return
	}
	m.mu.Lock()
	entry, ok := m.codes[r.PostForm.Get("code")]
	delete(m.codes, r.PostForm.Get("code"))
	m.mu.Unlock()
	if !ok || clientID != entry.clientID || r.PostForm.Get("redirect_uri") != entry.redirectURI || r.PostForm.Get("grant_type") != "authorization_code" {
		writeMockJSON(w, 400, map[string]string{"error": "invalid_grant"})
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if entry.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
		writeMockJSON(w, 400, map[string]string{"error": "invalid_grant"})
		return
	}
	claims := map[string]any{
		"iss":   m.issuer(),
		"aud":   entry.clientID,
		"sub":   "subject-1",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"nonce": entry.nonce,
	}
	for name, value := range entry.claims {
		if value == nil {
			delete(claims, name)
			continue
		}
		claims[name] = value
	}
	writeMockJSON(w, 200, map[string]any{
		"access_token": "access-token",
		"token_type":   "Bearer",
		"id_token":     m.sign(claims, useOther),
	})
}

func (m *mockIDP) handleKeys(w http.ResponseWriter, r *http.Request) {
	public := m.key.Public().(*rsa.PublicKey)
	writeMockJSON(w, 200, map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": m.kid,
		"n":   base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(public.E)).Bytes()),
	}}})
}

func (m *mockIDP) sign(claims map[string]any, useOther bool) string {
	key := m.key
	if useOther {
		key = m.other
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": m.kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		m.t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func writeMockJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// ---------------------------------------------------------------------------
// Harness

type loginTestEnv struct {
	t      *testing.T
	idp    *mockIDP
	auth   *loginAuth
	app    *httptest.Server
	mux    *http.ServeMux
	client *http.Client
}

func newLoginTestEnv(t *testing.T, mappings []RoleMappingConfig, adminToken string) *loginTestEnv {
	t.Helper()
	return newLoginTestEnvForIDP(t, newMockIDP(t), "vrouter-client", mappings, adminToken)
}

// newLoginTestEnvForIDP builds the sign-in surface around an existing mock
// identity provider, so tests can share one IdP across client configurations
// or give it a custom issuer.
func newLoginTestEnvForIDP(t *testing.T, idp *mockIDP, clientID string, mappings []RoleMappingConfig, adminToken string) *loginTestEnv {
	t.Helper()
	idp.mu.Lock()
	idp.wantSecret = "client-secret"
	idp.mu.Unlock()
	mux := http.NewServeMux()
	app := httptest.NewServer(mux)
	t.Cleanup(app.Close)
	auth, err := newLoginAuth(IdentityConfig{
		PublicURL: app.URL,
		Providers: []OIDCProviderConfig{{
			ID:              "test-idp",
			Name:            "Test IdP",
			Issuer:          idp.issuer(),
			ClientID:        clientID,
			ClientSecretEnv: "VROUTER_TEST_IDP_SECRET",
			clientSecret:    "client-secret",
			RoleMappings:    mappings,
		}},
	}, adminToken)
	if err != nil {
		t.Fatalf("newLoginAuth: %v", err)
	}
	t.Cleanup(func() { _ = auth.Close() })
	auth.register(mux)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &loginTestEnv{
		t:    t,
		idp:  idp,
		auth: auth,
		app:  app,
		mux:  mux,
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func getResponse(t *testing.T, client *http.Client, target string) *http.Response {
	t.Helper()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	return resp
}

func responseJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var value T
	if err := json.NewDecoder(resp.Body).Decode(&value); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return value
}

func cookieByName(resp *http.Response, name string) *http.Cookie {
	for _, cookie := range resp.Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

// startLogin begins a sign-in and asserts the authorization request and the
// browser-bound transaction. It returns the authorization URL and handle.
func (e *loginTestEnv) startLogin(t *testing.T) (*url.URL, string) {
	t.Helper()
	resp := getResponse(t, e.client, e.app.URL+"/auth/login/test-idp")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login start: %d %s", resp.StatusCode, body)
	}
	location := resp.Header.Get("Location")
	authURL, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	query := authURL.Query()
	if query.Get("client_id") != e.auth.providers["test-idp"].clientID || query.Get("response_type") != "code" || query.Get("redirect_uri") != e.app.URL+"/auth/callback/test-idp" {
		t.Fatalf("authorization URL parameters wrong: %s", location)
	}
	if !strings.Contains(query.Get("scope"), "openid") || query.Get("state") == "" || query.Get("nonce") == "" || query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization URL missing required parameters: %s", location)
	}
	transactionCookie := cookieByName(resp, loginStateCookie)
	if transactionCookie == nil || transactionCookie.Value == "" {
		t.Fatal("login transaction cookie missing")
	}
	if !transactionCookie.HttpOnly || transactionCookie.SameSite != http.SameSiteLaxMode || transactionCookie.Path != "/" {
		t.Fatalf("login transaction cookie flags wrong: %+v", transactionCookie)
	}
	e.auth.mu.Lock()
	transaction, ok := e.auth.pending[transactionCookie.Value]
	e.auth.mu.Unlock()
	if !ok || transaction.providerID != "test-idp" || transaction.state != query.Get("state") || transaction.nonce != query.Get("nonce") {
		t.Fatal("stored login transaction does not match the redirect")
	}
	if oauth2.S256ChallengeFromVerifier(transaction.verifier) != query.Get("code_challenge") {
		t.Fatal("PKCE challenge does not match the stored verifier")
	}
	return authURL, transactionCookie.Value
}

// idpRedirect plays the identity provider: it authorizes the request and
// returns the callback URL the browser would be sent to next.
func (e *loginTestEnv) idpRedirect(t *testing.T, authURL string) string {
	t.Helper()
	resp := getResponse(t, noRedirectClient(), authURL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("idp authorize: %d %s", resp.StatusCode, body)
	}
	return resp.Header.Get("Location")
}

func (e *loginTestEnv) completeLogin(t *testing.T) (*http.Response, string) {
	t.Helper()
	authURL, _ := e.startLogin(t)
	callback := e.idpRedirect(t, authURL.String())
	return getResponse(t, e.client, callback), callback
}

// ---------------------------------------------------------------------------
// Tests

func TestOIDCLoginFullFlow(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "vrouter-admins", Role: "admin"}}, "")
	env.idp.setClaims(map[string]any{
		"groups":         []string{"everyone", "vrouter-admins"},
		"name":           "Ada Lovelace",
		"email":          "ada@example.com",
		"email_verified": true,
	})

	status := responseJSON[loginStatusView](t, getResponse(t, env.client, env.app.URL+"/api/auth"))
	if status.Mode != "oidc" || status.User != nil || len(status.Providers) != 1 {
		t.Fatalf("public auth status wrong: %+v", status)
	}
	if provider := status.Providers[0]; provider.ID != "test-idp" || provider.Name != "Test IdP" || provider.LoginURL != env.app.URL+"/auth/login/test-idp" {
		t.Fatalf("provider view wrong: %+v", provider)
	}

	resp, _ := env.completeLogin(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != env.app.URL+"/" {
		t.Fatalf("callback did not redirect home: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	session := cookieByName(resp, loginSessionCookie)
	if session == nil || session.Value == "" {
		t.Fatal("session cookie missing after login")
	}
	if !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" || session.Secure {
		t.Fatalf("session cookie flags wrong: %+v", session)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == loginStateCookie && cookie.MaxAge >= 0 {
			t.Fatal("login transaction cookie was not cleared")
		}
	}

	status = responseJSON[loginStatusView](t, getResponse(t, env.client, env.app.URL+"/api/auth"))
	if status.User == nil {
		t.Fatal("session did not authenticate /api/auth")
	}
	if status.User.ID != oidcUserID(env.idp.issuer(), "subject-1") || status.User.Name != "Ada Lovelace" || status.User.Email != "ada@example.com" || status.User.Role != "admin" {
		t.Fatalf("user identity wrong: %+v", status.User)
	}

	request := httptest.NewRequest("GET", "http://localhost/api/state", nil)
	request.AddCookie(session)
	if user, ok := env.auth.authenticate(request); !ok || user.ID != status.User.ID || user.Role != "admin" {
		t.Fatalf("session did not authenticate a management request: %+v", user)
	}
}

func TestOIDCLoginDeniesUnmappedUser(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "vrouter-admins", Role: "admin"}}, "")
	env.idp.setClaims(map[string]any{"groups": []string{"employees"}, "email": "nobody@example.com"})
	resp, _ := env.completeLogin(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unmapped user was not denied: %d", resp.StatusCode)
	}
	if cookieByName(resp, loginSessionCookie) != nil {
		t.Fatal("unmapped user received a session")
	}
	status := responseJSON[loginStatusView](t, getResponse(t, env.client, env.app.URL+"/api/auth"))
	if status.User != nil {
		t.Fatalf("unmapped user stayed signed in: %+v", status.User)
	}
}

func TestOIDCLoginRejectsTamperedStateAndReplays(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "")
	env.idp.setClaims(map[string]any{"groups": []string{"admins"}})
	authURL, _ := env.startLogin(t)
	callback := env.idpRedirect(t, authURL.String())

	parsed, err := url.Parse(callback)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("state", "forged-state")
	parsed.RawQuery = query.Encode()
	if resp := getResponse(t, env.client, parsed.String()); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state accepted: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// The transaction is consumed even when validation fails, so the genuine
	// callback cannot be replayed afterwards.
	if resp := getResponse(t, env.client, callback); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("consumed transaction accepted: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// Without the browser-bound cookie the callback is refused outright.
	if resp := getResponse(t, noRedirectClient(), callback); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without transaction cookie accepted: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// A fresh sign-in succeeds, and replaying its callback fails.
	success, _ := env.completeLogin(t)
	if success.StatusCode != http.StatusFound {
		t.Fatalf("fresh login failed: %d", success.StatusCode)
	}
	success.Body.Close()
	if resp := getResponse(t, env.client, callback); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback accepted: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestOIDCLoginRejectsBadTokens(t *testing.T) {
	mappings := []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}
	longAudience := []string{"vrouter-client", "other-client"}
	cases := []struct {
		name        string
		claims      map[string]any
		useOtherKey bool
		wantStatus  int
	}{
		{name: "wrong nonce", claims: map[string]any{"nonce": "not-the-nonce"}, wantStatus: http.StatusUnauthorized},
		{name: "missing nonce", claims: map[string]any{"nonce": nil}, wantStatus: http.StatusUnauthorized},
		{name: "expired token", claims: map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, wantStatus: http.StatusUnauthorized},
		{name: "future not-before", claims: map[string]any{"nbf": time.Now().Add(time.Hour).Unix()}, wantStatus: http.StatusUnauthorized},
		{name: "wrong audience", claims: map[string]any{"aud": "another-client"}, wantStatus: http.StatusUnauthorized},
		{name: "wrong issuer", claims: map[string]any{"iss": "https://evil.example"}, wantStatus: http.StatusUnauthorized},
		{name: "wrong signing key", useOtherKey: true, wantStatus: http.StatusUnauthorized},
		{name: "missing subject", claims: map[string]any{"sub": ""}, wantStatus: http.StatusUnauthorized},
		{name: "multi audience without authorized party", claims: map[string]any{"aud": longAudience}, wantStatus: http.StatusUnauthorized},
		{name: "multi audience with wrong authorized party", claims: map[string]any{"aud": longAudience, "azp": "other-client"}, wantStatus: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newLoginTestEnv(t, mappings, "")
			claims := map[string]any{"groups": []string{"admins"}}
			for name, value := range tc.claims {
				claims[name] = value
			}
			env.idp.setClaims(claims)
			env.idp.mu.Lock()
			env.idp.useOtherKey = tc.useOtherKey
			env.idp.mu.Unlock()
			resp, _ := env.completeLogin(t)
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("token accepted with status %d", resp.StatusCode)
			}
			if cookieByName(resp, loginSessionCookie) != nil {
				t.Fatal("rejected token created a session")
			}
		})
	}
	t.Run("multi audience with authorized party", func(t *testing.T) {
		env := newLoginTestEnv(t, mappings, "")
		env.idp.setClaims(map[string]any{"groups": []string{"admins"}, "aud": longAudience, "azp": "vrouter-client"})
		resp, _ := env.completeLogin(t)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("valid multi-audience token rejected: %d", resp.StatusCode)
		}
	})
	// The library allows five minutes of clock skew on nbf; a token inside
	// that window must still be accepted.
	t.Run("not-before within clock skew", func(t *testing.T) {
		env := newLoginTestEnv(t, mappings, "")
		env.idp.setClaims(map[string]any{"groups": []string{"admins"}, "nbf": time.Now().Add(2 * time.Minute).Unix()})
		resp, _ := env.completeLogin(t)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("token within the nbf leeway rejected: %d", resp.StatusCode)
		}
	})
}

func TestOIDCLoginRejectsTamperedPKCE(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "")
	env.idp.setClaims(map[string]any{"groups": []string{"admins"}})
	authURL, handle := env.startLogin(t)
	callback := env.idpRedirect(t, authURL.String())
	env.auth.mu.Lock()
	transaction := env.auth.pending[handle]
	transaction.verifier = strings.Repeat("b", 43)
	env.auth.pending[handle] = transaction
	env.auth.mu.Unlock()
	resp := getResponse(t, env.client, callback)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("tampered verifier was not refused: %d", resp.StatusCode)
	}
	if cookieByName(resp, loginSessionCookie) != nil {
		t.Fatal("failed exchange created a session")
	}
}

func TestOIDCLoginProviderError(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "")
	_, handle := env.startLogin(t)
	if handle == "" {
		t.Fatal("no transaction handle")
	}
	for _, tc := range []struct {
		query    string
		contains string
	}{
		{"error=access_denied", "cancelled"},
		{"error=server_error&error_description=boom", "failed"},
	} {
		resp := getResponse(t, env.client, env.app.URL+"/auth/callback/test-idp?"+tc.query)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), tc.contains) {
			t.Fatalf("provider error %q: %d %s", tc.query, resp.StatusCode, body)
		}
		if cookieByName(resp, loginSessionCookie) != nil {
			t.Fatal("provider error created a session")
		}
	}
}

func TestOIDCLoginTransactionExpires(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "")
	env.idp.setClaims(map[string]any{"groups": []string{"admins"}})
	authURL, handle := env.startLogin(t)
	callback := env.idpRedirect(t, authURL.String())
	env.auth.mu.Lock()
	transaction := env.auth.pending[handle]
	transaction.expires = time.Now().Add(-time.Minute)
	env.auth.pending[handle] = transaction
	env.auth.mu.Unlock()
	resp := getResponse(t, env.client, callback)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expired transaction accepted: %d", resp.StatusCode)
	}
}

func TestOIDCLoginTransactionLimit(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "")
	env.auth.mu.Lock()
	for i := 0; i < maxLoginTransactions; i++ {
		env.auth.pending[fmt.Sprintf("filler-%d", i)] = loginTransaction{expires: time.Now().Add(time.Hour)}
	}
	env.auth.mu.Unlock()
	resp := getResponse(t, env.client, env.app.URL+"/auth/login/test-idp")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("transaction limit not enforced: %d", resp.StatusCode)
	}
	// Expired transactions are swept instead of filling the limit forever.
	env.auth.mu.Lock()
	env.auth.pending = map[string]loginTransaction{}
	env.auth.mu.Unlock()
	expired := time.Now().Add(-time.Minute)
	env.auth.mu.Lock()
	for i := 0; i < maxLoginTransactions; i++ {
		env.auth.pending[fmt.Sprintf("stale-%d", i)] = loginTransaction{expires: expired}
	}
	env.auth.mu.Unlock()
	resp = getResponse(t, env.client, env.app.URL+"/auth/login/test-idp")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expired transactions were not swept: %d", resp.StatusCode)
	}
}

func TestLoginLogoutAndSessionExpiry(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "")
	env.idp.setClaims(map[string]any{"groups": []string{"admins"}})

	success, _ := env.completeLogin(t)
	session := cookieByName(success, loginSessionCookie)
	success.Body.Close()
	if session == nil {
		t.Fatal("no session cookie")
	}
	request, err := http.NewRequest("POST", env.app.URL+"/api/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	logoutResp, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout: %d", logoutResp.StatusCode)
	}
	var okBody map[string]bool
	if err := json.NewDecoder(logoutResp.Body).Decode(&okBody); err != nil || !okBody["ok"] {
		t.Fatalf("logout body: %v %v", okBody, err)
	}
	if cleared := cookieByName(logoutResp, loginSessionCookie); cleared == nil || cleared.MaxAge >= 0 {
		t.Fatal("logout did not clear the session cookie")
	}
	status := responseJSON[loginStatusView](t, getResponse(t, env.client, env.app.URL+"/api/auth"))
	if status.User != nil {
		t.Fatal("logout left a session")
	}
	stale := httptest.NewRequest("GET", "http://localhost/api/state", nil)
	stale.AddCookie(&http.Cookie{Name: loginSessionCookie, Value: session.Value})
	if _, ok := env.auth.authenticate(stale); ok {
		t.Fatal("revoked session still authenticates")
	}

	// A session that passes its deadline stops authenticating.
	success, _ = env.completeLogin(t)
	session = cookieByName(success, loginSessionCookie)
	success.Body.Close()
	if session == nil {
		t.Fatal("no session cookie after second login")
	}
	env.auth.mu.Lock()
	entry := env.auth.sessions[session.Value]
	entry.expires = time.Now().Add(-time.Second)
	env.auth.sessions[session.Value] = entry
	env.auth.mu.Unlock()
	status = responseJSON[loginStatusView](t, getResponse(t, env.client, env.app.URL+"/api/auth"))
	if status.User != nil {
		t.Fatalf("expired session stayed valid: %+v", status.User)
	}
}

// TestLogoutRejectsCrossOriginAndKeepsSession proves a cross-origin POST to
// /api/logout is refused before any server-side session or browser cookie is
// touched, while a same-origin logout still works.
func TestLogoutRejectsCrossOriginAndKeepsSession(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "")
	env.idp.setClaims(map[string]any{"groups": []string{"admins"}})
	success, _ := env.completeLogin(t)
	session := cookieByName(success, loginSessionCookie)
	success.Body.Close()
	if session == nil {
		t.Fatal("no session cookie")
	}

	post := func(origin, fetchSite string) *http.Response {
		t.Helper()
		request, err := http.NewRequest("POST", env.app.URL+"/api/logout", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(session)
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if fetchSite != "" {
			request.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		resp, err := noRedirectClient().Do(request)
		if err != nil {
			t.Fatalf("POST /api/logout: %v", err)
		}
		return resp
	}

	for _, tc := range []struct {
		name      string
		origin    string
		fetchSite string
	}{
		{name: "cross-site fetch metadata", fetchSite: "cross-site"},
		{name: "mismatched origin", origin: "https://evil.example"},
		{name: "cross-site origin and metadata", origin: "https://evil.example", fetchSite: "cross-site"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(tc.origin, tc.fetchSite)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("cross-origin logout allowed: %d", resp.StatusCode)
			}
			if cookieByName(resp, loginSessionCookie) != nil || cookieByName(resp, loginStateCookie) != nil {
				t.Fatal("rejected logout still cleared cookies")
			}
			// The server-side session must survive a rejected logout.
			request := httptest.NewRequest("GET", "http://localhost/api/auth", nil)
			request.AddCookie(session)
			if user, ok := env.auth.authenticate(request); !ok || user.Role != "admin" {
				t.Fatalf("rejected logout destroyed the session: %+v", user)
			}
		})
	}

	// A same-origin logout still clears the session and both cookies.
	resp := post(env.app.URL, "same-origin")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-origin logout: %d", resp.StatusCode)
	}
	if cleared := cookieByName(resp, loginSessionCookie); cleared == nil || cleared.MaxAge >= 0 {
		t.Fatal("same-origin logout did not clear the session cookie")
	}
	request := httptest.NewRequest("GET", "http://localhost/api/auth", nil)
	request.AddCookie(session)
	if _, ok := env.auth.authenticate(request); ok {
		t.Fatal("same-origin logout left the session valid")
	}
}

func TestLoginIdentityStableAcrossEmailChanges(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "role", Value: "user", Role: "user"}}, "")
	env.idp.setClaims(map[string]any{"role": "user", "email": "first@example.com", "name": "First"})
	success, _ := env.completeLogin(t)
	success.Body.Close()
	first := responseJSON[loginStatusView](t, getResponse(t, env.client, env.app.URL+"/api/auth"))
	wantID := oidcUserID(env.idp.issuer(), "subject-1")
	if first.User == nil || first.User.ID != wantID {
		t.Fatalf("identity wrong: %+v", first.User)
	}
	if !strings.HasPrefix(wantID, "oidc_") || len(wantID) != len("oidc_")+64 {
		t.Fatalf("identity is not the bounded opaque id: %q", wantID)
	}
	request, _ := http.NewRequest("POST", env.app.URL+"/api/logout", nil)
	logout, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	logout.Body.Close()

	env.idp.setClaims(map[string]any{"role": "user", "email": "second@example.com", "name": "Second"})
	success, _ = env.completeLogin(t)
	success.Body.Close()
	second := responseJSON[loginStatusView](t, getResponse(t, env.client, env.app.URL+"/api/auth"))
	if second.User == nil || second.User.ID != first.User.ID || second.User.Email != "second@example.com" {
		t.Fatalf("identity changed with email: %+v", second.User)
	}
}

// TestOIDCUserIDBoundsAndUnambiguity covers the derivation itself: stable,
// bounded regardless of claim length, and unambiguous when either part
// contains a delimiter-like character.
func TestOIDCUserIDBoundsAndUnambiguity(t *testing.T) {
	base := oidcUserID("https://idp.example/realms/main", "subject-1")
	if !strings.HasPrefix(base, "oidc_") || len(base) != len("oidc_")+64 {
		t.Fatalf("identity shape wrong: %q", base)
	}
	if base != oidcUserID("https://idp.example/realms/main", "subject-1") {
		t.Fatal("identity is not deterministic")
	}
	if base == oidcUserID("https://idp.example/realms/main/", "subject-1") {
		t.Fatal("trailing slash issuer collapsed into the same identity")
	}
	// The old issuer+"|"+sub encoding could not tell these two apart.
	if oidcUserID("https://idp.example/a|b", "c") == oidcUserID("https://idp.example/a", "b|c") {
		t.Fatal("delimiter in the issuer collided with a delimiter in the subject")
	}
	if oidcUserID("a", "b|c") == oidcUserID("a|b", "c") {
		t.Fatal("ambiguous issuer/subject split produced the same identity")
	}
	longIssuer := "https://" + strings.Repeat("i", 4096) + ".example/realms/" + strings.Repeat("r", 1024)
	longSubject := strings.Repeat("s", 8192)
	longID := oidcUserID(longIssuer, longSubject)
	if len(longID) != len(base) || len(longID) > 256 {
		t.Fatalf("long claims produced an unbounded identity: %d characters", len(longID))
	}
	if longID != oidcUserID(longIssuer, longSubject) {
		t.Fatal("long identity is not deterministic")
	}
	if longID == oidcUserID(longIssuer, longSubject+"x") {
		t.Fatal("subject change did not change the identity")
	}
	if longID == oidcUserID(longIssuer+"x", longSubject) {
		t.Fatal("issuer change did not change the identity")
	}
}

// TestOIDCIdentityIndependentOfClientConfig shows the same issuer and subject
// authenticate to the same owner ID even when the providers use different
// OAuth clients, so ownership survives a client reconfiguration.
func TestOIDCIdentityIndependentOfClientConfig(t *testing.T) {
	idp := newMockIDP(t)
	mappings := []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}
	envA := newLoginTestEnvForIDP(t, idp, "client-a", mappings, "")
	envB := newLoginTestEnvForIDP(t, idp, "client-b", mappings, "")
	idp.setClaims(map[string]any{"groups": []string{"admins"}})

	successA, _ := envA.completeLogin(t)
	successA.Body.Close()
	statusA := responseJSON[loginStatusView](t, getResponse(t, envA.client, envA.app.URL+"/api/auth"))
	successB, _ := envB.completeLogin(t)
	successB.Body.Close()
	statusB := responseJSON[loginStatusView](t, getResponse(t, envB.client, envB.app.URL+"/api/auth"))

	if statusA.User == nil || statusB.User == nil {
		t.Fatalf("sign-in failed: %+v %+v", statusA.User, statusB.User)
	}
	if statusA.User.ID != statusB.User.ID {
		t.Fatalf("client configuration changed the identity: %q != %q", statusA.User.ID, statusB.User.ID)
	}
	if statusA.User.ID != oidcUserID(idp.issuer(), "subject-1") {
		t.Fatalf("unexpected identity: %q", statusA.User.ID)
	}
}

// TestOIDCLoginIssuerTrailingSlashExact proves the configured issuer is used
// exactly as written: discovery and login succeed against an issuer that ends
// in a slash, the identity keeps that spelling, and it is stable across
// sign-ins.
func TestOIDCLoginIssuerTrailingSlashExact(t *testing.T) {
	idp := newMockIDP(t)
	idp.setIssuer(idp.server.URL + "/")
	env := newLoginTestEnvForIDP(t, idp, "vrouter-client", []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "")
	env.idp.setClaims(map[string]any{"groups": []string{"admins"}})
	if !strings.HasSuffix(idp.issuer(), "/") {
		t.Fatal("mock issuer does not end in a slash")
	}

	success, _ := env.completeLogin(t)
	success.Body.Close()
	if success.StatusCode != http.StatusFound {
		t.Fatalf("slash issuer login failed: %d", success.StatusCode)
	}
	wantID := oidcUserID(idp.issuer(), "subject-1")
	status := responseJSON[loginStatusView](t, getResponse(t, env.client, env.app.URL+"/api/auth"))
	if status.User == nil || status.User.ID != wantID {
		t.Fatalf("slash issuer identity wrong: %+v", status.User)
	}

	// Log out and back in: the same exact issuer keeps the same owner ID.
	request, _ := http.NewRequest("POST", env.app.URL+"/api/logout", nil)
	logout, err := env.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	logout.Body.Close()
	success, _ = env.completeLogin(t)
	success.Body.Close()
	status = responseJSON[loginStatusView](t, getResponse(t, env.client, env.app.URL+"/api/auth"))
	if status.User == nil || status.User.ID != wantID {
		t.Fatalf("slash issuer identity changed between sign-ins: %+v", status.User)
	}
	// The slash is part of the identity, not cosmetic normalization.
	if oidcUserID(strings.TrimSuffix(idp.issuer(), "/"), "subject-1") == wantID {
		t.Fatal("slash and slash-less issuers produced the same identity")
	}
}

func TestLoginNameAndEmailPresentation(t *testing.T) {
	cases := []struct {
		name      string
		claims    map[string]any
		wantName  string
		wantEmail string
	}{
		{name: "preferred username fallback", claims: map[string]any{"preferred_username": "ada", "email": "ada@example.com"}, wantName: "ada", wantEmail: "ada@example.com"},
		{name: "unverified email omitted", claims: map[string]any{"name": "Ada", "email": "ada@example.com", "email_verified": false}, wantName: "Ada", wantEmail: ""},
		{name: "subject fallback", claims: map[string]any{}, wantName: "subject-1", wantEmail: ""},
		{name: "email is display metadata", claims: map[string]any{"email": "ada@example.com"}, wantName: "ada@example.com", wantEmail: "ada@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "")
			claims := map[string]any{"groups": []string{"admins"}}
			for name, value := range tc.claims {
				claims[name] = value
			}
			env.idp.setClaims(claims)
			success, _ := env.completeLogin(t)
			success.Body.Close()
			status := responseJSON[loginStatusView](t, getResponse(t, env.client, env.app.URL+"/api/auth"))
			if status.User == nil || status.User.Name != tc.wantName || status.User.Email != tc.wantEmail {
				t.Fatalf("presentation wrong: %+v", status.User)
			}
		})
	}
}

func TestLoginAdminTokenAndModes(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "break-glass")
	request := httptest.NewRequest("GET", "http://localhost/api/state", nil)
	request.RemoteAddr = "127.0.0.1:5555"
	if _, ok := env.auth.authenticate(request); ok {
		t.Fatal("oidc mode trusted the loopback peer without a session")
	}
	request.Header.Set("Authorization", "Bearer break-glass")
	user, ok := env.auth.authenticate(request)
	if !ok || user.ID != "local-admin" || user.Name != "Administrator" || user.Role != "admin" {
		t.Fatalf("admin bearer not accepted: %+v", user)
	}
	request.Header.Set("Authorization", "Bearer wrong")
	if _, ok := env.auth.authenticate(request); ok {
		t.Fatal("wrong admin bearer accepted")
	}
	// The public status endpoint reflects a valid bearer too.
	statusReq, err := http.NewRequest("GET", env.app.URL+"/api/auth", nil)
	if err != nil {
		t.Fatal(err)
	}
	statusReq.Header.Set("Authorization", "Bearer break-glass")
	statusResp, err := noRedirectClient().Do(statusReq)
	if err != nil {
		t.Fatal(err)
	}
	status := responseJSON[loginStatusView](t, statusResp)
	if status.Mode != "oidc" || status.User == nil || status.User.ID != "local-admin" {
		t.Fatalf("status did not show admin bearer: %+v", status)
	}

	local, err := newLoginAuth(IdentityConfig{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if local.mode() != "local" {
		t.Fatalf("local mode is %q", local.mode())
	}
	loopback := httptest.NewRequest("GET", "http://localhost/api/state", nil)
	loopback.RemoteAddr = "127.0.0.1:1234"
	if user, ok := local.authenticate(loopback); !ok || user.ID != "local-admin" {
		t.Fatalf("local loopback denied: %+v", user)
	}
	loopback.Host = "evil.example"
	if _, ok := local.authenticate(loopback); ok {
		t.Fatal("local mode trusted a forged Host")
	}
	remote := httptest.NewRequest("GET", "http://localhost/api/state", nil)
	remote.RemoteAddr = "192.0.2.10:1234"
	if _, ok := local.authenticate(remote); ok {
		t.Fatal("local mode trusted a remote peer")
	}

	token, err := newLoginAuth(IdentityConfig{}, "token-value")
	if err != nil {
		t.Fatal(err)
	}
	if token.mode() != "token" {
		t.Fatalf("token mode is %q", token.mode())
	}
	plain := httptest.NewRequest("GET", "http://localhost/api/state", nil)
	plain.RemoteAddr = "127.0.0.1:1234"
	if _, ok := token.authenticate(plain); ok {
		t.Fatal("token mode trusted loopback")
	}
	plain.Header.Set("Authorization", "Bearer token-value")
	if user, ok := token.authenticate(plain); !ok || user.ID != "local-admin" {
		t.Fatalf("token mode rejected the admin token: %+v", user)
	}
}

func TestLoginStatusModesAndUnknownProviders(t *testing.T) {
	mux := http.NewServeMux()
	local, err := newLoginAuth(IdentityConfig{}, "")
	if err != nil {
		t.Fatal(err)
	}
	local.register(mux)
	app := httptest.NewServer(mux)
	defer app.Close()
	status := responseJSON[loginStatusView](t, getResponse(t, noRedirectClient(), app.URL+"/api/auth"))
	if status.Mode != "local" || status.User == nil || status.User.ID != "local-admin" {
		t.Fatalf("local status wrong: %+v", status)
	}
	if status.Providers == nil || len(status.Providers) != 0 {
		t.Fatalf("providers must be an empty array: %+v", status.Providers)
	}
	for _, path := range []string{"/auth/login/idp", "/auth/callback/idp"} {
		resp := getResponse(t, noRedirectClient(), app.URL+path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s in local mode: %d", path, resp.StatusCode)
		}
	}

	oidcEnv := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}}, "")
	for _, path := range []string{"/auth/login/nope", "/auth/callback/nope"} {
		resp := getResponse(t, noRedirectClient(), oidcEnv.app.URL+path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
}

func TestLoginCookieFlags(t *testing.T) {
	idp := newMockIDP(t)
	provider := OIDCProviderConfig{ID: "idp", Name: "IdP", Issuer: idp.issuer(), ClientID: "client", RoleMappings: []RoleMappingConfig{{Claim: "role", Value: "user", Role: "user"}}}
	secureAuth, err := newLoginAuth(IdentityConfig{PublicURL: "https://vrouter.example", Providers: []OIDCProviderConfig{provider}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !secureAuth.secureCookies() {
		t.Fatal("HTTPS public URL did not enable secure cookies")
	}
	recorder := httptest.NewRecorder()
	secureAuth.setCookie(recorder, loginSessionCookie, "opaque-token", time.Now().Add(time.Hour))
	cookie := recorder.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge <= 0 {
		t.Fatalf("secure cookie flags wrong: %+v", cookie)
	}
	if cookie.Value != "opaque-token" {
		t.Fatal("cookie value was rewritten")
	}

	plainAuth, err := newLoginAuth(IdentityConfig{PublicURL: "http://127.0.0.1:9999", Providers: []OIDCProviderConfig{provider}}, "")
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	plainAuth.setCookie(recorder, loginSessionCookie, "opaque-token", time.Now().Add(time.Hour))
	if cookie = recorder.Result().Cookies()[0]; cookie.Secure {
		t.Fatal("loopback HTTP cookies must not be marked secure")
	}
}

func TestLoginConfigValidationInNewLoginAuth(t *testing.T) {
	mappings := []RoleMappingConfig{{Claim: "role", Value: "user", Role: "user"}}
	cases := []struct {
		name string
		cfg  IdentityConfig
	}{
		{name: "providers without public url", cfg: IdentityConfig{Providers: []OIDCProviderConfig{{ID: "idp", Name: "IdP", Issuer: "https://idp.example", ClientID: "client", RoleMappings: mappings}}}},
		{name: "missing name", cfg: IdentityConfig{PublicURL: "https://vrouter.example", Providers: []OIDCProviderConfig{{ID: "idp", Issuer: "https://idp.example", ClientID: "client", RoleMappings: mappings}}}},
		{name: "invalid role", cfg: IdentityConfig{PublicURL: "https://vrouter.example", Providers: []OIDCProviderConfig{{ID: "idp", Name: "IdP", Issuer: "https://idp.example", ClientID: "client", RoleMappings: []RoleMappingConfig{{Claim: "role", Value: "x", Role: "root"}}}}}},
		{name: "unresolved secret", cfg: IdentityConfig{PublicURL: "https://vrouter.example", Providers: []OIDCProviderConfig{{ID: "idp", Name: "IdP", Issuer: "https://idp.example", ClientID: "client", ClientSecretEnv: "VROUTER_UNSET_SECRET", RoleMappings: mappings}}}},
		{name: "no role mappings", cfg: IdentityConfig{PublicURL: "https://vrouter.example", Providers: []OIDCProviderConfig{{ID: "idp", Name: "IdP", Issuer: "https://idp.example", ClientID: "client"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newLoginAuth(tc.cfg, ""); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestLoginDiscoveryFailureAndInsecureEndpoints(t *testing.T) {
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	_, err := newLoginAuth(IdentityConfig{PublicURL: "http://127.0.0.1:9999", Providers: []OIDCProviderConfig{{
		ID: "idp", Name: "IdP", Issuer: plain.URL, ClientID: "client",
		RoleMappings: []RoleMappingConfig{{Claim: "role", Value: "user", Role: "user"}},
	}}}, "")
	if err == nil || !strings.Contains(err.Error(), "idp") {
		t.Fatalf("discovery failure not reported: %v", err)
	}

	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		writeMockJSON(w, 200, map[string]any{
			"issuer":                                "http://" + r.Host,
			"authorization_endpoint":                "http://evil.example/authorize",
			"token_endpoint":                        "https://idp.example/token",
			"jwks_uri":                              "http://" + r.Host + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	defer insecure.Close()
	_, err = newLoginAuth(IdentityConfig{PublicURL: insecure.URL, Providers: []OIDCProviderConfig{{
		ID: "idp", Name: "IdP", Issuer: insecure.URL, ClientID: "client",
		RoleMappings: []RoleMappingConfig{{Claim: "role", Value: "user", Role: "user"}},
	}}}, "")
	if err == nil || !strings.Contains(err.Error(), "authorization_endpoint") {
		t.Fatalf("insecure discovery endpoint not reported: %v", err)
	}
}

// TestLoginDiscoveryEndpointValidation proves the discovered jwks_uri gets the
// same checks as the authorization and token endpoints: HTTPS (loopback HTTP
// allowed), no user information, no fragment, and present at all.
func TestLoginDiscoveryEndpointValidation(t *testing.T) {
	cases := []struct {
		name         string
		mutate       func(doc map[string]any, host string)
		wantContains []string
	}{
		{
			name:         "jwks insecure remote",
			mutate:       func(doc map[string]any, host string) { doc["jwks_uri"] = "http://evil.example/keys" },
			wantContains: []string{"jwks_uri", "HTTPS"},
		},
		{
			name:         "jwks user information",
			mutate:       func(doc map[string]any, host string) { doc["jwks_uri"] = "https://user:pass@idp.example/keys" },
			wantContains: []string{"jwks_uri", "user information or a fragment"},
		},
		{
			name:         "jwks fragment",
			mutate:       func(doc map[string]any, host string) { doc["jwks_uri"] = "https://idp.example/keys#fragment" },
			wantContains: []string{"jwks_uri", "user information or a fragment"},
		},
		{
			name:         "jwks missing",
			mutate:       func(doc map[string]any, host string) { delete(doc, "jwks_uri") },
			wantContains: []string{"jwks_uri"},
		},
		{
			name:         "token user information",
			mutate:       func(doc map[string]any, host string) { doc["token_endpoint"] = "https://user@idp.example/token" },
			wantContains: []string{"token_endpoint", "user information or a fragment"},
		},
		{
			name: "authorization fragment",
			mutate: func(doc map[string]any, host string) {
				doc["authorization_endpoint"] = "https://idp.example/authorize#fragment"
			},
			wantContains: []string{"authorization_endpoint", "user information or a fragment"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/.well-known/openid-configuration" {
					http.NotFound(w, r)
					return
				}
				doc := map[string]any{
					"issuer":                                "http://" + r.Host,
					"authorization_endpoint":                "http://" + r.Host + "/authorize",
					"token_endpoint":                        "http://" + r.Host + "/token",
					"jwks_uri":                              "http://" + r.Host + "/keys",
					"id_token_signing_alg_values_supported": []string{"RS256"},
				}
				tc.mutate(doc, r.Host)
				writeMockJSON(w, 200, doc)
			}))
			defer server.Close()
			_, err := newLoginAuth(IdentityConfig{PublicURL: "http://127.0.0.1:9999", Providers: []OIDCProviderConfig{{
				ID: "idp", Name: "IdP", Issuer: server.URL, ClientID: "client",
				RoleMappings: []RoleMappingConfig{{Claim: "role", Value: "user", Role: "user"}},
			}}}, "")
			if err == nil {
				t.Fatal("invalid discovery document accepted")
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// countingIdleTransport records CloseIdleConnections calls so tests can prove
// the auth HTTP client releases its connections on success and failure paths.
type countingIdleTransport struct {
	base   *http.Transport
	closed atomic.Int32
}

func (c *countingIdleTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return c.base.RoundTrip(r)
}

func (c *countingIdleTransport) CloseIdleConnections() {
	c.closed.Add(1)
	c.base.CloseIdleConnections()
}

// withCountingLoginHTTPClient swaps the auth HTTP client factory for a client
// whose transport counts idle-connection cleanup.
func withCountingLoginHTTPClient(t *testing.T) *countingIdleTransport {
	t.Helper()
	counting := &countingIdleTransport{base: http.DefaultTransport.(*http.Transport).Clone()}
	original := loginHTTPClient
	loginHTTPClient = func() *http.Client {
		return &http.Client{Transport: counting, Timeout: loginRequestWait}
	}
	t.Cleanup(func() { loginHTTPClient = original })
	return counting
}

func testIdentityConfig(issuer string) IdentityConfig {
	return IdentityConfig{PublicURL: "http://127.0.0.1:9999", Providers: []OIDCProviderConfig{{
		ID: "idp", Name: "IdP", Issuer: issuer, ClientID: "client",
		RoleMappings: []RoleMappingConfig{{Claim: "role", Value: "user", Role: "user"}},
	}}}
}

func TestLoginAuthCloseReleasesConnections(t *testing.T) {
	idp := newMockIDP(t)
	counting := withCountingLoginHTTPClient(t)
	auth, err := newLoginAuth(testIdentityConfig(idp.issuer()), "")
	if err != nil {
		t.Fatalf("newLoginAuth: %v", err)
	}
	if got := counting.closed.Load(); got != 0 {
		t.Fatalf("idle connections closed while auth was still in use: %d", got)
	}
	if err := auth.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := auth.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := counting.closed.Load(); got != 1 {
		t.Fatalf("Close was not idempotent: %d cleanup calls", got)
	}
}

func TestLoginAuthCloseOnInitFailure(t *testing.T) {
	t.Run("discovery failure", func(t *testing.T) {
		plain := httptest.NewServer(http.NotFoundHandler())
		defer plain.Close()
		counting := withCountingLoginHTTPClient(t)
		if _, err := newLoginAuth(testIdentityConfig(plain.URL), ""); err == nil {
			t.Fatal("failed discovery accepted")
		}
		if counting.closed.Load() < 1 {
			t.Fatal("failed discovery leaked idle connections")
		}
	})
	t.Run("endpoint validation failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/.well-known/openid-configuration" {
				http.NotFound(w, r)
				return
			}
			writeMockJSON(w, 200, map[string]any{
				"issuer":                                "http://" + r.Host,
				"authorization_endpoint":                "http://evil.example/authorize",
				"token_endpoint":                        "http://" + r.Host + "/token",
				"jwks_uri":                              "http://" + r.Host + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		}))
		defer server.Close()
		counting := withCountingLoginHTTPClient(t)
		if _, err := newLoginAuth(testIdentityConfig(server.URL), ""); err == nil {
			t.Fatal("insecure endpoint accepted")
		}
		if counting.closed.Load() < 1 {
			t.Fatal("rejected discovery leaked idle connections")
		}
	})
	t.Run("later provider failure closes earlier connections", func(t *testing.T) {
		good := newMockIDP(t)
		bad := httptest.NewServer(http.NotFoundHandler())
		defer bad.Close()
		counting := withCountingLoginHTTPClient(t)
		cfg := IdentityConfig{PublicURL: "http://127.0.0.1:9999", Providers: []OIDCProviderConfig{
			{ID: "good", Name: "Good", Issuer: good.issuer(), ClientID: "client", RoleMappings: []RoleMappingConfig{{Claim: "role", Value: "user", Role: "user"}}},
			{ID: "bad", Name: "Bad", Issuer: bad.URL, ClientID: "client", RoleMappings: []RoleMappingConfig{{Claim: "role", Value: "user", Role: "user"}}},
		}}
		if _, err := newLoginAuth(cfg, ""); err == nil {
			t.Fatal("failing provider accepted")
		}
		if counting.closed.Load() < 1 {
			t.Fatal("provider failure leaked connections from earlier discovery")
		}
	})
}

// TestLoginVerifierAfterDiscoveryContextCanceled pins the library behavior the
// auth flow relies on: newLoginAuth cancels its discovery context before it
// returns, yet the provider verifier must still fetch JWKS on the first
// verification. The library creates its key set lazily from context.Background
// (with the configured client) and wraps it in context.WithoutCancel, so the
// canceled discovery context cannot poison later logins.
func TestLoginVerifierAfterDiscoveryContextCanceled(t *testing.T) {
	env := newLoginTestEnv(t, []RoleMappingConfig{{Claim: "role", Value: "user", Role: "user"}}, "")
	claims := map[string]any{
		"iss":   env.idp.issuer(),
		"aud":   "vrouter-client",
		"sub":   "subject-1",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"nonce": "any-nonce",
	}
	raw := env.idp.sign(claims, false)
	if _, err := env.auth.providers["test-idp"].verifier.Verify(context.Background(), raw); err != nil {
		t.Fatalf("verifier could not fetch keys after discovery context cancellation: %v", err)
	}
}

func TestMatchRole(t *testing.T) {
	mappings := []RoleMappingConfig{
		{Claim: "realm_access.roles", Value: "vrouter-user", Role: "user"},
		{Claim: "groups", Value: "admins", Role: "admin"},
	}
	nested := map[string]any{
		"realm_access": map[string]any{"roles": []any{"vrouter-user"}},
		"groups":       []any{"admins"},
	}
	if role, ok := matchRole(nested, mappings); !ok || role != "admin" {
		t.Fatalf("admin precedence failed: %q %v", role, ok)
	}
	if role, ok := matchRole(map[string]any{"groups": "admins"}, mappings); !ok || role != "admin" {
		t.Fatalf("string leaf failed: %q %v", role, ok)
	}
	if role, ok := matchRole(map[string]any{"realm_access": map[string]any{"roles": []any{"vrouter-user"}}}, mappings); !ok || role != "user" {
		t.Fatalf("nested array failed: %q %v", role, ok)
	}
	if role, ok := matchRole(map[string]any{"groups": []any{map[string]any{"name": "admins"}}}, []RoleMappingConfig{{Claim: "groups.name", Value: "admins", Role: "user"}}); !ok || role != "user" {
		t.Fatalf("array traversal failed: %q %v", role, ok)
	}
	if role, ok := matchRole(map[string]any{"groups": []any{"staff"}}, mappings); ok || role != "" {
		t.Fatalf("unmatched user granted %q", role)
	}
	if role, ok := matchRole(map[string]any{"groups": []any{42}}, []RoleMappingConfig{{Claim: "groups", Value: "42", Role: "user"}}); ok {
		t.Fatalf("non-string claim matched: %q", role)
	}
	if role, ok := matchRole(nil, mappings); ok {
		t.Fatalf("nil claims granted %q", role)
	}
}

// TestLoginIntegratedWithRouter exercises the sign-in surface through the real
// public New facade: the router must expose /api/auth publicly, refuse
// management without authentication, and keep the legacy admin token working
// in OIDC mode while requiring an explicit gateway selection.
func TestLoginIntegratedWithRouter(t *testing.T) {
	idp := newMockIDP(t)
	handler, err := New(Config{
		DataDir:    t.TempDir(),
		AdminToken: "break-glass",
		Identity: IdentityConfig{
			PublicURL: "http://127.0.0.1:9999",
			Providers: []OIDCProviderConfig{{
				ID: "test-idp", Name: "Test IdP", Issuer: idp.issuer(), ClientID: "vrouter-client",
				RoleMappings: []RoleMappingConfig{{Claim: "groups", Value: "admins", Role: "admin"}},
			}},
		},
	}, fstest.MapFS{"index.html": {Data: []byte("vrouter")}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = handler.(io.Closer).Close() }()

	call := func(method, path, token, gateway string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://localhost"+path, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		if gateway != "" {
			request.Header.Set(gatewayHeader, gateway)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	if recorder := call("GET", "/api/auth", "", ""); recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"mode":"oidc"`) {
		t.Fatalf("public /api/auth wrong: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := call("GET", "/api/state", "", ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated management allowed: %d", recorder.Code)
	}
	if recorder := call("GET", "/api/gateways", "break-glass", ""); recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"id":"default"`) {
		t.Fatalf("admin gateway list wrong: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := call("GET", "/api/state", "break-glass", ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("OIDC mode served management without a gateway selection: %d", recorder.Code)
	}
	if recorder := call("GET", "/api/state", "break-glass", defaultGatewayID); recorder.Code != 200 {
		t.Fatalf("selected default gateway unavailable: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := call("GET", "/api/state", "break-glass", "missing-gateway"); recorder.Code != http.StatusNotFound {
		t.Fatalf("missing gateway not reported as 404: %d", recorder.Code)
	}
}

// TestLoginLongSubjectCreatesOwnedGateway drives a browser-style sign-in with
// a multi-kilobyte subject through the public facade, then creates a gateway:
// the derived owner ID must stay inside the registry's bound while ownership
// still matches the signed-in identity.
func TestLoginLongSubjectCreatesOwnedGateway(t *testing.T) {
	idp := newMockIDP(t)
	longSubject := strings.Repeat("subject-", 512)
	idp.setClaims(map[string]any{"groups": []string{"vrouter-users"}, "name": "Long Subject", "sub": longSubject})

	handler, err := New(Config{
		DataDir: t.TempDir(),
		Identity: IdentityConfig{
			PublicURL: "http://127.0.0.1:9999",
			Providers: []OIDCProviderConfig{{
				ID: "test-idp", Name: "Test IdP", Issuer: idp.issuer(), ClientID: "vrouter-client",
				RoleMappings: []RoleMappingConfig{{Claim: "groups", Value: "vrouter-users", Role: "user"}},
			}},
		},
	}, fstest.MapFS{"index.html": {Data: []byte("vrouter")}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = handler.(io.Closer).Close() }()

	app := httptest.NewServer(handler)
	defer app.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	loginResp := getResponse(t, client, app.URL+"/auth/login/test-idp")
	loginResp.Body.Close()
	authURL, err := url.Parse(loginResp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	idpResp := getResponse(t, noRedirectClient(), authURL.String())
	idpResp.Body.Close()
	callback, err := url.Parse(idpResp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	// The configured public URL is not the test listener; run the callback on
	// the listener while keeping the query intact.
	callbackResp := getResponse(t, client, app.URL+callback.Path+"?"+callback.RawQuery)
	callbackResp.Body.Close()
	if callbackResp.StatusCode != http.StatusFound {
		t.Fatalf("callback: %d", callbackResp.StatusCode)
	}
	if session := cookieByName(callbackResp, loginSessionCookie); session == nil {
		t.Fatal("no session cookie after sign-in")
	}

	status := responseJSON[loginStatusView](t, getResponse(t, client, app.URL+"/api/auth"))
	wantID := oidcUserID(idp.issuer(), longSubject)
	if status.User == nil || status.User.Role != "user" || status.User.ID != wantID {
		t.Fatalf("long subject identity wrong: %+v", status.User)
	}
	if len(status.User.ID) > 256 {
		t.Fatalf("owner ID exceeds the registry bound: %d", len(status.User.ID))
	}

	request, err := http.NewRequest("POST", app.URL+"/api/gateways", strings.NewReader(`{"name":"Long subject gateway"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	createResp, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(createResp.Body)
		t.Fatalf("gateway creation: %d %s", createResp.StatusCode, raw)
	}
	var created gatewayResponse
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Gateway.ID == "" || created.Gateway.OwnerID != wantID {
		t.Fatalf("created gateway owner wrong: %+v", created.Gateway)
	}

	// The created gateway is visible to its owner through the same session.
	list := responseJSON[gatewaysResponse](t, getResponse(t, client, app.URL+"/api/gateways"))
	found := false
	for _, gateway := range list.Gateways {
		if gateway.ID == created.Gateway.ID && gateway.OwnerID == wantID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created gateway missing from the owner's list: %+v", list.Gateways)
	}
}
