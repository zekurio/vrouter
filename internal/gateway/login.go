package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Application sign-in is a distinct integration from provider account OAuth
// (Codex, Claude): it authenticates people against OIDC providers configured
// in the environment and never touches stored provider credentials.
const (
	loginSessionCookie   = "vrouter_session"
	loginStateCookie     = "vrouter_login"
	loginSessionTTL      = 12 * time.Hour
	loginTransactionTTL  = 5 * time.Minute
	maxLoginTransactions = 128
	loginDiscoveryWait   = 20 * time.Second
	loginRequestWait     = 20 * time.Second
)

// loginUser is an authenticated application user. ID is a stable opaque value
// derived from the verified issuer and subject, so email changes never change
// the identity and claim length can never overflow the registry.
type loginUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	Role  string `json:"role"`
}

// oidcProvider is one configured sign-in provider. The client secret lives
// here and in the oauth2 config only; it is never written to a response.
type oidcProvider struct {
	id           string
	name         string
	issuer       string
	clientID     string
	roleMappings []RoleMappingConfig
	oauth        oauth2.Config
	verifier     *oidc.IDTokenVerifier
}

type loginSession struct {
	user    loginUser
	expires time.Time
}

// loginTransaction is the server-side half of an in-flight sign-in. The
// browser holds a random handle in an HttpOnly cookie, so a callback that was
// not started in this browser cannot complete, and each transaction works
// exactly once.
type loginTransaction struct {
	providerID string
	state      string
	nonce      string
	verifier   string
	expires    time.Time
}

// loginAuth implements application sign-in. Sessions and pending sign-ins
// live in memory; there are no goroutines to stop.
type loginAuth struct {
	authMode   string
	publicURL  string
	adminToken string
	providers  map[string]*oidcProvider
	ordered    []*oidcProvider
	client     *http.Client

	mu       sync.Mutex
	sessions map[string]loginSession
	pending  map[string]loginTransaction
	now      func() time.Time

	closeOnce sync.Once
}

// loginHTTPClient builds the HTTP client used for OIDC discovery, token
// exchange and key fetches.
func loginHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = loginRequestWait
	return &http.Client{
		Transport: transport,
		Timeout:   loginRequestWait,
		// A redirect could send credentials or a code to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// newLoginAuth validates the configuration and performs OIDC discovery for
// every provider. Discovery fails startup so a typo cannot become a login
// that silently never works. The admin token remains accepted in every mode.
func newLoginAuth(cfg IdentityConfig, adminToken string) (*loginAuth, error) {
	normalized := normalizeIdentityConfig(cfg)
	if err := validateIdentityConfig(normalized); err != nil {
		return nil, err
	}
	a := &loginAuth{
		authMode:   "local",
		adminToken: adminToken,
		providers:  map[string]*oidcProvider{},
		sessions:   map[string]loginSession{},
		pending:    map[string]loginTransaction{},
		now:        time.Now,
	}
	if len(normalized.Providers) == 0 {
		if adminToken != "" {
			a.authMode = "token"
		}
		return a, nil
	}
	publicURL, err := normalizePublicURL(normalized.PublicURL)
	if err != nil {
		return nil, err
	}
	a.publicURL = publicURL
	a.authMode = "oidc"
	a.client = loginHTTPClient()
	for _, providerCfg := range normalized.Providers {
		providerCtx, cancel := context.WithTimeout(context.Background(), loginDiscoveryWait)
		providerCtx = oidc.ClientContext(providerCtx, a.client)
		discovered, err := oidc.NewProvider(providerCtx, providerCfg.Issuer)
		cancel()
		if err != nil {
			_ = a.Close()
			return nil, fmt.Errorf("OIDC provider %q: discovery against %s failed: %w", providerCfg.ID, providerCfg.Issuer, err)
		}
		if err := validateDiscoveredEndpoints(providerCfg.ID, discovered); err != nil {
			_ = a.Close()
			return nil, err
		}
		provider := a.buildProvider(providerCfg, discovered)
		a.ordered = append(a.ordered, provider)
		a.providers[provider.id] = provider
	}
	return a, nil
}

// Close releases idle HTTP connections used for discovery and token exchange.
// It is safe to call more than once.
func (a *loginAuth) Close() error {
	a.closeOnce.Do(func() {
		if a.client != nil {
			a.client.CloseIdleConnections()
		}
	})
	return nil
}

func (a *loginAuth) buildProvider(providerCfg OIDCProviderConfig, discovered *oidc.Provider) *oidcProvider {
	endpoint := discovered.Endpoint()
	return &oidcProvider{
		id:           providerCfg.ID,
		name:         providerCfg.Name,
		issuer:       providerCfg.Issuer,
		clientID:     providerCfg.ClientID,
		roleMappings: append([]RoleMappingConfig(nil), providerCfg.RoleMappings...),
		oauth: oauth2.Config{
			ClientID:     providerCfg.ClientID,
			ClientSecret: providerCfg.clientSecret,
			Endpoint:     endpoint,
			RedirectURL:  a.publicURL + "/auth/callback/" + providerCfg.ID,
			Scopes:       append([]string(nil), providerCfg.Scopes...),
		},
		verifier: discovered.Verifier(&oidc.Config{ClientID: providerCfg.ClientID}),
	}
}

// validateDiscoveredEndpoints rejects a discovery document that points the
// flow at an insecure or ambiguous location. Providers may use separate hosts
// for the authorization, token and JWKS endpoints, so only each URL itself is
// checked, but all three must be HTTPS (loopback HTTP is allowed for
// development) and none may carry user information or a fragment. The JWKS
// URL comes from the library's own discovery claims API, never from a
// separately parsed weaker document.
func validateDiscoveredEndpoints(providerID string, discovered *oidc.Provider) error {
	var metadata struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := discovered.Claims(&metadata); err != nil {
		return fmt.Errorf("OIDC provider %q: discovery metadata could not be read: %w", providerID, err)
	}
	endpoint := discovered.Endpoint()
	checks := []struct {
		label string
		raw   string
	}{
		{label: "authorization_endpoint", raw: endpoint.AuthURL},
		{label: "token_endpoint", raw: endpoint.TokenURL},
		{label: "jwks_uri", raw: metadata.JWKSURL},
	}
	for _, check := range checks {
		parsed, err := url.Parse(check.raw)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" {
			return fmt.Errorf("OIDC provider %q: discovery has no valid %s", providerID, check.label)
		}
		if parsed.User != nil || parsed.Fragment != "" {
			return fmt.Errorf("OIDC provider %q: %s must not contain user information or a fragment", providerID, check.label)
		}
		scheme := strings.ToLower(parsed.Scheme)
		if scheme != "https" && !(scheme == "http" && loopbackHost(parsed.Hostname())) {
			return fmt.Errorf("OIDC provider %q: %s must use HTTPS", providerID, check.label)
		}
	}
	return nil
}

// mode reports the authentication mode: oidc when providers are configured,
// token when only an admin token is set, local otherwise.
func (a *loginAuth) mode() string { return a.authMode }

// register installs the application sign-in endpoints. Management routes stay
// owned by the caller; these endpoints never expose gateway data.
func (a *loginAuth) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth", a.handleAuthStatus)
	mux.HandleFunc("GET /auth/login/{provider}", a.handleLogin)
	mux.HandleFunc("GET /auth/callback/{provider}", a.handleCallback)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
}

// authenticate resolves the request identity. A legacy admin Bearer token is
// accepted in every mode and maps to local-admin. Cookie sessions resolve to
// the signed-in user. In local mode the legacy peer and Host loopback guard
// applies, exactly as the original server did.
func (a *loginAuth) authenticate(r *http.Request) (loginUser, bool) {
	if a.adminToken != "" {
		if header := r.Header.Get("Authorization"); header != "" {
			if !strings.HasPrefix(header, "Bearer ") || !tokenEqual(strings.TrimPrefix(header, "Bearer "), a.adminToken) {
				return loginUser{}, false
			}
			return localAdminUser(), true
		}
	}
	if cookie, err := r.Cookie(loginSessionCookie); err == nil && cookie.Value != "" {
		a.mu.Lock()
		session, ok := a.sessions[cookie.Value]
		if ok && a.now().After(session.expires) {
			delete(a.sessions, cookie.Value)
			ok = false
		}
		a.sweepLocked()
		a.mu.Unlock()
		if ok {
			return session.user, true
		}
	}
	if a.authMode == "local" && localRequest(r) {
		return localAdminUser(), true
	}
	return loginUser{}, false
}

type loginProviderView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	LoginURL string `json:"loginUrl"`
}

type loginStatusView struct {
	Mode      string              `json:"mode"`
	Providers []loginProviderView `json:"providers"`
	User      *loginUser          `json:"user"`
}

func (a *loginAuth) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	status := loginStatusView{Mode: a.authMode, Providers: make([]loginProviderView, 0, len(a.ordered))}
	for _, provider := range a.ordered {
		status.Providers = append(status.Providers, loginProviderView{
			ID:       provider.id,
			Name:     provider.name,
			LoginURL: a.publicURL + "/auth/login/" + provider.id,
		})
	}
	if user, ok := a.authenticate(r); ok {
		status.User = &user
	}
	writeJSON(w, 200, status)
}

func (a *loginAuth) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.authMode != "oidc" {
		writeJSON(w, 404, map[string]string{"error": "Single sign-on is not configured"})
		return
	}
	provider := a.providers[r.PathValue("provider")]
	if provider == nil {
		writeJSON(w, 404, map[string]string{"error": "Unknown sign-in provider"})
		return
	}
	handle, err := newLoginSecret()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not start sign-in"})
		return
	}
	state, err := newLoginSecret()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not start sign-in"})
		return
	}
	nonce, err := newLoginSecret()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not start sign-in"})
		return
	}
	verifier, err := newLoginSecret()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not start sign-in"})
		return
	}
	expires := a.now().Add(loginTransactionTTL)
	a.mu.Lock()
	a.sweepLocked()
	if len(a.pending) >= maxLoginTransactions {
		a.mu.Unlock()
		writeJSON(w, 503, map[string]string{"error": "Too many sign-in attempts are already in progress"})
		return
	}
	if cookie, err := r.Cookie(loginStateCookie); err == nil && cookie.Value != "" {
		delete(a.pending, cookie.Value)
	}
	a.pending[handle] = loginTransaction{
		providerID: provider.id,
		state:      state,
		nonce:      nonce,
		verifier:   verifier,
		expires:    expires,
	}
	a.mu.Unlock()
	a.setCookie(w, loginStateCookie, handle, expires)
	target := provider.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, target, http.StatusFound)
}

func (a *loginAuth) handleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.authMode != "oidc" {
		writeJSON(w, 404, map[string]string{"error": "Single sign-on is not configured"})
		return
	}
	provider := a.providers[r.PathValue("provider")]
	if provider == nil {
		writeJSON(w, 404, map[string]string{"error": "Unknown sign-in provider"})
		return
	}
	query := r.URL.Query()
	if providerError := query.Get("error"); providerError != "" {
		message := "Sign-in failed at the identity provider"
		if providerError == "access_denied" {
			message = "Sign-in was cancelled"
		}
		writeJSON(w, 400, map[string]string{"error": message})
		return
	}
	code, state := query.Get("code"), query.Get("state")
	if code == "" || state == "" {
		writeJSON(w, 400, map[string]string{"error": "The sign-in response is missing the authorization code or state"})
		return
	}
	cookie, err := r.Cookie(loginStateCookie)
	if err != nil || cookie.Value == "" {
		writeJSON(w, 400, map[string]string{"error": "The sign-in session is missing or has expired"})
		return
	}
	// The transaction is consumed whether or not it validates, so a captured
	// callback cannot be replayed.
	a.mu.Lock()
	transaction, ok := a.pending[cookie.Value]
	delete(a.pending, cookie.Value)
	a.mu.Unlock()
	a.clearCookie(w, loginStateCookie)
	if !ok || transaction.providerID != provider.id || a.now().After(transaction.expires) {
		writeJSON(w, 400, map[string]string{"error": "The sign-in session is missing or has expired"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(transaction.state), []byte(state)) != 1 {
		writeJSON(w, 400, map[string]string{"error": "The sign-in state does not match"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), loginRequestWait)
	defer cancel()
	ctx = oidc.ClientContext(ctx, a.client)
	ctx = context.WithValue(ctx, oauth2.HTTPClient, a.client)
	token, err := provider.oauth.Exchange(ctx, code, oauth2.VerifierOption(transaction.verifier))
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "The identity provider rejected the sign-in"})
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		writeJSON(w, 502, map[string]string{"error": "The identity provider returned no identity token"})
		return
	}
	idToken, err := provider.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		writeJSON(w, 401, map[string]string{"error": "The identity provider returned an invalid identity token"})
		return
	}
	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		writeJSON(w, 401, map[string]string{"error": "The identity provider returned an invalid identity token"})
		return
	}
	if idToken.Subject == "" {
		writeJSON(w, 401, map[string]string{"error": "The identity token has no subject"})
		return
	}
	// With several audiences the token must name this client as the
	// authorized party; otherwise the audience list alone could let a token
	// minted for another client pass as ours.
	if len(idToken.Audience) > 1 {
		party, _ := claims["azp"].(string)
		if party != provider.clientID {
			writeJSON(w, 401, map[string]string{"error": "The identity token was issued for another client"})
			return
		}
	}
	if idToken.Nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(transaction.nonce)) != 1 {
		writeJSON(w, 401, map[string]string{"error": "The identity token nonce does not match"})
		return
	}
	role, ok := matchRole(claims, provider.roleMappings)
	if !ok {
		writeJSON(w, 403, map[string]string{"error": "This account is not authorized for vrouter"})
		return
	}
	user := loginUser{
		ID:    oidcUserID(provider.issuer, idToken.Subject),
		Name:  claimString(claims, "name"),
		Email: claimString(claims, "email"),
		Role:  role,
	}
	if verified, ok := claims["email_verified"].(bool); ok && !verified {
		user.Email = ""
	}
	if user.Name == "" {
		user.Name = claimString(claims, "preferred_username")
	}
	if user.Name == "" {
		user.Name = user.Email
	}
	if user.Name == "" {
		user.Name = idToken.Subject
	}
	session, err := newLoginSecret()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not complete sign-in"})
		return
	}
	a.mu.Lock()
	a.sweepLocked()
	a.sessions[session] = loginSession{user: user, expires: a.now().Add(loginSessionTTL)}
	a.mu.Unlock()
	a.setCookie(w, loginSessionCookie, session, a.now().Add(loginSessionTTL))
	http.Redirect(w, r, a.publicURL+"/", http.StatusFound)
}

func (a *loginAuth) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Cross-origin mutations must not be able to sign a user out or clear
	// their browser-bound sign-in transaction.
	if !sameOriginWrite(r) {
		writeJSON(w, 403, map[string]string{"error": "Cross-origin logout requests are not allowed"})
		return
	}
	if cookie, err := r.Cookie(loginSessionCookie); err == nil && cookie.Value != "" {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}
	if cookie, err := r.Cookie(loginStateCookie); err == nil && cookie.Value != "" {
		a.mu.Lock()
		delete(a.pending, cookie.Value)
		a.mu.Unlock()
	}
	a.clearCookie(w, loginSessionCookie)
	a.clearCookie(w, loginStateCookie)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *loginAuth) setCookie(w http.ResponseWriter, name, value string, expires time.Time) {
	maxAge := int(expires.Sub(a.now()).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *loginAuth) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *loginAuth) secureCookies() bool {
	return strings.HasPrefix(a.publicURL, "https://")
}

// sweepLocked drops expired sessions and sign-ins. Callers hold a.mu.
func (a *loginAuth) sweepLocked() {
	now := a.now()
	for token, session := range a.sessions {
		if now.After(session.expires) {
			delete(a.sessions, token)
		}
	}
	for handle, transaction := range a.pending {
		if now.After(transaction.expires) {
			delete(a.pending, handle)
		}
	}
}

func newLoginSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("secure random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// oidcUserID derives a stable opaque identity from the verified issuer and
// subject. The two parts are serialized as a JSON array, so no delimiter
// inside either part can be mistaken for the boundary between them, and the
// SHA-256 keeps the ID bounded (69 characters) no matter how long the claims
// are. The same issuer and subject therefore always map to the same owner ID,
// independent of client configuration or email changes.
func oidcUserID(issuer, subject string) string {
	// Marshaling a [2]string never fails.
	serialized, _ := json.Marshal([2]string{issuer, subject})
	sum := sha256.Sum256(serialized)
	return "oidc_" + hex.EncodeToString(sum[:])
}

func localAdminUser() loginUser {
	return loginUser{ID: "local-admin", Name: "Administrator", Role: "admin"}
}

// localRequest mirrors the legacy server's loopback guard: the socket peer
// and the Host must both be local, which stops DNS rebinding.
func localRequest(r *http.Request) bool {
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	hostIP, peerIP := net.ParseIP(host), net.ParseIP(peer)
	return peerIP != nil && peerIP.IsLoopback() && (host == "localhost" || (hostIP != nil && hostIP.IsLoopback()))
}

// matchRole applies the provider's role mappings to verified claims. Admin
// wins when several mappings match, and an unmatched account gets no role.
func matchRole(claims map[string]any, mappings []RoleMappingConfig) (string, bool) {
	user := false
	for _, mapping := range mappings {
		if !claimMatches(claims, mapping.Claim, mapping.Value) {
			continue
		}
		if mapping.Role == "admin" {
			return "admin", true
		}
		user = true
	}
	if user {
		return "user", true
	}
	return "", false
}

func claimMatches(claims map[string]any, path, value string) bool {
	for _, candidate := range claimValues(claims, strings.Split(path, ".")) {
		if candidate == value {
			return true
		}
	}
	return false
}

// claimValues resolves a dot-separated claim path. A segment into a JSON
// object selects a key, a JSON array is traversed element by element, and the
// leaf may be a string or an array of strings. Unknown shapes yield nothing.
func claimValues(value any, path []string) []string {
	if len(path) == 0 {
		switch leaf := value.(type) {
		case string:
			return []string{leaf}
		case []any:
			values := make([]string, 0, len(leaf))
			for _, item := range leaf {
				if text, ok := item.(string); ok {
					values = append(values, text)
				}
			}
			return values
		default:
			return nil
		}
	}
	switch current := value.(type) {
	case map[string]any:
		next, ok := current[path[0]]
		if !ok {
			return nil
		}
		return claimValues(next, path[1:])
	case []any:
		var values []string
		for _, item := range current {
			values = append(values, claimValues(item, path)...)
		}
		return values
	default:
		return nil
	}
}
