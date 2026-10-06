package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

func keyList(t *testing.T, s *server, gatewayID string) []keyView {
	t.Helper()
	w := gatewayCall(s, gatewayID, "GET", "/api/keys", "")
	if w.Code != 200 {
		t.Fatalf("list keys: %d %s", w.Code, w.Body)
	}
	var response struct {
		Keys []keyView `json:"keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Keys
}

func findKeyView(keys []keyView, id string) (keyView, bool) {
	for _, key := range keys {
		if key.ID == id {
			return key, true
		}
	}
	return keyView{}, false
}

func TestKeyLifecycleValidationAndDemo(t *testing.T) {
	s := oauthServer(t)
	key, secret := createKeyOn(t, s, defaultGatewayID, "first", int64Ptr(5), int64Ptr(1000))
	if key.LimitRequests != 5 || key.LimitTokens != 1000 || key.UsedRequests != 0 || key.UsageUncertain {
		t.Fatalf("created key %+v", key)
	}
	// 0 and missing mean unlimited.
	unlimited, _ := createKeyOn(t, s, defaultGatewayID, "unlimited", int64Ptr(0), nil)
	if unlimited.LimitRequests != 0 || unlimited.LimitTokens != 0 {
		t.Fatalf("unlimited key %+v", unlimited)
	}
	if w := gatewayCall(s, defaultGatewayID, "GET", "/api/keys", ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if strings.Contains(oauthCall(s, "GET", "/api/keys", "").Body.String(), secret) {
		t.Fatal("secret appears in a list response")
	}
	for _, body := range []string{
		`{"name":""}`,
		`{"name":"x","limitRequests":-1}`,
		`{"name":"x","limitTokens":-3}`,
		`{"name":"x","limitRequests":1.5}`,
	} {
		if w := gatewayCall(s, defaultGatewayID, "POST", "/api/keys", body); w.Code != 400 {
			t.Fatalf("accepted invalid key %s: %d %s", body, w.Code, w.Body)
		}
	}

	// Renaming and re-limit keep the key usable, revocation is irreversible.
	w := gatewayCall(s, defaultGatewayID, "PATCH", "/api/keys/"+key.ID, `{"name":"renamed","limitRequests":9}`)
	if w.Code != 200 {
		t.Fatalf("patch: %d %s", w.Code, w.Body)
	}
	if keys := keyList(t, s, defaultGatewayID); func() bool {
		k, ok := findKeyView(keys, key.ID)
		return !ok || k.Name != "renamed" || k.LimitRequests != 9
	}() {
		t.Fatalf("patch did not apply: %+v", keys)
	}
	if w = gatewayCall(s, defaultGatewayID, "PATCH", "/api/keys/"+key.ID, `{"revoked":true}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"revokedAt"`) {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w = nativeCall(s, "GET", "/v1/models", secret, ""); w.Code != 401 {
		t.Fatalf("revoked key authorized: %d", w.Code)
	}
	if w = gatewayCall(s, defaultGatewayID, "PATCH", "/api/keys/"+key.ID, `{"revoked":false}`); w.Code != 400 {
		t.Fatalf("revocation was undone: %d %s", w.Code, w.Body)
	}
	if w = gatewayCall(s, defaultGatewayID, "PATCH", "/api/keys/"+key.ID, `{}`); w.Code != 400 {
		t.Fatalf("empty patch: %d", w.Code)
	}
	if w = gatewayCall(s, defaultGatewayID, "PATCH", "/api/keys/missing", `{"name":"x"}`); w.Code != 404 {
		t.Fatalf("missing key patch: %d", w.Code)
	}
	if w = gatewayCall(s, defaultGatewayID, "DELETE", "/api/keys/"+unlimited.ID, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w = gatewayCall(s, defaultGatewayID, "DELETE", "/api/keys/"+unlimited.ID, ""); w.Code != 404 {
		t.Fatal("deleted key still present")
	}
	if keys := keyList(t, s, defaultGatewayID); func() bool { _, ok := findKeyView(keys, unlimited.ID); return ok }() {
		t.Fatal("deleted key remained listed")
	}

	// Demo mode has no registry: key management is refused with 409.
	h, err := New(Config{Demo: true}, fstest.MapFS{"index.html": {Data: []byte("vrouter")}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.(*server).Close()
	for _, tc := range []struct{ method, path, body string }{{"GET", "/api/keys", ""}, {"POST", "/api/keys", `{"name":"x"}`}, {"PATCH", "/api/keys/some-id", `{"name":"y"}`}, {"DELETE", "/api/keys/some-id", ""}} {
		if w := nativeCall(h, tc.method, tc.path, "", tc.body); w.Code != 409 {
			t.Fatalf("demo %s keys: %d %s", tc.method, w.Code, w.Body)
		}
	}
	if w := nativeCall(h, "GET", "/api/telemetry", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"requests":[]`) {
		t.Fatalf("demo telemetry: %d %s", w.Code, w.Body)
	}
	if w := nativeCall(h, "GET", "/api/gateways", "admin", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"default"`) {
		t.Fatalf("demo gateways: %d %s", w.Code, w.Body)
	}
	if w := nativeCall(h, "POST", "/api/gateways", "", `{"name":"x"}`); w.Code != 409 {
		t.Fatalf("demo gateway create: %d", w.Code)
	}
}

func TestRequestLimitConsumesBeforeForwarding(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	var providerCalls atomic.Int32
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		providerCalls.Add(1)
		return nativeSSE("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"), nil
	}))
	key, secret := createKeyOn(t, s, defaultGatewayID, "limited", int64Ptr(2), nil)

	// GET models never consumes.
	for range 3 {
		if w := nativeCall(s, "GET", "/v1/models", secret, ""); w.Code != 200 {
			t.Fatalf("models: %d", w.Code)
		}
	}
	if got := keyList(t, s, defaultGatewayID)[0].UsedRequests; got != 0 {
		t.Fatalf("models consumed %d requests", got)
	}
	// Malformed bodies are supported inference attempts and consume.
	if w := nativeCall(s, "POST", "/v1/responses", secret, "{"); w.Code != 400 {
		t.Fatalf("malformed: %d", w.Code)
	}
	if providerCalls.Load() != 0 {
		t.Fatal("malformed request reached the provider")
	}
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("first inference: %d %s", w.Code, w.Body)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("provider calls %d", providerCalls.Load())
	}
	// The limit is now reached; further calls are denied before forwarding.
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 429 {
		t.Fatalf("over limit: %d %s", w.Code, w.Body)
	}
	if providerCalls.Load() != 1 {
		t.Fatal("denied request reached the provider")
	}
	keys := keyList(t, s, defaultGatewayID)
	if k, ok := findKeyView(keys, key.ID); !ok || k.UsedRequests != 2 {
		t.Fatalf("usedRequests %+v", keys)
	}

	// Consumed requests survive a restart.
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
	if w := nativeCall(restarted, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 429 {
		t.Fatalf("request limit forgotten after restart: %d %s", w.Code, w.Body)
	}
}

func TestTokenLimitConcurrentInFlightAndUnknownUsage(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var providerCalls atomic.Int32
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		providerCalls.Add(1)
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}\n\n")
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
	}))
	key, secret := createKeyOn(t, s, defaultGatewayID, "tokens", nil, int64Ptr(1000))

	done := make(chan int, 1)
	go func() {
		done <- nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`).Code
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not start")
	}
	// A second request while the first is in flight must be refused, because
	// its measured usage is not known yet.
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 429 {
		t.Fatalf("concurrent token-limited request: %d %s", w.Code, w.Body)
	}
	if providerCalls.Load() != 1 {
		t.Fatal("concurrent request reached the provider")
	}
	close(release)
	if code := <-done; code != 200 {
		t.Fatalf("in-flight request: %d", code)
	}
	keys := keyList(t, s, defaultGatewayID)
	if k, ok := findKeyView(keys, key.ID); !ok || k.UsedTokens != 5 || k.UsageUncertain {
		t.Fatalf("settled key %+v", keys)
	}
	// The settled request frees the budget for the next call.
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("post-settle request: %d %s", w.Code, w.Body)
	}
}

func TestTokenLimitUnknownUsageBlocksUntilBudgetChange(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	var withUsage atomic.Bool
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		if withUsage.Load() {
			return nativeSSE("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"), nil
		}
		return nativeSSE("data: done\n\n"), nil
	}))
	key, secret := createKeyOn(t, s, defaultGatewayID, "measured", nil, int64Ptr(100))
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	keys := keyList(t, s, defaultGatewayID)
	if k, ok := findKeyView(keys, key.ID); !ok || !k.UsageUncertain {
		t.Fatalf("unknown usage did not block the key: %+v", keys)
	}
	// Blocked future calls explain the state instead of allowing unbounded
	// unknown spend.
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 429 || !strings.Contains(w.Body.String(), "token usage could not be measured or recorded") {
		t.Fatalf("uncertain key still forwarded: %d %s", w.Code, w.Body)
	}
	// An owner budget change clears the block.
	if w := gatewayCall(s, defaultGatewayID, "PATCH", "/api/keys/"+key.ID, `{"limitTokens":1000}`); w.Code != 200 || strings.Contains(w.Body.String(), `"usageUncertain":true`) {
		t.Fatalf("budget change: %d %s", w.Code, w.Body)
	}
	withUsage.Store(true)
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("unblocked key: %d %s", w.Code, w.Body)
	}
	keys = keyList(t, s, defaultGatewayID)
	if k, ok := findKeyView(keys, key.ID); !ok || k.UsageUncertain || k.UsedTokens != 2 {
		t.Fatalf("measured key after recovery: %+v", keys)
	}
}

func TestTokenLimitAllowsOverrunThenBlocks(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		return nativeSSE("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":30,\"output_tokens\":20}}}\n\n"), nil
	}))
	key, secret := createKeyOn(t, s, defaultGatewayID, "small", nil, int64Ptr(10))
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 429 || !strings.Contains(w.Body.String(), "token limit is reached") {
		t.Fatalf("overrun not blocked: %d %s", w.Code, w.Body)
	}
	keys := keyList(t, s, defaultGatewayID)
	if k, ok := findKeyView(keys, key.ID); !ok || k.UsedTokens != 50 {
		t.Fatalf("one request may exceed the remaining budget: %+v", keys)
	}
}

func TestRevocationDoesNotKillRunningResponse(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
	}))
	key, secret := createKeyOn(t, s, defaultGatewayID, "running", nil, nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not start")
	}
	if w := gatewayCall(s, defaultGatewayID, "PATCH", "/api/keys/"+key.ID, `{"revoked":true}`); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	close(release)
	if w := <-done; w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("running response was killed: %d %s", w.Code, w.Body)
	}
	// The settled row is still recorded and new calls are refused.
	telemetry := gatewayCall(s, defaultGatewayID, "GET", "/api/telemetry", "")
	if !strings.Contains(telemetry.Body.String(), key.ID) {
		t.Fatalf("telemetry lost: %s", telemetry.Body)
	}
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 401 {
		t.Fatalf("revoked key reused: %d", w.Code)
	}
}

func TestConcurrentAdmissionEnforcesRequestLimit(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		return nativeSSE("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"), nil
	}))
	key, secret := createKeyOn(t, s, defaultGatewayID, "burst", int64Ptr(20), nil)
	const callers = 64
	var wg sync.WaitGroup
	results := make(chan int, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`).Code
		}()
	}
	wg.Wait()
	close(results)
	allowed, denied := 0, 0
	for code := range results {
		switch code {
		case 200:
			allowed++
		case 429:
			denied++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if allowed != 20 || denied != 44 {
		t.Fatalf("allowed %d denied %d", allowed, denied)
	}
	keys := keyList(t, s, defaultGatewayID)
	if k, ok := findKeyView(keys, key.ID); !ok || k.UsedRequests != 20 {
		t.Fatalf("usedRequests after burst: %+v", keys)
	}
}

func TestKeyManagementWriteFailureFailsClosed(t *testing.T) {
	s := oauthServer(t)
	key, _ := createKeyOn(t, s, defaultGatewayID, "durable", nil, nil)
	if err := os.RemoveAll(s.cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	if w := gatewayCall(s, defaultGatewayID, "PATCH", "/api/keys/"+key.ID, `{"name":"nope"}`); w.Code != 500 {
		t.Fatalf("patch on unwritable registry: %d %s", w.Code, w.Body)
	}
	if w := gatewayCall(s, defaultGatewayID, "POST", "/api/keys", `{"name":"fresh"}`); w.Code != 500 {
		t.Fatalf("create on unwritable registry: %d %s", w.Code, w.Body)
	}
	if keys := keyList(t, s, defaultGatewayID); func() bool { k, ok := findKeyView(keys, key.ID); return !ok || k.Name != "durable" }() {
		t.Fatalf("failed writes changed key state: %+v", keys)
	}
}
