package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func ptr[T any](v T) *T { return &v }
func testAssets() fs.FS { return fstest.MapFS{"index.html": {Data: []byte("vrouter")}} }
func testServer(t *testing.T, cfg Config) *server {
	t.Helper()
	cfg.DataDir = t.TempDir()
	h, err := New(cfg, testAssets())
	if err != nil {
		t.Fatal(err)
	}
	s := h.(*server)
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func call(t *testing.T, s *server, method, path, body, secret string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Host = "localhost"
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func newTestKey(t *testing.T, s *server, quotas string) (string, string) {
	t.Helper()
	w := call(t, s, "POST", "/api/keys", `{"name":"test","providerQuotas":`+quotas+`}`, "")
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Secret string  `json:"secret"`
		Key    keyView `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Key.ID, body.Secret
}

func TestPercentWindowsAndProviders(t *testing.T) {
	now := time.Now()
	five := now.Add(time.Hour)
	week := now.Add(7 * 24 * time.Hour)
	key := keyRecord{ProviderQuotas: map[string]providerPercentQuota{"claude": {FiveHour: ptr(25.0), SevenDay: ptr(25.0)}, "codex": {FiveHour: ptr(45.0)}}}
	settlePercent(&key, []percentCharge{{Provider: "claude", AccountID: "a", Window: "five-hour", ResetAt: five, Percent: 25}, {Provider: "claude", AccountID: "b", Window: "weekly", ResetAt: week, Percent: 10}}, nil)
	if percentAdmission(key, "claude", now) == "" {
		t.Fatal("5h cap not enforced")
	}
	if percentAdmission(key, "codex", now) != "" {
		t.Fatal("Claude usage blocked Codex")
	}
	if percentAdmission(key, "claude", five) != "" {
		t.Fatal("expired 5h cap still blocks")
	}
	if got := keyPercentUsage(key, five)["claude"].SevenDay; got != 10 {
		t.Fatalf("5h reset lost weekly usage: %v", got)
	}
	key.ProviderQuotas["codex"] = providerPercentQuota{FiveHour: ptr(0.0)}
	if percentAdmission(key, "codex", now) == "" {
		t.Fatal("explicit zero must block")
	}
	key.ProviderQuotas["codex"] = providerPercentQuota{}
	if percentAdmission(key, "codex", now) != "" {
		t.Fatal("omitted limits should be unlimited")
	}
}
func TestPercentDelta(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	sample := func(used float64, resetAt *time.Time) quotaCache {
		return quotaCache{ObservedAt: now, ReportedWindows: []QuotaWindow{{ID: "five-hour", used: ptr(used), ResetAt: resetAt}}}
	}
	delta, _, ok := percentDelta(sample(10, &reset), sample(18, &reset), "five-hour", now)
	if !ok || delta != 8 {
		t.Fatalf("delta %v %v", delta, ok)
	}
	if _, _, ok := percentDelta(sample(10, &reset), sample(3, &reset), "five-hour", now); ok {
		t.Fatal("decreasing utilization trusted")
	}
	changed := reset.Add(time.Hour)
	if _, _, ok := percentDelta(sample(10, &reset), sample(18, &changed), "five-hour", now); ok {
		t.Fatal("mid-request reset trusted")
	}
	if delta, _, ok := percentDelta(sample(0, nil), sample(2, &reset), "five-hour", now); !ok || delta != 2 {
		t.Fatal("idle window did not start at zero")
	}
	if _, _, ok := percentDelta(sample(0, nil), quotaCache{Error: "missing"}, "five-hour", now); ok {
		t.Fatal("missing usage trusted")
	}
}
func TestPercentQuotasPersistAndRecover(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	id, secret := newTestKey(t, s, `{"claude":{"fiveHour":25,"sevenDay":25},"codex":{"sevenDay":45}}`)
	w := call(t, s, "GET", "/api/keys", "", "")
	if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), `"hash"`) {
		t.Fatal("listed secret or hash")
	}
	if ok, _, _ := s.manager.reserve(inferenceKey{KeyID: id, GatewayID: defaultGatewayID, Provider: "claude"}); !ok {
		t.Fatal("reservation refused")
	}
	raw, _ := json.Marshal(s.manager.registry.snapshot())
	recovered, err := registryDecode(raw)
	if err != nil {
		t.Fatal(err)
	}
	key := findKey(&recovered, id)
	if key.InFlight != 0 || percentAdmission(*key, "claude", time.Now()) == "" {
		t.Fatal("unfinished reservation did not fail closed")
	}
	clone := registryClone(recovered)
	*clone.Keys[0].ProviderQuotas["claude"].FiveHour = 99
	clone.Keys[0].PercentUncertain["claude"] = false
	if *key.ProviderQuotas["claude"].FiveHour != 25 || !key.PercentUncertain["claude"] {
		t.Fatal("registry clone aliases percentage state")
	}
}
func TestPercentValidation(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	for _, quotas := range []string{`{"claude":{"fiveHour":101}}`, `{"codex":{"sevenDay":-1}}`, `{"other":{"fiveHour":10}}`} {
		w := call(t, s, "POST", "/api/keys", `{"name":"bad","providerQuotas":`+quotas+`}`, "")
		if w.Code != 400 {
			t.Fatalf("bad quota accepted: %s %d", quotas, w.Code)
		}
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func addClaude(t *testing.T, s *server, id string) {
	t.Helper()
	err := s.store.update(func(d *diskState) error {
		d.Accounts = append(d.Accounts, storedAccount{ID: id, Provider: "claude", AuthMode: "oauth", AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestInferencePercentageEnforcement(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "measured", true: "missing"}[missing], func(t *testing.T) {
			s := testServer(t, Config{ExternalAuth: true})
			addClaude(t, s, "a")
			_, secret := newTestKey(t, s, `{"claude":{"fiveHour":25,"sevenDay":25}}`)
			sent := 0
			reset := time.Now().Add(5 * time.Hour).Format(time.RFC3339)
			weekly := time.Now().Add(7 * 24 * time.Hour).Format(time.RFC3339)
			transport := roundTripper(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/v1/models":
					return jsonResponse(`{"data":[{"id":"claude-haiku-test"}]}`, 200), nil
				case "/api/oauth/profile":
					return jsonResponse(`{}`, 200), nil
				case "/api/oauth/usage":
					if missing && sent > 0 {
						return jsonResponse(`{}`, 503), nil
					}
					used := 0
					if sent > 0 {
						used = 30
					}
					body, _ := json.Marshal(map[string]any{"five_hour": map[string]any{"utilization": used, "resets_at": reset}, "seven_day": map[string]any{"utilization": used / 2, "resets_at": weekly}})
					return jsonResponse(string(body), 200), nil
				case "/v1/messages":
					sent++
					return jsonResponse(`{"type":"message","usage":{"input_tokens":2,"output_tokens":3}}`, 200), nil
				}
				t.Errorf("unexpected path %s", r.URL.Path)
				return jsonResponse(`{}`, 404), nil
			})
			s.client.Transport = transport
			s.streamClient.Transport = transport
			body := `{"model":"claude-haiku-test","messages":[{"role":"user","content":"hello"}],"max_tokens":8}`
			first := call(t, s, "POST", "/v1/messages", body, secret)
			if first.Code != 200 {
				t.Fatalf("first %d %s", first.Code, first.Body.String())
			}
			second := call(t, s, "POST", "/v1/messages", body, secret)
			if second.Code != 429 || sent != 1 {
				t.Fatalf("second %d, forwarded %d: %s", second.Code, sent, second.Body.String())
			}
			key := s.manager.registry.snapshot().Keys[0]
			usage := keyPercentUsage(key, time.Now())["claude"]
			if missing && !usage.Uncertain {
				t.Fatal("missing measurement did not block")
			}
			if !missing && (usage.FiveHour != 30 || usage.SevenDay != 15) {
				t.Fatalf("wrong usage %+v", usage)
			}
		})
	}
}
func TestUsageGate(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	newTestKey(t, s, `{"claude":{"fiveHour":25}}`)
	release, ok := s.lockPercentUsage("claude")
	if !ok {
		t.Fatal("first gate")
	}
	if other, ok := s.lockPercentUsage("claude"); ok {
		other()
		t.Fatal("concurrent metering allowed")
	}
	codex, ok := s.lockPercentUsage("codex")
	if !ok {
		t.Fatal("Claude blocked Codex")
	}
	codex()
	release()
}
func TestManagementAuthModes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		status int
	}{{"local", Config{}, 403}, {"external", Config{ExternalAuth: true}, 200}, {"token", Config{ExternalAuth: true, AdminToken: "admin"}, 401}} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, tc.cfg)
			r := httptest.NewRequest("GET", "https://router.example/api/keys", nil)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("got %d want %d", w.Code, tc.status)
			}
			r = httptest.NewRequest("POST", "https://router.example/api/keys", bytes.NewBufferString(`{"name":"x"}`))
			r.Header.Set("Origin", "https://other.example")
			w = httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("cross-origin write %d", w.Code)
			}
			if got := call(t, s, "GET", "/auth/login/company", "", "").Code; got != 404 {
				t.Fatalf("OIDC route still exists: %d", got)
			}
			if got := call(t, s, "GET", "/v1/models", "", "").Code; got == 200 {
				t.Fatal("proxy mode bypassed inference API key")
			}
		})
	}
}

func TestRelativeResetTimestamps(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	next := reset.Add(time.Second)
	before := quotaCache{ObservedAt: now, ReportedWindows: []QuotaWindow{{ID: "weekly", used: ptr(10.0), ResetAt: &reset}}}
	after := quotaCache{ObservedAt: now, ReportedWindows: []QuotaWindow{{ID: "weekly", used: ptr(12.0), ResetAt: &next}}}
	if delta, _, ok := percentDelta(before, after, "weekly", now); !ok || delta != 2 {
		t.Fatal("relative countdown jitter rejected")
	}
	key := keyRecord{}
	settlePercent(&key, []percentCharge{{Provider: "claude", AccountID: "a", Window: "weekly", ResetAt: reset, Percent: 1}, {Provider: "claude", AccountID: "a", Window: "weekly", ResetAt: next, Percent: 2}}, nil)
	if len(key.PercentCharges) != 1 || key.PercentCharges[0].Percent != 3 {
		t.Fatal("same relative window not merged")
	}
}

func TestKeyQuotaEdits(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	id, _ := newTestKey(t, s, `{"claude":{"fiveHour":25}}`)
	invalid := call(t, s, "PATCH", "/api/keys/"+id, `{"providerQuotas":{"claude":{"weekly":10}}}`, "")
	if invalid.Code != 400 {
		t.Fatal("misspelled quota silently accepted")
	}
	s.manager.reserve(inferenceKey{KeyID: id, GatewayID: defaultGatewayID, Provider: "claude"})
	busy := call(t, s, "PATCH", "/api/keys/"+id, `{"providerQuotas":{"claude":{"fiveHour":30}}}`, "")
	if busy.Code != 409 {
		t.Fatalf("editing active quota: %d", busy.Code)
	}
	if err := s.manager.update(func(d *diskRegistry) error {
		key := findKey(d, id)
		key.InFlight = 0
		key.PercentUncertain = map[string]bool{"claude": true}
		key.PercentCharges = []percentCharge{{Provider: "claude", AccountID: "a", Window: "five-hour", ResetAt: time.Now().Add(time.Hour), Percent: 10}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	renamed := call(t, s, "PATCH", "/api/keys/"+id, `{"name":"renamed"}`, "")
	if renamed.Code != 200 {
		t.Fatal("rename failed")
	}
	key := s.manager.registry.snapshot().Keys[0]
	if !key.PercentUncertain["claude"] {
		t.Fatal("rename acknowledged missing usage")
	}
	ack := call(t, s, "PATCH", "/api/keys/"+id, `{"providerQuotas":{"claude":{"fiveHour":30}}}`, "")
	if ack.Code != 200 {
		t.Fatal("quota acknowledgement failed")
	}
	key = s.manager.registry.snapshot().Keys[0]
	if key.PercentUncertain["claude"] || keyPercentUsage(key, time.Now())["claude"].FiveHour != 10 {
		t.Fatal("acknowledgement lost known usage")
	}
	revoked := call(t, s, "PATCH", "/api/keys/"+id, `{"revoked":true}`, "")
	if revoked.Code != 200 {
		t.Fatal("revoke failed")
	}
	if ok, _, _ := s.manager.reserve(inferenceKey{KeyID: id, Provider: "claude"}); ok {
		t.Fatal("revoked key admitted")
	}
}

func TestKeyExpiry(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	create := func(body string) *httptest.ResponseRecorder {
		return call(t, s, "POST", "/api/keys", body, "")
	}
	if w := create(`{"name":"past","expiresAt":"2000-01-01T00:00:00Z"}`); w.Code != 400 {
		t.Fatalf("past expiry accepted: %d", w.Code)
	}
	if w := create(`{"name":"bad","expiresAt":"tomorrow"}`); w.Code != 400 {
		t.Fatalf("malformed expiry accepted: %d", w.Code)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	w := create(`{"name":"k","expiresAt":"` + future + `"}`)
	var out struct {
		Key    keyView
		Secret string
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Key.ExpiresAt == nil {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	principal := inferenceKey{KeyID: out.Key.ID, GatewayID: defaultGatewayID, Provider: "claude"}
	if ok, _, _ := s.manager.reserve(principal); !ok {
		t.Fatal("unexpired key refused")
	}
	past := time.Now().Add(-time.Minute)
	if err := s.manager.update(func(d *diskRegistry) error { findKey(d, out.Key.ID).ExpiresAt = &past; return nil }); err != nil {
		t.Fatal(err)
	}
	if ok, status, _ := s.manager.reserve(principal); ok || status != 401 {
		t.Fatalf("expired key admitted: %v %d", ok, status)
	}
	if auth := call(t, s, "GET", "/v1/models", "", out.Secret); auth.Code != 401 || !strings.Contains(auth.Body.String(), "expired") {
		t.Fatalf("expired key authorized: %d %s", auth.Code, auth.Body)
	}
	patch := func(body string) *httptest.ResponseRecorder {
		return call(t, s, "PATCH", "/api/keys/"+out.Key.ID, body, "")
	}
	// Renaming an expired key resends its stored expiry unchanged.
	if w := patch(`{"name":"renamed","expiresAt":"` + past.UTC().Format(time.RFC3339Nano) + `"}`); w.Code != 200 {
		t.Fatalf("rename of expired key: %d %s", w.Code, w.Body)
	}
	if w := patch(`{"expiresAt":"2000-01-01T00:00:00Z"}`); w.Code != 400 {
		t.Fatalf("new past expiry accepted: %d", w.Code)
	}
	if w := patch(`{"expiresAt":null}`); w.Code != 200 {
		t.Fatalf("clear expiry: %d %s", w.Code, w.Body)
	}
	if ok, _, _ := s.manager.reserve(principal); !ok {
		t.Fatal("key still refused after clearing its expiry")
	}
}

func TestRegistryLoadsLegacyLifetimeLimits(t *testing.T) {
	hash := strings.Repeat("a", 64)
	state, err := registryDecode([]byte(`{"version":1,"keys":[{"id":"k","gatewayId":"` + defaultGatewayID + `","name":"old","prefix":"vr_abc","hash":"` + hash + `","createdAt":"2026-01-01T00:00:00Z","limitRequests":5,"limitTokens":10,"usedRequests":5,"usedTokens":3,"usageUncertain":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "limit") || strings.Contains(string(data), "usageUncertain") {
		t.Fatalf("legacy fields survived: %s", data)
	}
}
