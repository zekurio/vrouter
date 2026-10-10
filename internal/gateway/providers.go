package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	claudeTokenURL = "https://platform.claude.com/v1/oauth/token" //nolint:gosec // public OAuth endpoint, not a credential
	claudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeScope    = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

	// Codex CLI native sign-in. This is the public client and flow used by
	// codex-rs login: no dynamic client ID, no SIWC resource or host
	// parameters, and the fixed loopback callback on port 1455.
	codexNativeAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	codexNativeTokenURL     = "https://auth.openai.com/oauth/token" //nolint:gosec // public OAuth endpoint, not a credential
	codexNativeClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexNativeScope        = "openid profile email offline_access"
	codexNativeOriginator   = "vrouter"
	codexNativeCallbackPort = "1455"
)

type tokenResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	IDToken      string  `json:"id_token"`
	TokenType    string  `json:"token_type"`
	ExpiresIn    int64   `json:"expires_in"`
	Scope        *string `json:"scope"`
	Account      struct {
		UUID  string `json:"uuid"`
		Email string `json:"email_address"`
	} `json:"account"`
}

type providerError struct {
	Status  int
	RetryAt time.Time
}

var (
	errProviderUnavailable = errors.New("provider is unavailable")
	errProviderTimeout     = errors.New("provider request timed out")
)

func providerHTTPError(resp *http.Response) *providerError {
	e := &providerError{Status: resp.StatusCode}
	value := resp.Header.Get("Retry-After")
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		e.RetryAt = time.Now().Add(time.Duration(seconds) * time.Second)
	} else if date, err := http.ParseTime(value); err == nil {
		e.RetryAt = date
	}
	return e
}

func (e *providerError) Error() string { return fmt.Sprintf("provider returned HTTP %d", e.Status) }

func readBoundedJSON(body io.Reader, out any) error {
	data, err := io.ReadAll(io.LimitReader(body, (2<<20)+1))
	if err != nil {
		return errors.New("could not read provider response")
	}
	if len(data) > 2<<20 {
		return errors.New("provider response exceeds size limit")
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("invalid provider response")
	}
	return nil
}

func (s *server) tokenRequest(ctx context.Context, endpoint string, fields url.Values, asJSON bool) (tokenResponse, error) {
	var tokens tokenResponse
	body := fields.Encode()
	contentType := "application/x-www-form-urlencoded"
	if asJSON {
		values := map[string]string{}
		for k := range fields {
			values[k] = fields.Get(k)
		}
		raw, _ := json.Marshal(values)
		body = string(raw)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return tokens, errors.New("invalid token endpoint")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return tokens, errors.New("token endpoint unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tokens, providerHTTPError(resp)
	}
	if err := readBoundedJSON(resp.Body, &tokens); err != nil {
		return tokens, err
	}
	if tokens.AccessToken == "" || tokens.ExpiresIn <= 0 || tokens.ExpiresIn > 31536000 || (tokens.TokenType != "" && !strings.EqualFold(tokens.TokenType, "Bearer")) {
		return tokens, errors.New("incomplete token response")
	}
	return tokens, nil
}

func (s *server) accessAccount(ctx context.Context, id string) (storedAccount, error) {
	// A refresh for another account must not block requests whose credentials
	// are already valid. Re-read under the lock before rotating any tokens.
	if err := ctx.Err(); err != nil {
		return storedAccount{}, err
	}
	for _, a := range s.store.snapshot().Accounts {
		if a.ID == id && !a.Disabled && routableAuth(a) && accountTokenFresh(a) {
			return a, nil
		}
	}
	// The disk store prevents another process from racing rotating refresh tokens.
	// This lock coalesces refreshes inside this process and serializes removal.
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return storedAccount{}, err
	}
	var a storedAccount
	found := false
	for _, candidate := range s.store.snapshot().Accounts {
		if candidate.ID == id {
			a = candidate
			found = true
			break
		}
	}
	if !found || a.Disabled {
		return a, errors.New("account is no longer enabled")
	}
	if !routableAuth(a) {
		return a, errors.New("unsupported account authentication; add the account using Codex sign-in")
	}
	if accountTokenFresh(a) {
		return a, nil
	}
	if a.RefreshToken == "" {
		return a, errors.New("account needs sign-in")
	}
	return s.refreshAccount(ctx, a)
}

// refreshAccount rotates an expired access token and saves it. The caller
// holds refreshMu.
func (s *server) refreshAccount(ctx context.Context, a storedAccount) (storedAccount, error) {
	fields := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.RefreshToken}}
	var endpoint string
	switch a.AuthMode {
	case "codex":
		endpoint = codexNativeTokenURL
		fields.Set("client_id", codexNativeClientID)
	case "oauth":
		endpoint = claudeTokenURL
		fields.Set("client_id", claudeClientID)
		fields.Set("scope", claudeScope)
	default:
		return a, errors.New("unsupported account authentication")
	}
	tokens, err := s.tokenRequest(ctx, endpoint, fields, a.Provider == "claude")
	if err != nil {
		return a, err
	}
	a.AccessToken = tokens.AccessToken
	a.ExpiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	if tokens.RefreshToken != "" {
		a.RefreshToken = tokens.RefreshToken
	}
	// Retain the identity-validated ID token. A refresh
	// does not establish a new identity.
	if tokens.Scope != nil {
		a.Scopes = strings.Fields(*tokens.Scope)
	}
	err = s.store.update(func(d *diskState) error {
		for i := range d.Accounts {
			if d.Accounts[i].ID == a.ID {
				d.Accounts[i] = a
				return nil
			}
		}
		return errors.New("account was removed")
	})
	if err != nil {
		return storedAccount{}, errors.New("could not persist refreshed credentials")
	}
	return a, nil
}

func accountTokenFresh(a storedAccount) bool {
	return a.AccessToken != "" && (a.AuthMode == "api_key" || (!a.ExpiresAt.IsZero() && time.Until(a.ExpiresAt) > time.Minute))
}

func routableAuth(a storedAccount) bool {
	return a.AuthMode == "api_key" ||
		(a.Provider == "codex" && a.AuthMode == "codex") ||
		(a.Provider == "claude" && a.AuthMode == "oauth")
}

func providerHeaders(req *http.Request, a storedAccount) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vrouter/0.2")
	if a.Provider == "claude" {
		claudeProviderHeaders(req, a)
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	if a.AuthMode == "codex" {
		req.Header.Set("Originator", "codex_cli_rs")
		if a.AccountID != "" {
			req.Header.Set("Chatgpt-Account-Id", a.AccountID)
		}
	}
}

func claudeProviderHeaders(req *http.Request, a storedAccount) {
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if a.AuthMode == "api_key" {
		req.Header.Set("X-Api-Key", a.AccessToken)
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	if a.AuthMode == "oauth" && req.URL.Host == "api.anthropic.com" &&
		(req.URL.Path == "/api/oauth/usage" || (strings.HasPrefix(req.URL.Path, "/api/organizations/") && strings.HasSuffix(req.URL.Path, "/reset_rate_limits"))) {
		// Like T3 Code, identify the native CLI protocol on reset reads
		// and claims. Claude otherwise responds eligible:false, surface.
		req.Header.Set("User-Agent", claudeResetUserAgent)
	}
}

func (s *server) providerJSON(ctx context.Context, a storedAccount, target string, out any) error {
	current, err := s.accessAccount(ctx, a.ID)
	if err != nil {
		return err
	}
	return s.readProviderJSON(ctx, current, target, out)
}

func (s *server) readProviderJSON(ctx context.Context, current storedAccount, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return errors.New("invalid provider endpoint")
	}
	providerHeaders(req, current)
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return errProviderTimeout
		}
		return errProviderUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return providerHTTPError(resp)
	}
	return readBoundedJSON(resp.Body, out)
}

func requestJSON(ctx context.Context, target string, payload map[string]json.RawMessage) (*http.Request, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, err
}

func providerInferenceRequest(ctx context.Context, target string, payload map[string]json.RawMessage, account storedAccount) (*http.Request, error) {
	prepared, err := claudeOAuthPayload(payload, account)
	if err != nil {
		return nil, err
	}
	req, err := requestJSON(ctx, target, prepared)
	if err == nil {
		providerHeaders(req, account)
	}
	return req, err
}
