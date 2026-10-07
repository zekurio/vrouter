package gateway

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func oauthResponse(status int, value any) *http.Response {
	body, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body)))}
}

func oauthBody(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestHostedClaudeAuthorizationCode(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true, PublicURL: "https://router.example"})
	w := call(t, s, "POST", "/api/oauth/claude", "", "")
	var start struct{ ID, Flow, RedirectURI string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &start) != nil || start.Flow != "code" || start.RedirectURI != claudeManualRedirectURL {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if len(s.callbacks) != 0 {
		t.Fatal("hosted sign-in opened a loopback listener")
	}
	session := s.oauth[start.ID]
	if session.Return != "https://router.example/#accounts" {
		t.Fatal("incorrect return URL")
	}
	exchanges := 0
	s.client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		exchanges++
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.URL.String() != claudeTokenURL || body["redirect_uri"] != claudeManualRedirectURL || body["state"] != session.State || body["code_verifier"] != session.Verifier || body["code"] != "approved-code" {
			t.Fatal("unexpected code exchange")
		}
		return oauthResponse(200, map[string]any{"access_token": "test-access", "refresh_token": "test-refresh", "expires_in": 3600, "account": map[string]string{"uuid": "claude-account", "email_address": "admin@example.com"}}), nil
	})}
	for _, code := range []string{"bare-code", "code#wrong-state", "code#" + session.State + "#extra"} {
		w = call(t, s, "POST", "/api/oauth/sessions/"+start.ID+"/callback", oauthBody(t, map[string]string{"code": code}), "")
		if w.Code != 400 || exchanges != 0 {
			t.Fatal("invalid code reached token exchange")
		}
	}
	body := oauthBody(t, map[string]string{"code": "approved-code#" + session.State})
	w = call(t, s, "POST", "/api/oauth/sessions/"+start.ID+"/callback", body, "")
	if w.Code != 200 || exchanges != 1 || len(s.store.snapshot().Accounts) != 1 {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	if w = call(t, s, "POST", "/api/oauth/sessions/"+start.ID+"/callback", body, ""); w.Code != 400 || exchanges != 1 {
		t.Fatal("authorization code replayed")
	}
}

func TestHostedCodexDeviceAuthorization(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}}
	verifier := strings.Repeat("v", 43)
	digest := sha256.Sum256([]byte(verifier))
	for _, scenario := range []string{"success", "reconnect", "reconnect-mismatch", "wrong-audience", "expired", "bad-signature", "bad-pkce", "cancel", "expire"} {
		t.Run(scenario, func(t *testing.T) {
			s := testServer(t, Config{ExternalAuth: true, PublicURL: "https://router.example"})
			claims := map[string]any{"iss": openAIIssuer, "aud": codexNativeClientID, "sub": "user", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "workspace"}}
			if scenario == "wrong-audience" {
				claims["aud"] = "another-client"
			}
			if scenario == "expired" {
				claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
			}
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
			payload := base64.RawURLEncoding.EncodeToString([]byte(oauthBody(t, claims)))
			signed := header + "." + payload
			hash := sha256.Sum256([]byte(signed))
			signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "bad-signature" {
				signature[0] ^= 1
			}
			token := signed + "." + base64.RawURLEncoding.EncodeToString(signature)
			polls, exchanges := 0, 0
			s.client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case codexDeviceAPI + "usercode":
					return oauthResponse(200, map[string]string{"device_auth_id": "private-device-id", "user_code": "ABCD-12345", "interval": "5"}), nil
				case codexDeviceAPI + "token":
					polls++
					var body map[string]string
					if json.NewDecoder(r.Body).Decode(&body) != nil || body["device_auth_id"] != "private-device-id" || body["user_code"] != "ABCD-12345" {
						t.Fatal("wrong device polled")
					}
					if polls == 1 {
						return oauthResponse(403, map[string]string{"error": "pending"}), nil
					}
					challenge := base64.RawURLEncoding.EncodeToString(digest[:])
					if scenario == "bad-pkce" {
						challenge = "wrong"
					}
					return oauthResponse(200, map[string]string{"authorization_code": "approved-code", "code_verifier": verifier, "code_challenge": challenge}), nil
				case codexNativeTokenURL:
					exchanges++
					if r.ParseForm() != nil || r.Form.Get("redirect_uri") != codexDeviceRedirectURL || r.Form.Get("code_verifier") != verifier || r.Form.Get("code") != "approved-code" || r.Form.Get("client_id") != codexNativeClientID {
						t.Fatal("incorrect device code exchange")
					}
					return oauthResponse(200, map[string]any{"access_token": "test-access", "refresh_token": "test-refresh", "id_token": token, "expires_in": 3600}), nil
				case openAIJWKSURL:
					return oauthResponse(200, jwks), nil
				default:
					t.Fatalf("unexpected provider endpoint: %s", r.URL)
					return nil, nil
				}
			})}
			startBody := "{}"
			if strings.HasPrefix(scenario, "reconnect") {
				workspace := "workspace"
				if scenario == "reconnect-mismatch" {
					workspace = "other-workspace"
				}
				if err := s.store.update(func(d *diskState) error {
					d.Accounts = append(d.Accounts, storedAccount{ID: "saved", Provider: "codex", AuthMode: "codex", ClientID: codexNativeClientID, AccountID: workspace, Subject: "user", AccessToken: "old-access"})
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				startBody = `{"accountId":"saved"}`
			}
			w := call(t, s, "POST", "/api/oauth/codex", startBody, "")
			var start struct{ ID, Flow, UserCode string }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &start) != nil || start.Flow != "device" || start.UserCode != "ABCD-12345" || strings.Contains(w.Body.String(), "private-device-id") || len(s.callbacks) != 0 {
				t.Fatalf("start: %d %s", w.Code, w.Body.String())
			}
			path := "/api/oauth/sessions/" + start.ID
			if w = call(t, s, "POST", path+"/callback", `{"redirectUrl":"https://auth.openai.com/deviceauth/callback?code=injected"}`, ""); w.Code != 400 || exchanges != 0 {
				t.Fatal("browser submitted a device code")
			}
			call(t, s, "GET", path, "", "")
			if polls != 0 {
				t.Fatal("polled before provider interval")
			}
			if scenario == "cancel" || scenario == "expire" {
				if scenario == "cancel" {
					call(t, s, "DELETE", path, "", "")
				} else {
					session := s.oauth[start.ID]
					session.Expires = time.Now().Add(-time.Second)
					s.oauth[start.ID] = session
				}
				if w = call(t, s, "GET", path, "", ""); w.Code != 410 || polls != 0 {
					t.Fatal("cancelled or expired session polled")
				}
				return
			}
			for range 2 {
				session := s.oauth[start.ID]
				session.NextPoll = time.Time{}
				s.oauth[start.ID] = session
				w = call(t, s, "GET", path, "", "")
			}
			if scenario == "success" || scenario == "reconnect" {
				accounts := s.store.snapshot().Accounts
				if !strings.Contains(w.Body.String(), `"connected"`) || len(accounts) != 1 || accounts[0].AccountID != "workspace" || accounts[0].Subject != "user" || accounts[0].AuthMode != "codex" {
					t.Fatalf("finish: %s, accounts %d", w.Body.String(), len(accounts))
				}
				// A device ID token without nonce must still fail browser verification.
				if _, err := s.verifyIDToken(t.Context(), token, codexNativeClientID, "required-nonce"); err == nil {
					t.Fatal("browser sign-in accepted an ID token without nonce")
				}
			} else if scenario == "reconnect-mismatch" {
				accounts := s.store.snapshot().Accounts
				if !strings.Contains(w.Body.String(), `"error"`) || len(accounts) != 1 || accounts[0].AccountID != "other-workspace" || accounts[0].AccessToken != "old-access" {
					t.Fatal("reconnect replaced a different workspace")
				}
			} else if !strings.Contains(w.Body.String(), `"error"`) || len(s.store.snapshot().Accounts) != 0 {
				t.Fatalf("invalid device result accepted: %s", w.Body.String())
			}
			call(t, s, "GET", path, "", "")
			if polls != 2 || exchanges > 1 {
				t.Fatal("finished session polled or exchanged again")
			}
		})
	}
}
