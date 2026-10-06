package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

type oauthTransport func(*http.Request) (*http.Response, error)

func (f oauthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func oauthReply(status int, value any) *http.Response {
	raw, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}
}
func oauthServer(t *testing.T) *server {
	t.Helper()
	h, err := New(Config{DataDir: t.TempDir(), APIKey: "client", AdminToken: "admin"}, fstest.MapFS{"index.html": {Data: []byte("vrouter")}})
	if err != nil {
		t.Fatal(err)
	}
	s := h.(*server)
	t.Cleanup(func() { s.Close() })
	return s
}
func oauthCall(s *server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}
func TestNativeOAuthRejectsCallbackSubstitutionAndCancels(t *testing.T) {
	for _, kind := range []string{"host", "port", "path", "state", "fragment", "duplicate", "wrong-client", "denied", "cancelled", "expired"} {
		t.Run(kind, func(t *testing.T) {
			s := oauthServer(t)
			id, _ := nativeCodexStart(t, s, "")
			session := s.oauth[id]
			u, _ := url.Parse(session.RedirectURI)
			q := url.Values{"code": {"code"}, "state": {session.State}, "client_id": {codexNativeClientID}}
			switch kind {
			case "host":
				u.Host = "localhost:" + u.Port()
			case "port":
				u.Host = "127.0.0.1:1"
			case "path":
				u.Path = "/wrong"
			case "state":
				q.Set("state", "wrong")
			case "fragment":
				u.Fragment = "bad"
			case "duplicate":
				q.Add("state", session.State)
			case "wrong-client":
				q.Set("client_id", "wrong")
			case "denied":
				q.Set("error", "access_denied")
			case "cancelled":
				oauthCall(s, "DELETE", "/api/oauth/sessions/"+id, "")
			case "expired":
				session.Expires = time.Now().Add(-time.Second)
				s.oauth[id] = session
			}
			s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
				t.Error("invalid callback reached network")
				return oauthReply(500, nil), nil
			})
			u.RawQuery = q.Encode()
			body, _ := json.Marshal(map[string]string{"redirectUrl": u.String()})
			w := oauthCall(s, "POST", "/api/oauth/sessions/"+id+"/callback", string(body))
			if w.Code == 200 {
				t.Fatalf("accepted %s", kind)
			}
			if len(s.store.snapshot().Accounts) != 0 {
				t.Fatal("invalid identity stored")
			}
		})
	}
}
func TestNativeRefreshRotatesOnceAndRetainsGrant(t *testing.T) {
	s := oauthServer(t)
	err := s.store.update(func(d *diskState) error {
		d.Accounts = []storedAccount{{ID: "a", Provider: "codex", AuthMode: "codex", ClientID: codexNativeClientID, AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute), Scopes: []string{"openid", "offline_access"}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		r.ParseForm()
		if r.URL.String() != codexNativeTokenURL || r.Form.Has("resource") || r.Form.Get("client_id") != codexNativeClientID || r.Form.Has("scope") {
			t.Error("wrong refresh contract")
		}
		return oauthReply(200, map[string]any{"access_token": "new", "refresh_token": "rotated", "expires_in": 3600}), nil
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := s.accessAccount(context.Background(), "a")
			if err != nil || a.AccessToken != "new" || !routableAuth(a) {
				t.Error("refresh failed")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 || s.store.snapshot().Accounts[0].RefreshToken != "rotated" {
		t.Fatal("refresh rotation raced")
	}
}
func TestNativeIdentityOnlyConnectionCannotInfer(t *testing.T) {
	a := storedAccount{AuthMode: "chatgpt", Scopes: []string{"openid", "email"}}
	if routableAuth(a) {
		t.Fatal("identity grant treated as inference grant")
	}
}

func TestNativeCallbackListenerAndCancellation(t *testing.T) {
	s := oauthServer(t)
	id, _ := nativeCodexStart(t, s, "")
	session := s.oauth[id]
	s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		t.Error("denied callback contacted provider")
		return oauthReply(500, nil), nil
	})
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(session.RedirectURI + "?" + url.Values{"state": {session.State}, "error": {"access_denied"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 400 || !strings.Contains(string(body), "Sign-in was not approved") || response.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("callback page did not report denial safely")
	}
	if w := oauthCall(s, "GET", "/api/oauth/sessions/"+id, ""); !strings.Contains(w.Body.String(), `"status":"error"`) {
		t.Fatal("denied session remains pending")
	}
	id, _ = nativeCodexStart(t, s, "")
	session = s.oauth[id]
	w := oauthCall(s, "DELETE", "/api/oauth/sessions/"+id, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = oauthCall(s, "GET", "/api/oauth/sessions/"+id, ""); w.Code != 410 {
		t.Fatal("cancelled session remains usable")
	}
}
