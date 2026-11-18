package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	claudeManualRedirectURL = "https://platform.claude.com/oauth/code/callback"
	codexDeviceURL          = "https://auth.openai.com/codex/device"
	codexDeviceAPI          = "https://auth.openai.com/api/accounts/deviceauth/"
	codexDeviceRedirectURL  = "https://auth.openai.com/deviceauth/callback"
)

func (s *server) hostedOAuth(r *http.Request) bool {
	u, err := url.Parse(s.appURL(r))
	if err != nil {
		return true
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return host != "localhost" && (ip == nil || !ip.IsLoopback())
}

// Codex's native device flow returns a code and PKCE pair through its fixed
// HTTPS endpoint. Nothing is redirected to the gateway or browser's localhost.
func (s *server) deviceRequest(ctx context.Context, path string, input, output any) (int, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexDeviceAPI+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, errors.New("device sign-in endpoint unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, readBoundedJSON(resp.Body, output)
}

func (s *server) startDeviceOAuth(ctx context.Context, session *oauthSession) error {
	var result struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		UserCodeAlt  string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
	}
	status, err := s.deviceRequest(ctx, "usercode", map[string]string{"client_id": session.ClientID}, &result)
	if err != nil || status != http.StatusOK {
		return errors.New("Could not start Codex device sign-in. Enable device code login in your ChatGPT security settings or workspace permissions, then retry.")
	}
	if result.UserCode == "" {
		result.UserCode = result.UserCodeAlt
	}
	if result.DeviceAuthID == "" || len(result.DeviceAuthID) > 4096 || result.UserCode == "" || len(result.UserCode) > 128 {
		return errors.New("Codex returned an incomplete device sign-in. Try again.")
	}
	// The native endpoint encodes interval as a string; also accept a number.
	interval := 5
	if len(result.Interval) > 0 {
		interval, err = strconv.Atoi(strings.Trim(string(result.Interval), `"`))
		if err != nil || interval < 1 || interval > 60 {
			return errors.New("Codex returned an invalid device polling interval. Try again.")
		}
	}
	session.Flow = "device"
	session.DeviceAuthID = result.DeviceAuthID
	session.UserCode = result.UserCode
	session.PollInterval = time.Duration(interval) * time.Second
	session.NextPoll = time.Now().Add(session.PollInterval)
	session.Expires = time.Now().Add(15 * time.Minute)
	session.RedirectURI = codexDeviceRedirectURL
	session.Nonce = "" // Device authorization has no OIDC nonce parameter.
	session.Verifier = ""
	return nil
}

// Called by authenticated status polls under oauthMu. Closing or switching the
// dialog deletes the session and stops further polling without a background job.
func (s *server) pollDeviceOAuth(ctx context.Context, id string) {
	session := s.oauth[id]
	if time.Now().Before(session.NextPoll) {
		return
	}
	session.NextPoll = time.Now().Add(session.PollInterval)
	s.oauth[id] = session
	var result struct {
		Code      string `json:"authorization_code"`
		Verifier  string `json:"code_verifier"`
		Challenge string `json:"code_challenge"`
	}
	status, err := s.deviceRequest(ctx, "token", map[string]string{"device_auth_id": session.DeviceAuthID, "user_code": session.UserCode}, &result)
	// The native protocol uses 403 and 404 while awaiting user approval.
	// Transport failures, throttling and server errors may be retried.
	if status == 0 || status == 403 || status == 404 || status == 429 || status >= 500 {
		session.NextPoll = time.Now().Add(max(session.PollInterval, 5*time.Second))
		s.oauth[id] = session
		return
	}
	digest := sha256.Sum256([]byte(result.Verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if err != nil || status != http.StatusOK || result.Code == "" || len(result.Verifier) < 43 || len(result.Verifier) > 128 || !tokenEqual(result.Challenge, challenge) {
		session.Error = "Codex device sign-in failed. Start a new sign-in."
		session.DeviceAuthID = ""
		session.UserCode = ""
		s.oauth[id] = session
		return
	}
	session.Verifier = result.Verifier
	s.oauth[id] = session
	// Only this fixed provider response can submit a device authorization code;
	// the browser callback handler explicitly refuses device sessions.
	if err := s.completeOAuth(ctx, id, url.Values{"code": {result.Code}, "state": {session.State}}); err != nil {
		session = s.oauth[id]
		session.Error = "Could not finish Codex device sign-in. Start a new sign-in."
		session.Verifier = ""
		session.DeviceAuthID = ""
		session.UserCode = ""
		s.oauth[id] = session
	}
}
