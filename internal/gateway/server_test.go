package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func nativeCall(s http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func nativeAccount(id string) storedAccount {
	return storedAccount{ID: id, Provider: "codex", AuthMode: "codex", ClientID: codexNativeClientID, AccessToken: "provider-" + id, ExpiresAt: time.Now().Add(time.Hour), Scopes: []string{"openid", "offline_access"}}
}
func nativeSeed(t *testing.T, s *server, accounts ...storedAccount) {
	t.Helper()
	if err := s.store.update(func(d *diskState) error { d.Accounts = accounts; return nil }); err != nil {
		t.Fatal(err)
	}
}
func nativeMock(s *server, handler oauthTransport) {
	s.client.Transport = handler
	s.streamClient.Transport = handler
}
func nativeModels(ids ...string) any {
	models := []map[string]string{}
	for _, id := range ids {
		models = append(models, map[string]string{"slug": id, "display_name": id, "visibility": "list"})
	}
	return map[string]any{"models": models}
}
func TestNativeManagementAndClientBoundaries(t *testing.T) {
	s := oauthServer(t)
	s.client.Transport = oauthTransport(func(r *http.Request) (*http.Response, error) {
		t.Fatal("empty store made outbound request")
		return nil, nil
	})
	for _, key := range []string{"", "client", "wrong"} {
		if w := nativeCall(s, "GET", "/api/state", key, ""); w.Code != 401 {
			t.Fatalf("management accepted %q", key)
		}
	}
	w := nativeCall(s, "GET", "/api/state", "admin", "")
	var state State
	json.Unmarshal(w.Body.Bytes(), &state)
	if w.Code != 200 || state.Mode != "live" || !state.Connected || len(state.Accounts) != 0 || state.Engine.Storage != "local" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("bad native state %s", w.Body)
	}
	for _, secret := range []string{"access_token", "refresh_token", "client_secret"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("state leaked credential fields")
		}
	}
	for _, tc := range []struct {
		key  string
		want int
	}{{"", 401}, {"wrong", 401}, {"admin", 401}, {"client", 200}} {
		if w = nativeCall(s, "GET", "/v1/models", tc.key, ""); w.Code != tc.want {
			t.Fatalf("key boundary %q: %d", tc.key, w.Code)
		}
	}
	for _, path := range []string{"/v1/models?key=client", "/v1/models?key=admin"} {
		if w = nativeCall(s, "GET", path, "client", ""); w.Code != 401 {
			t.Fatal("accepted URL key")
		}
	}
	r := httptest.NewRequest("GET", "http://localhost/v1/models", nil)
	r.Header.Set("Authorization", "Bearer client")
	r.Header.Set("X-Api-Key", "admin")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("accepted conflicting credentials")
	}
	r = httptest.NewRequest("DELETE", "http://localhost/api/accounts", strings.NewReader(`{"id":"a"}`))
	r.Header.Set("Authorization", "Bearer admin")
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("accepted cross-origin mutation")
	}
}
func TestNativeLocalAccessAndDemoIsolation(t *testing.T) {
	h, err := New(Config{Demo: true}, fstest.MapFS{"index.html": {Data: []byte("vrouter")}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.(*server).Close()
	for _, tc := range []struct {
		host, peer string
		want       int
	}{{"localhost", "127.0.0.1:1234", 200}, {"evil.example", "127.0.0.1:1234", 403}, {"localhost", "192.0.2.1:1234", 403}} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/api/state", nil)
		r.RemoteAddr = tc.peer
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("local guard: %+v -> %d", tc, w.Code)
		}
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/", 200}, {"GET", "/missing", 404}, {"GET", "/v1/models", 503}, {"POST", "/api/oauth/codex", 503}, {"PUT", "/api/model-settings", 503}} {
		if w := nativeCall(h, tc.method, tc.path, "", ""); w.Code != tc.want {
			t.Fatalf("%s: %d", tc.path, w.Code)
		}
	}
	if _, err := New(Config{Demo: true, APIKey: "same", AdminToken: "same"}, fstest.MapFS{}); err == nil {
		t.Fatal("accepted shared admin/client secret")
	}
}
func TestNativeModelPolicyPersistsAndControlsRoutes(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	mock := oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		var input map[string]any
		json.NewDecoder(r.Body).Decode(&input)
		if input["model"] != "model" {
			t.Error("alias was not resolved")
		}
		return oauthReply(200, map[string]string{"ok": "yes"}), nil
	})
	nativeMock(s, mock)
	var settings modelSettings
	w := oauthCall(s, "GET", "/api/model-settings", "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &settings)
	body, _ := json.Marshal(map[string]any{"revision": settings.Revision, "models": []map[string]any{{"id": "model", "provider": "Codex", "enabled": true, "alias": "fast"}}})
	w = oauthCall(s, "PUT", "/api/model-settings", string(body))
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = oauthCall(s, "PUT", "/api/model-settings", string(body)); w.Code != 409 {
		t.Fatal("accepted stale revision")
	}
	if w = nativeCall(s, "POST", "/v1/responses", "client", `{"model":"model","stream":true}`); w.Code != 404 {
		t.Fatal("native ID escaped replacement alias")
	}
	if w = nativeCall(s, "POST", "/v1/responses", "client", `{"model":"fast","stream":true}`); w.Code != 200 {
		t.Fatalf("alias failed: %s", w.Body)
	}
	cfg := s.cfg
	s.Close()
	h, err := New(cfg, fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	s = h.(*server)
	defer s.Close()
	nativeMock(s, mock)
	w = nativeCall(s, "GET", "/v1/models", "client", "")
	if !strings.Contains(w.Body.String(), `"id":"fast"`) {
		t.Fatal("alias did not survive restart")
	}
	w = oauthCall(s, "PATCH", "/api/accounts", `{"id":"a","enabled":false}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = nativeCall(s, "POST", "/v1/responses", "client", `{"model":"fast","stream":true}`); w.Code != 404 {
		t.Fatal("disabled account remained routable")
	}
	oauthCall(s, "PATCH", "/api/accounts", `{"id":"a","enabled":true}`)
	w = oauthCall(s, "GET", "/api/model-settings", "")
	json.Unmarshal(w.Body.Bytes(), &settings)
	body, _ = json.Marshal(map[string]any{"revision": settings.Revision, "models": []map[string]any{{"id": "model", "provider": "Codex", "enabled": false, "alias": "fast"}}})
	if w = oauthCall(s, "PUT", "/api/model-settings", string(body)); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = nativeCall(s, "POST", "/v1/responses", "client", `{"model":"fast","stream":true}`); w.Code != 404 {
		t.Fatal("disabled model remained routable")
	}
	if w = oauthCall(s, "DELETE", "/api/accounts", `{"id":"a"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if len(s.store.snapshot().Accounts) != 0 {
		t.Fatal("account not removed")
	}
}

// Keep io used in the shared response helper below; streaming tests use it too.
func nativeSSE(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}
