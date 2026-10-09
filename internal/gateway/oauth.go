package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type oauthSession struct {
	Provider, State, Return, Verifier, Nonce, RedirectURI, ClientID, AccountID, AuthMode string
	Expires                                                                              time.Time
	Submitted, Completed                                                                 bool
	Error                                                                                string
	Flow                                                                                 string
	DeviceAuthID, UserCode                                                               string
	PollInterval                                                                         time.Duration
	NextPoll                                                                             time.Time
}

func randomToken() string {
	var b [32]byte
	_, err := rand.Read(b[:])
	if err != nil {
		panic("system random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func (s *server) startOAuth(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("provider")
	if p != "codex" && p != "claude" {
		writeJSON(w, 400, map[string]string{"error": "Choose Codex or Claude"})
		return
	}
	var input struct {
		AccountID string `json:"accountId"`
		AuthMode  string `json:"authMode"`
	}
	if r.Body != nil {
		err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input)
		if err != nil && err != io.EOF {
			writeJSON(w, 400, map[string]string{"error": "Invalid sign-in request"})
			return
		}
	}
	requested := strings.ToLower(strings.TrimSpace(input.AuthMode))
	if requested != "" {
		if p != "codex" {
			writeJSON(w, 400, map[string]string{"error": "This sign-in method is not supported for Claude"})
			return
		}
		if requested != "codex" {
			writeJSON(w, 400, map[string]string{"error": "Unsupported sign-in method. Use Codex sign-in."})
			return
		}
	}
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	s.sweepOAuth()
	if len(s.oauth) >= 16 {
		writeJSON(w, 429, map[string]string{"error": "Too many sign-in sessions. Cancel one or wait for expiry."})
		return
	}
	session := oauthSession{Provider: p, State: randomToken(), Verifier: randomToken(), Nonce: randomToken(), Expires: time.Now().Add(5 * time.Minute), ClientID: codexNativeClientID, AuthMode: "codex"}
	endpoint := codexNativeAuthorizeURL
	scope := codexNativeScope
	query := url.Values{}
	if p == "claude" {
		session.ClientID = claudeClientID
		session.AuthMode = "oauth"
		endpoint = "https://claude.ai/oauth/authorize"
		scope = claudeScope
	}
	if input.AccountID != "" {
		var selected *storedAccount
		for _, a := range s.store.snapshot().Accounts {
			if a.ID == input.AccountID && a.Provider == p {
				candidate := a
				selected = &candidate
				break
			}
		}
		if selected == nil {
			missing := "Saved Claude registration not found"
			if p == "codex" {
				missing = "Saved Codex registration not found"
			}
			writeJSON(w, 404, map[string]string{"error": missing})
			return
		}
		switch p {
		case "claude":
			if selected.AuthMode != "oauth" {
				writeJSON(w, 404, map[string]string{"error": "Saved Claude registration not found"})
				return
			}
			// Reconnect runs the shared Claude sign-in client used by token
			// refresh. A record stored with a different client cannot complete
			// this flow, so it is refused instead of authorizing the wrong
			// registration.
			if selected.ClientID != claudeClientID {
				writeJSON(w, 400, map[string]string{"error": "Saved Claude connection uses a different sign-in client and cannot be renewed"})
				return
			}
			session.AccountID = selected.ID
			if selected.Email != "" {
				query.Set("login_hint", selected.Email)
			}
		case "codex":
			if selected.AuthMode != "codex" {
				writeJSON(w, 400, map[string]string{"error": "This connection cannot be renewed. Add the account using Codex sign-in."})
				return
			}
			if selected.ClientID != codexNativeClientID {
				writeJSON(w, 400, map[string]string{"error": "Saved Codex connection uses a different sign-in client and cannot be renewed"})
				return
			}
			// Reconnect binds the previously verified workspace and user.
			// Imported records without a subject must be added again instead.
			if selected.AccountID == "" || selected.Subject == "" {
				writeJSON(w, 400, map[string]string{"error": "Saved Codex connection has no verified user identity to renew. Add the account again instead."})
				return
			}
			session.AccountID = selected.ID
		}
	}
	if p == "codex" {
		query.Set("nonce", session.Nonce)
		query.Set("id_token_add_organizations", "true")
		query.Set("codex_cli_simplified_flow", "true")
		query.Set("originator", codexNativeOriginator)
	}
	id := randomToken()
	session.Return = s.appURL(r) + "/#accounts"
	if s.hostedOAuth(r) && p == "codex" {
		if err := s.startDeviceOAuth(r.Context(), &session); err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		s.saveOAuthSession(id, session)
		writeJSON(w, 200, map[string]any{"id": id, "provider": p, "flow": session.Flow, "url": codexDeviceURL, "userCode": session.UserCode, "expiresAt": session.Expires})
		return
	}
	session.Flow = "loopback"
	var uri string
	var err error
	if s.hostedOAuth(r) && p == "claude" {
		session.Flow = "code"
		uri = claudeManualRedirectURL
	} else {
		uri, err = s.openCallback(id, p, session.AuthMode)
	}
	if err != nil {
		message := "Could not open the loopback callback listener. Retry the sign-in."
		switch {
		case p == "claude":
			message = "Could not open the loopback callback listener. Close any application using Claude's port 54545 and retry."
		case session.AuthMode == "codex":
			message = "Could not open the local Codex callback on 127.0.0.1:1455. Close Codex CLI or cancel the pending sign-in using that port, then retry."
		}
		writeJSON(w, 502, map[string]string{"error": message})
		return
	}
	session.RedirectURI = uri
	digest := sha256.Sum256([]byte(session.Verifier))
	query.Set("client_id", session.ClientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", uri)
	query.Set("scope", scope)
	query.Set("state", session.State)
	query.Set("code_challenge_method", "S256")
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(digest[:]))
	s.saveOAuthSession(id, session)
	writeJSON(w, 200, map[string]any{"id": id, "provider": p, "flow": session.Flow, "url": endpoint + "?" + query.Encode(), "redirectUri": uri, "expiresAt": session.Expires})
}

func (s *server) saveOAuthSession(id string, session oauthSession) {
	s.oauth[id] = session
	time.AfterFunc(time.Until(session.Expires)+time.Second, func() { s.oauthMu.Lock(); defer s.oauthMu.Unlock(); s.sweepOAuth() })
}
func (s *server) session(w http.ResponseWriter, r *http.Request) (oauthSession, bool) {
	session, ok := s.oauth[r.PathValue("id")]
	if !ok || time.Now().After(session.Expires) {
		s.sweepOAuth()
		writeJSON(w, 410, map[string]string{"error": "Sign-in expired. Start a new connection."})
		return oauthSession{}, false
	}
	return session, true
}
func (s *server) oauthStatus(w http.ResponseWriter, r *http.Request) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	session, ok := s.session(w, r)
	if !ok {
		return
	}
	if session.Flow == "device" && !session.Completed && session.Error == "" && !session.Submitted {
		s.pollDeviceOAuth(r.Context(), r.PathValue("id"))
		session = s.oauth[r.PathValue("id")]
	}
	status := "pending"
	if session.Completed {
		status = "connected"
	} else if session.Error != "" {
		status = "error"
	}
	writeJSON(w, 200, map[string]string{"status": status, "error": session.Error})
}
func (s *server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RedirectURL string `json:"redirectUrl"`
		Code        string `json:"code"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body) != nil {
		writeJSON(w, 400, map[string]string{"error": "Enter the full callback URL"})
		return
	}
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	session, ok := s.session(w, r)
	if !ok {
		return
	}
	if session.Flow == "device" {
		writeJSON(w, 400, map[string]string{"error": "Finish device sign-in with the provider. No callback is needed."})
		return
	}
	var q url.Values
	if session.Flow == "code" {
		code, state, ok := strings.Cut(strings.TrimSpace(body.Code), "#")
		if !ok || code == "" || state == "" || strings.ContainsAny(code+state, " \t\r\n#") {
			writeJSON(w, 400, map[string]string{"error": "Paste the complete authorization code from Claude, including # and the text after it"})
			return
		}
		q = url.Values{"code": {code}, "state": {state}}
	} else {
		u, err := url.Parse(strings.TrimSpace(body.RedirectURL))
		expected, _ := url.Parse(session.RedirectURI)
		if err != nil || u.User != nil || u.Fragment != "" || u.Scheme != expected.Scheme || u.Host != expected.Host || u.Path != expected.Path {
			writeJSON(w, 400, map[string]string{"error": "Callback address does not match this sign-in"})
			return
		}
		q = u.Query()
	}
	if err := s.completeOAuth(r.Context(), r.PathValue("id"), q); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "connected"})
}

// oauthMu is held throughout callback validation and exchange. A submitted
// authorization code is consumed even when exchange fails, and cannot replay.
func (s *server) completeOAuth(ctx context.Context, id string, q url.Values) error {
	session, ok := s.oauth[id]
	if !ok || time.Now().After(session.Expires) {
		return errors.New("Sign-in expired")
	}
	if session.Submitted || session.Completed {
		return errors.New("Callback already submitted")
	}
	if len(q["state"]) != 1 || !tokenEqual(q.Get("state"), session.State) || len(q["code"]) > 1 || len(q["client_id"]) > 1 || len(q["error"]) > 1 {
		return errors.New("Callback does not match this sign-in")
	}
	if q.Get("code") == "" && q.Get("error") == "" {
		return errors.New("Callback has no authorization result")
	}
	session.Submitted = true
	s.oauth[id] = session
	fail := func(message string) error {
		session.Error = message
		session.Verifier = ""
		session.Nonce = ""
		s.oauth[id] = session
		s.sweepOAuth()
		return errors.New(message)
	}
	if q.Get("error") != "" {
		return fail("Sign-in was not approved. Start again to grant access.")
	}
	if session.Provider == "codex" {
		if session.AuthMode != "codex" {
			return fail("Unsupported sign-in method. Start a new Codex sign-in.")
		}
		// A native callback need not name the public client, but cannot
		// substitute a different one.
		if issued := q.Get("client_id"); issued != "" && issued != session.ClientID {
			return fail("Returned client ID does not match the native sign-in client")
		}
	}
	fields := url.Values{"grant_type": {"authorization_code"}, "client_id": {session.ClientID}, "code": {q.Get("code")}, "code_verifier": {session.Verifier}, "redirect_uri": {session.RedirectURI}}
	endpoint := codexNativeTokenURL
	if session.Provider == "claude" {
		endpoint = claudeTokenURL
		fields.Set("state", session.State)
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	tokens, err := s.tokenRequest(ctx, endpoint, fields, session.Provider == "claude")
	if err != nil {
		return fail("Could not exchange the authorization code. Start a new sign-in.")
	}
	a := storedAccount{ID: randomToken(), Provider: session.Provider, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, IDToken: tokens.IDToken, ClientID: session.ClientID, ExpiresAt: time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second), CreatedAt: time.Now().UTC(), AuthMode: "oauth"}
	if tokens.Scope != nil {
		a.Scopes = strings.Fields(*tokens.Scope)
	}
	if session.Provider == "codex" {
		var identity verifiedIdentity
		if session.Flow == "device" {
			identity, err = s.verifyDeviceIDToken(ctx, tokens.IDToken, session.ClientID)
		} else {
			identity, err = s.verifyIDToken(ctx, tokens.IDToken, session.ClientID, session.Nonce)
		}
		if err != nil {
			return fail("Provider identity could not be verified. Start a new sign-in.")
		}
		// A native login without a workspace cannot route subscription
		// traffic and cannot be bound safely, so it is refused.
		if identity.WorkspaceID == "" {
			return fail("Codex sign-in did not return a workspace identity. Start a new sign-in.")
		}
		a.AuthMode = "codex"
		a.AccountID = identity.WorkspaceID
		a.Subject = identity.Subject
		a.Plan = identity.PlanType
		a.Email = identity.Email
		a.Label = identity.Name
	} else {
		a.AccountID = tokens.Account.UUID
		a.Email = tokens.Account.Email
	}
	if a.Label == "" {
		a.Label = a.Email
	}
	if a.Label == "" {
		a.Label = providerLabel(a.Provider)
	}
	err = s.store.update(func(d *diskState) error {
		selected, same := -1, -1
		for i, old := range d.Accounts {
			if session.AccountID != "" && old.ID == session.AccountID {
				selected = i
				continue
			}
			if same < 0 && oauthSameAccount(old, a) {
				same = i
			}
		}
		// A selected reconnect must still find its record, and the record must
		// still describe the freshly verified identity. Anything else is
		// refused rather than silently replaced.
		if session.AccountID != "" {
			if selected < 0 {
				return errors.New("selected account was removed")
			}
			old := d.Accounts[selected]
			if err := oauthReconnectAllowed(old, a); err != nil {
				return err
			}
			a = oauthReplacement(old, a)
			d.Accounts[selected] = a
			return nil
		}
		if same >= 0 {
			a = oauthReplacement(d.Accounts[same], a)
			d.Accounts[same] = a
			return nil
		}
		d.Accounts = append(d.Accounts, a)
		return nil
	})
	if err != nil {
		return fail("Could not save the verified connection")
	}
	s.catalogMu.Lock()
	delete(s.catalogs, a.ID)
	s.catalogMu.Unlock()
	s.quotaMu.Lock()
	delete(s.quotas, a.ID)
	s.quotaMu.Unlock()
	session.Completed = true
	session.Verifier = ""
	session.Nonce = ""
	session.DeviceAuthID = ""
	session.UserCode = ""
	s.oauth[id] = session
	s.sweepOAuth()
	return nil
}

// oauthSameAccount reports whether a freshly authorized credential is the same
// saved identity, so a repeated sign-in replaces that record instead of
// appending a duplicate. Codex requires the same verified workspace and user
// subject, so users of a shared workspace are never folded into one entry.
func oauthSameAccount(old, fresh storedAccount) bool {
	if old.Provider != fresh.Provider || old.AuthMode != fresh.AuthMode || fresh.ClientID == "" || old.ClientID != fresh.ClientID {
		return false
	}
	if old.Provider == "claude" && old.AuthMode == "oauth" {
		return claudeIdentityMatch(old, fresh)
	}
	if old.Provider == "codex" && old.AuthMode == "codex" {
		return old.AccountID != "" && old.AccountID == fresh.AccountID &&
			old.Subject != "" && old.Subject == fresh.Subject
	}
	return false
}

// oauthReconnectAllowed validates a user-selected reconnect. It refuses a
// saved registration that changed while the sign-in was pending, or one that
// no longer describes the verified identity. Native Codex records must already
// bind both a verified workspace and user subject; an email match is never
// sufficient and an unverified import is never rebound.
func oauthReconnectAllowed(old, fresh storedAccount) error {
	if old.Provider != fresh.Provider || old.AuthMode != fresh.AuthMode {
		return errors.New("saved account changed during sign-in")
	}
	if fresh.ClientID == "" || old.ClientID != fresh.ClientID {
		return errors.New("saved registration uses a different sign-in client")
	}
	if old.Provider == "claude" && old.AuthMode == "oauth" {
		if !claudeIdentityMatch(old, fresh) {
			return errors.New("verified identity does not match the saved account")
		}
		return nil
	}
	if old.Provider == "codex" && old.AuthMode == "codex" {
		// Native reconnect requires the workspace and user subject that were
		// stored by a verified sign-in. An import without a verified subject
		// must be added again, even for the same workspace.
		if old.AccountID == "" || old.Subject == "" {
			return errors.New("saved account has no verified identity to reconnect")
		}
		if fresh.AccountID != old.AccountID || fresh.Subject != old.Subject {
			return errors.New("verified identity does not match the saved account")
		}
		return nil
	}
	return errors.New("unsupported sign-in method")
}

// claudeIdentityMatch compares a saved Claude identity with the identity the
// token exchange returned. A stored account UUID must match exactly and is
// never overridden by an email match; only a record without a UUID may fall
// back to a case-insensitive email comparison. A record with neither is
// treated as unverifiable.
func claudeIdentityMatch(old, fresh storedAccount) bool {
	if old.AccountID != "" {
		return fresh.AccountID != "" && old.AccountID == fresh.AccountID
	}
	return old.Email != "" && fresh.Email != "" && strings.EqualFold(old.Email, fresh.Email)
}

// oauthReplacement keeps the record's identity, creation time, disabled
// state, and user-chosen label while adopting the new credentials.
func oauthReplacement(old, fresh storedAccount) storedAccount {
	fresh.ID = old.ID
	fresh.CreatedAt = old.CreatedAt
	fresh.Disabled = old.Disabled
	if old.Label != "" {
		fresh.Label = old.Label
	}
	return fresh
}

func (s *server) cancelOAuth(w http.ResponseWriter, r *http.Request) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	delete(s.oauth, r.PathValue("id"))
	s.sweepOAuth()
	writeJSON(w, 200, map[string]string{"status": "cancelled"})
}
