package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig names the identity provider people sign in with, such as
// Pocket ID.
type OIDCConfig struct {
	// Issuer is the provider's URL, exactly as its discovery document names it.
	Issuer   string
	ClientID string
	// ClientSecret is empty for a public client, which relies on PKCE alone.
	ClientSecret string
	// Groups maps provider groups to roles as comma-separated group=role
	// pairs. People in no listed group cannot sign in.
	Groups string
}

const (
	loginCookie      = "vrouter_login"
	loginLifetime    = 10 * time.Minute
	oidcCallbackPath = "/api/auth/callback"
)

// pendingLogin is a sign-in sent to the provider and not yet back. The
// browser holds it in an HTTP-only cookie, so unfinished sign-ins cost the
// server nothing.
type pendingLogin struct {
	state, nonce, verifier string
}

func (l pendingLogin) cookie() string { return l.state + "." + l.nonce + "." + l.verifier }

func parsePendingLogin(value string) (pendingLogin, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return pendingLogin{}, false
	}
	return pendingLogin{state: parts[0], nonce: parts[1], verifier: parts[2]}, true
}

// oidcClient signs people in with the identity provider. Discovery runs on
// first use and again after a failure, so vrouter starts, and keeps serving
// client keys, while the provider is down.
type oidcClient struct {
	cfg      OIDCConfig
	issuer   *url.URL
	roles    map[string]string
	redirect string
	client   *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
}

func newOIDCClient(cfg OIDCConfig, publicURL string, client *http.Client) (*oidcClient, error) {
	issuer, err := url.Parse(cfg.Issuer)
	if cfg.Issuer == "" || err != nil || (issuer.Scheme != "https" && issuer.Scheme != "http") || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return nil, errors.New("VROUTER_OIDC_ISSUER must be the identity provider's http or https URL")
	}
	if issuer.Scheme == "http" && !loopbackHost(issuer.Hostname()) {
		return nil, errors.New("VROUTER_OIDC_ISSUER must use https unless the provider runs on this machine")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("VROUTER_OIDC_CLIENT_ID is required")
	}
	if publicURL == "" {
		return nil, errors.New("VROUTER_PUBLIC_URL is required, because sign-in returns to it")
	}
	roles, err := parseGroupRoles(cfg.Groups)
	if err != nil {
		return nil, err
	}
	return &oidcClient{cfg: cfg, issuer: issuer, roles: roles, redirect: publicURL + oidcCallbackPath, client: client}, nil
}

func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// parseGroupRoles reads group=role pairs. At least one group must grant
// admin, or nobody could manage shared accounts.
func parseGroupRoles(value string) (map[string]string, error) {
	roles := map[string]string{}
	admin := false
	for pair := range strings.SplitSeq(value, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		group, role, ok := strings.Cut(pair, "=")
		group, role = strings.TrimSpace(group), strings.ToLower(strings.TrimSpace(role))
		if !ok || group == "" || (role != roleAdmin && role != roleMember) {
			return nil, fmt.Errorf("VROUTER_OIDC_GROUPS: %q is not a group=admin or group=member pair", pair)
		}
		if _, duplicate := roles[group]; duplicate {
			return nil, fmt.Errorf("VROUTER_OIDC_GROUPS lists %q twice", group)
		}
		roles[group] = role
		admin = admin || role == roleAdmin
	}
	if !admin {
		return nil, errors.New("VROUTER_OIDC_GROUPS must map at least one group to admin, such as vrouter_admins=admin")
	}
	return roles, nil
}

// role is the highest role any of the groups grants, or "" for none.
func (c *oidcClient) role(groups []string) string {
	role := ""
	for _, group := range groups {
		switch c.roles[group] {
		case roleAdmin:
			return roleAdmin
		case roleMember:
			role = roleMember
		}
	}
	return role
}

func (c *oidcClient) issuerHost() string { return c.issuer.Host }

func (c *oidcClient) discover(ctx context.Context) (*oidc.Provider, oauth2.Config, error) {
	c.mu.Lock()
	provider := c.provider
	c.mu.Unlock()
	if provider == nil {
		discovered, err := oidc.NewProvider(oidc.ClientContext(ctx, c.client), c.cfg.Issuer)
		if err != nil {
			return nil, oauth2.Config{}, err
		}
		c.mu.Lock()
		c.provider = discovered
		c.mu.Unlock()
		provider = discovered
	}
	config := oauth2.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  c.redirect,
		Scopes:       []string{oidc.ScopeOpenID, oidc.ScopeEmail, oidc.ScopeProfile, "groups"},
	}
	return provider, config, nil
}

// exchange redeems an authorization code and verifies the ID token it
// returns. Groups come from the ID token, or from userinfo when the token
// carries none.
func (c *oidcClient) exchange(ctx context.Context, code string, login pendingLogin) (oidcIdentity, []string, error) {
	provider, config, err := c.discover(ctx)
	if err != nil {
		return oidcIdentity{}, nil, err
	}
	ctx = oidc.ClientContext(ctx, c.client)
	token, err := config.Exchange(ctx, code, oauth2.VerifierOption(login.verifier))
	if err != nil {
		return oidcIdentity{}, nil, fmt.Errorf("code exchange: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return oidcIdentity{}, nil, errors.New("the provider returned no ID token")
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: c.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return oidcIdentity{}, nil, fmt.Errorf("ID token: %w", err)
	}
	if !tokenEqual(idToken.Nonce, login.nonce) {
		return oidcIdentity{}, nil, errors.New("ID token nonce does not match")
	}
	var claims struct {
		Email             string    `json:"email"`
		Name              string    `json:"name"`
		PreferredUsername string    `json:"preferred_username"`
		Picture           string    `json:"picture"`
		Groups            *[]string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return oidcIdentity{}, nil, fmt.Errorf("ID token claims: %w", err)
	}
	if claims.Groups == nil {
		info, err := provider.UserInfo(ctx, config.TokenSource(ctx, token))
		if err != nil {
			return oidcIdentity{}, nil, fmt.Errorf("userinfo: %w", err)
		}
		if err := info.Claims(&claims); err != nil {
			return oidcIdentity{}, nil, fmt.Errorf("userinfo claims: %w", err)
		}
	}
	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}
	identity := oidcIdentity{issuer: idToken.Issuer, subject: idToken.Subject, email: claims.Email, name: name, picture: pictureURL(claims.Picture)}
	if claims.Groups == nil {
		return identity, nil, nil
	}
	return identity, *claims.Groups, nil
}

// startLogin sends the browser to the identity provider. A short-lived cookie
// ties the returning callback to this browser.
func (s *server) startLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	_, config, err := s.oidc.discover(ctx)
	if err != nil {
		slog.Warn("identity provider unavailable", "error", err)
		s.signInFailed(w, r, "unavailable")
		return
	}
	login := pendingLogin{state: randomToken(), nonce: randomToken(), verifier: oauth2.GenerateVerifier()}
	s.setCookie(w, loginCookie, login.cookie(), "/api/auth", int(loginLifetime/time.Second), http.SameSiteLaxMode)
	http.Redirect(w, r, config.AuthCodeURL(login.state, oidc.Nonce(login.nonce), oauth2.S256ChallengeOption(login.verifier)), http.StatusFound)
}

// signInFailed returns to the dashboard, which explains the reason.
func (s *server) signInFailed(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, s.cfg.PublicURL+"/?signin="+reason, http.StatusFound)
}

// finishLogin handles the provider's redirect back. It signs the browser in
// only for a verified identity in a mapped group.
func (s *server) finishLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.setCookie(w, loginCookie, "", "/api/auth", -1, http.SameSiteLaxMode)
	query := r.URL.Query()
	cookie, err := r.Cookie(loginCookie)
	if err != nil || len(cookie.Value) > 4*maxCookieBytes {
		s.signInFailed(w, r, "expired")
		return
	}
	login, ok := parsePendingLogin(cookie.Value)
	if !ok || query.Get("state") == "" || !tokenEqual(login.state, query.Get("state")) {
		s.signInFailed(w, r, "expired")
		return
	}
	if query.Get("error") != "" {
		s.signInFailed(w, r, "cancelled")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	identity, groups, err := s.oidc.exchange(ctx, query.Get("code"), login)
	if err != nil {
		slog.Warn("sign-in failed", "error", err)
		s.signInFailed(w, r, "failed")
		return
	}
	role := s.oidc.role(groups)
	if role == "" {
		slog.Info("sign-in refused: the user is in no group VROUTER_OIDC_GROUPS lists")
		s.signInFailed(w, r, "denied")
		return
	}
	user, err := s.signIn(ctx, identity, role)
	if errors.Is(err, errUserDisabled) {
		s.signInFailed(w, r, "disabled")
		return
	}
	if err == nil {
		err = s.beginSession(ctx, w, user)
	}
	if err != nil {
		slog.Error("sign-in not saved", "error", err)
		s.signInFailed(w, r, "failed")
		return
	}
	http.Redirect(w, r, s.cfg.PublicURL+"/", http.StatusFound)
}
