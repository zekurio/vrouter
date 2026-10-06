package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

func createGateway(t *testing.T, s *server, name string) gatewayView {
	t.Helper()
	w := oauthCall(s, "POST", "/api/gateways", fmt.Sprintf(`{"name":%q}`, name))
	if w.Code != 201 {
		t.Fatalf("create gateway: %d %s", w.Code, w.Body)
	}
	var response gatewayResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Gateway.ID == "" || response.Gateway.OwnerID != localAdminID {
		t.Fatalf("bad gateway response %+v", response.Gateway)
	}
	return response.Gateway
}

func gatewayCall(s *server, gatewayID, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer admin")
	if gatewayID != "" {
		req.Header.Set(gatewayHeader, gatewayID)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func createKeyOn(t *testing.T, s *server, gatewayID, name string, limitRequests, limitTokens *int64) (keyView, string) {
	t.Helper()
	body := map[string]any{"name": name}
	if limitRequests != nil {
		body["limitRequests"] = *limitRequests
	}
	if limitTokens != nil {
		body["limitTokens"] = *limitTokens
	}
	raw, _ := json.Marshal(body)
	w := gatewayCall(s, gatewayID, "POST", "/api/keys", string(raw))
	if w.Code != 201 {
		t.Fatalf("create key: %d %s", w.Code, w.Body)
	}
	var response struct {
		Key    keyView `json:"key"`
		Secret string  `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response.Secret, "vr_") || response.Key.Prefix == "" {
		t.Fatalf("bad key response %+v", response)
	}
	return response.Key, response.Secret
}

func int64Ptr(value int64) *int64 { return &value }

func TestGatewayIsolationLifecycleAndPersistence(t *testing.T) {
	s := oauthServer(t)
	w := oauthCall(s, "GET", "/api/gateways", "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var listed gatewaysResponse
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Gateways) != 1 || listed.Gateways[0].ID != defaultGatewayID || listed.User.ID != localAdminID || listed.User.Role != "admin" {
		t.Fatalf("default gateway listing %+v", listed)
	}

	created := createGateway(t, s, "work")
	first := createGateway(t, s, "second")
	second := createGateway(t, s, "third")
	third := createGateway(t, s, "fourth")
	json.Unmarshal(oauthCall(s, "GET", "/api/gateways", "").Body.Bytes(), &listed)
	if len(listed.Gateways) != 5 || listed.Gateways[0].ID != defaultGatewayID {
		t.Fatalf("gateway order %+v", listed.Gateways)
	}

	// Child and legacy accounts must never bleed into each other.
	child, err := s.manager.engine(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	nativeSeed(t, s, nativeAccount("root-account"))
	nativeSeed(t, child, nativeAccount("child-account"))
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("root-model")), nil
		}
		return nativeSSE("data: done\n\n"), nil
	}))
	nativeMock(child, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		return nativeSSE("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":7}}}\n\n"), nil
	}))

	state := gatewayCall(s, "", "GET", "/api/state", "")
	if !strings.Contains(state.Body.String(), "root-account") || strings.Contains(state.Body.String(), "child-account") {
		t.Fatalf("legacy state leaked child data: %s", state.Body)
	}
	childState := gatewayCall(s, created.ID, "GET", "/api/state", "")
	if !strings.Contains(childState.Body.String(), "child-account") || strings.Contains(childState.Body.String(), "root-account") {
		t.Fatalf("child state leaked root data: %s", childState.Body)
	}
	if code := gatewayCall(s, created.ID, "GET", "/api/model-settings", "").Code; code != 200 {
		t.Fatalf("child model settings: %d", code)
	}

	key, secret := createKeyOn(t, s, created.ID, "worker", nil, nil)
	// The legacy key reaches the legacy account set only.
	if w = nativeCall(s, "POST", "/v1/responses", "client", `{"model":"model","stream":true}`); w.Code != 404 {
		t.Fatalf("legacy key reached child accounts: %d %s", w.Code, w.Body)
	}
	// A child key reaches the child account set only, whatever the model.
	if w = nativeCall(s, "POST", "/v1/responses", secret, `{"model":"root-model","stream":true}`); w.Code != 404 {
		t.Fatalf("child key reached legacy accounts: %d %s", w.Code, w.Body)
	}
	if w = nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("child key inference: %d %s", w.Code, w.Body)
	}

	// Keys of a different gateway must not authorize inference anywhere.
	otherKey, otherSecret := createKeyOn(t, s, first.ID, "other", nil, nil)
	if w = nativeCall(s, "POST", "/v1/responses", otherSecret, `{"model":"model","stream":true}`); w.Code != 404 {
		t.Fatalf("key of an account-less gateway inferred: %d %s", w.Code, w.Body)
	}
	unknown := gatewayCall(s, defaultGatewayID, "GET", "/api/keys", "")
	if unknown.Code != 200 || !strings.Contains(unknown.Body.String(), `"keys":[]`) {
		t.Fatalf("default gateway has foreign keys: %d %s", unknown.Code, unknown.Body)
	}
	if foreign := gatewayCall(s, first.ID, "GET", "/api/keys", ""); !strings.Contains(foreign.Body.String(), otherKey.ID) {
		t.Fatalf("first gateway keys missing: %s", foreign.Body)
	}
	keys := gatewayCall(s, created.ID, "GET", "/api/keys", "")
	if !strings.Contains(keys.Body.String(), key.ID) || strings.Contains(keys.Body.String(), otherKey.ID) {
		t.Fatalf("child keys mixed across gateways: %s", keys.Body)
	}
	if strings.Contains(keys.Body.String(), "hash") || strings.Contains(keys.Body.String(), secret) {
		t.Fatalf("key listing leaked secret material: %s", keys.Body)
	}
	if second.ID == third.ID {
		t.Fatal("gateway IDs repeated")
	}

	// The durable registry must not contain the plaintext secret.
	registryBytes, err := os.ReadFile(filepath.Join(s.cfg.DataDir, registryFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(registryBytes), secret) {
		t.Fatal("registry persisted the plaintext key")
	}
	sum := sha256.Sum256([]byte(secret))
	if !strings.Contains(string(registryBytes), hex.EncodeToString(sum[:])) {
		t.Fatal("registry did not persist the key hash")
	}

	cfg := s.cfg
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := New(cfg, fstest.MapFS{"index.html": {Data: []byte("vrouter")}})
	if err != nil {
		t.Fatal(err)
	}
	restarted := h.(*server)
	defer restarted.Close()
	restartedChild, err := restarted.manager.engine(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	nativeMock(restartedChild, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		return nativeSSE("data: done\n\n"), nil
	}))

	if w = nativeCall(restarted, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("child key did not survive restart: %d %s", w.Code, w.Body)
	}
	childState = gatewayCall(restarted, created.ID, "GET", "/api/state", "")
	if !strings.Contains(childState.Body.String(), "child-account") {
		t.Fatalf("child account did not survive restart: %s", childState.Body)
	}
}

func TestRegistryCrossProcessLockAndInitFailureLeavesNoLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	first, err := openRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openRegistry(dir); err == nil {
		t.Fatal("second process opened the same registry")
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}

	// A malformed registry must fail startup and release its lock, so fixing
	// the file lets the next process start.
	if err := os.WriteFile(filepath.Join(dir, registryFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openRegistry(dir); err == nil {
		t.Fatal("accepted a malformed registry")
	}
	if err := os.WriteFile(filepath.Join(dir, registryFileName), []byte(`{"version":1,"gateways":[],"keys":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := openRegistry(dir)
	if err != nil {
		t.Fatalf("lock was orphaned after a failed init: %v", err)
	}
	defer recovered.close()
	if len(recovered.snapshot().Gateways) != 1 || recovered.snapshot().Gateways[0].ID != defaultGatewayID {
		t.Fatal("default gateway was not seeded")
	}
}

func TestRegistryRollbackOnWriteFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	registry, err := openRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.close()
	create := func(mutate func(*diskRegistry) error) error { return registry.update(mutate) }
	if err := create(func(state *diskRegistry) error {
		state.Keys = append(state.Keys, keyRecord{ID: "k1", GatewayID: defaultGatewayID, Name: "one", Prefix: "vr_abcdefgh", Hash: strings.Repeat("a", 64), CreatedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := registry.snapshot()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	err = registry.update(func(state *diskRegistry) error {
		state.Keys[0].UsedRequests = 99
		return nil
	})
	if err == nil {
		t.Fatal("update succeeded without a writable directory")
	}
	after := registry.snapshot()
	if after.Keys[0].UsedRequests != 0 {
		t.Fatalf("failed write changed memory: %+v", after.Keys)
	}
	if len(after.Keys) != len(before.Keys) || len(after.Telemetry) != len(before.Telemetry) {
		t.Fatal("failed write changed the registry shape")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := registry.update(func(state *diskRegistry) error {
		state.Keys[0].UsedRequests = 99
		return nil
	}); err != nil {
		t.Fatal("registry unusable after a failed write")
	}
	if registry.snapshot().Keys[0].UsedRequests != 99 {
		t.Fatal("recovered write did not apply")
	}
}

// testAuth is a deterministic authenticator for facade tests.
type testAuth struct {
	users map[string]loginUser
}

func (a *testAuth) mode() string { return "oidc" }

func (a *testAuth) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"mode": "oidc", "providers": []any{}, "user": nil})
	})
}

func (a *testAuth) authenticate(r *http.Request) (loginUser, bool) {
	user, ok := a.users[r.Header.Get("X-Test-User")]
	return user, ok
}

func routerCall(t *testing.T, h http.Handler, user, gateway, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	if gateway != "" {
		req.Header.Set(gatewayHeader, gateway)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRouterOwnershipHeaderRulesAndIsolation(t *testing.T) {
	auth := &testAuth{users: map[string]loginUser{
		"admin": {ID: "admin-1", Name: "Admin", Role: "admin"},
		"u1":    {ID: "user-1", Name: "User One", Role: "user"},
		"u2":    {ID: "user-2", Name: "User Two", Role: "user"},
	}}
	cfg := Config{DataDir: t.TempDir(), APIKey: "client", AdminToken: "admin"}
	h, err := newRouter(cfg, fstest.MapFS{"index.html": {Data: []byte("vrouter")}}, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if w := routerCall(t, h, "", "", "GET", "/api/gateways", ""); w.Code != 401 {
		t.Fatalf("anonymous gateway list: %d", w.Code)
	}
	if w := routerCall(t, h, "u1", "", "GET", "/api/gateways", ""); w.Code != 200 {
		t.Fatalf("user gateway list: %d %s", w.Code, w.Body)
	} else if !strings.Contains(w.Body.String(), `"user":{"id":"user-1"`) {
		t.Fatalf("gateway list user mismatch: %s", w.Body)
	}

	// u1 creates a gateway; the legacy default belongs to local-admin.
	w := routerCall(t, h, "u1", "", "POST", "/api/gateways", `{"name":"u1 gateway"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created gatewayResponse
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Gateway.OwnerID != "user-1" {
		t.Fatalf("owner %q", created.Gateway.OwnerID)
	}

	if w = routerCall(t, h, "u1", "", "GET", "/api/state", ""); w.Code != 400 {
		t.Fatalf("missing header must be refused in OIDC mode: %d %s", w.Code, w.Body)
	}
	if w = routerCall(t, h, "u2", created.Gateway.ID, "GET", "/api/state", ""); w.Code != 404 {
		t.Fatalf("foreign gateway leaked: %d %s", w.Code, w.Body)
	}
	if w = routerCall(t, h, "u1", created.Gateway.ID, "GET", "/api/state", ""); w.Code != 200 {
		t.Fatalf("own gateway: %d %s", w.Code, w.Body)
	}
	if w = routerCall(t, h, "u2", created.Gateway.ID, "GET", "/api/keys", ""); w.Code != 404 {
		t.Fatalf("foreign keys leaked: %d %s", w.Code, w.Body)
	}
	if w = routerCall(t, h, "u1", defaultGatewayID, "GET", "/api/state", ""); w.Code != 404 {
		t.Fatalf("legacy default leaked to user: %d %s", w.Code, w.Body)
	}
	if w = routerCall(t, h, "admin", defaultGatewayID, "GET", "/api/state", ""); w.Code != 200 {
		t.Fatalf("admin cannot reach legacy default: %d %s", w.Code, w.Body)
	}
	if w = routerCall(t, h, "admin", "", "GET", "/api/gateways", ""); w.Code != 200 || !strings.Contains(w.Body.String(), defaultGatewayID) {
		t.Fatalf("admin gateway list: %d %s", w.Code, w.Body)
	}
	// Listing is independent of a stale selection.
	if w = routerCall(t, h, "u1", "missing-gateway", "GET", "/api/gateways", ""); w.Code != 200 {
		t.Fatalf("stale selection hid gateway list: %d %s", w.Code, w.Body)
	}
	// Cross-origin mutations are refused for session-style management.
	req := httptest.NewRequest("POST", "http://localhost/api/gateways", strings.NewReader(`{"name":"evil"}`))
	req.Header.Set("X-Test-User", "u1")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("cross-origin gateway create: %d", rec.Code)
	}
	// An API key must never authorize management.
	if w = routerCall(t, h, "", created.Gateway.ID, "GET", "/api/state", ""); w.Code != 401 {
		t.Fatalf("anonymous state: %d", w.Code)
	}

	// Inference selects its gateway from the key, not the header.
	root := h.root
	engine := h.manager
	child, err := engine.engine(created.Gateway.ID)
	if err != nil {
		t.Fatal(err)
	}
	nativeSeed(t, child, nativeAccount("child"))
	nativeMock(child, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		return nativeSSE("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n\n"), nil
	}))
	nativeSeed(t, root, nativeAccount("root"))
	nativeMock(root, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		return nativeSSE("data: done\n\n"), nil
	}))

	_, secret := createKeyForRouter(t, h, "u1", created.Gateway.ID, "worker")
	req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"model","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set(gatewayHeader, defaultGatewayID) // must be ignored
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("child key inference: %d %s", rec.Code, rec.Body)
	}
	if w = routerCall(t, h, "", "", "GET", "/v1/models", ""); w.Code != 401 {
		t.Fatalf("models without key: %d", w.Code)
	}

	// VROUTER_API_KEY still selects the legacy default only.
	req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"model","stream":true}`))
	req.Header.Set("Authorization", "Bearer client")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("legacy key inference: %d %s", rec.Code, rec.Body)
	}
	if w = routerCall(t, h, "", "", "POST", "/v1/responses", `{"model":"model","stream":true}`); w.Code != 401 {
		t.Fatalf("inference without key: %d", w.Code)
	}
}

func createKeyForRouter(t *testing.T, h http.Handler, user, gatewayID, name string) (keyView, string) {
	t.Helper()
	w := routerCall(t, h, user, gatewayID, "POST", "/api/keys", fmt.Sprintf(`{"name":%q}`, name))
	if w.Code != 201 {
		t.Fatalf("create key: %d %s", w.Code, w.Body)
	}
	var response struct {
		Key    keyView `json:"key"`
		Secret string  `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Key, response.Secret
}

func TestGatewayKeyAccessIsolation(t *testing.T) {
	s := oauthServer(t)
	g1, g2 := createGateway(t, s, "one"), createGateway(t, s, "two")
	_, secret1 := createKeyOn(t, s, g1.ID, "one", nil, nil)
	_, secret2 := createKeyOn(t, s, g2.ID, "two", nil, nil)

	e1, err := s.manager.engine(g1.ID)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.manager.engine(g2.ID)
	if err != nil {
		t.Fatal(err)
	}
	nativeSeed(t, e1, nativeAccount("one"))
	nativeSeed(t, e2, nativeAccount("two"))
	var calls1, calls2 atomic.Int32
	mock := func(counter *atomic.Int32) oauthTransport {
		return oauthTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method == "GET" {
				return oauthReply(200, nativeModels("model")), nil
			}
			counter.Add(1)
			return nativeSSE("data: done\n\n"), nil
		})
	}
	nativeMock(e1, mock(&calls1))
	nativeMock(e2, mock(&calls2))
	if w := nativeCall(s, "POST", "/v1/responses", secret1, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("key one: %d %s", w.Code, w.Body)
	}
	if w := nativeCall(s, "POST", "/v1/responses", secret2, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("key two: %d %s", w.Code, w.Body)
	}
	if calls1.Load() != 1 || calls2.Load() != 1 {
		t.Fatalf("keys crossed gateways: %d %d", calls1.Load(), calls2.Load())
	}
	// A made-up key never reaches any engine.
	if w := nativeCall(s, "POST", "/v1/responses", "vr_not-a-real-key", `{"model":"model","stream":true}`); w.Code != 401 {
		t.Fatalf("forged key: %d", w.Code)
	}
}
