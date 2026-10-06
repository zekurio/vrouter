package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageParserSplitSSECumulativeAndTerminal(t *testing.T) {
	var claude usageParser
	claude.begin(true)
	for _, chunk := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"cache_read_input_tokens\":4,\"cache_creation_input_tokens\":2}}}\n\n",
		"event: message_delta\nda",
		"ta: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":15}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	} {
		claude.observe([]byte(chunk))
	}
	claude.complete()
	totals := claude.totals()
	if !totals.Known || totals.Input != 16 || totals.Output != 15 || totals.Cached != 4 || totals.Total != 31 {
		t.Fatalf("claude totals %+v", totals)
	}
	if claude.terminalFailure {
		t.Fatal("completed Claude stream marked as failed")
	}

	var codex usageParser
	codex.begin(true)
	codex.observe([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":5,\"total_tokens\":25,\"input_tokens_details\":{\"cached_tokens\":8}}}}\n\n"))
	codex.complete()
	totals = codex.totals()
	if !totals.Known || totals.Input != 20 || totals.Output != 5 || totals.Cached != 8 || totals.Total != 25 {
		t.Fatalf("codex totals %+v", totals)
	}

	// A cumulative event repeated or partially delivered is never summed.
	var repeat usageParser
	repeat.begin(true)
	repeat.observe([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":30}}\n\n"))
	repeat.observe([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":30}}\n\n"))
	repeat.complete()
	if got := repeat.totals(); got.Output != 30 {
		t.Fatalf("cumulative output summed: %+v", got)
	}

	for _, event := range []string{
		"data: {\"type\":\"response.failed\"}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n",
	} {
		var failure usageParser
		failure.begin(true)
		failure.observe([]byte(event))
		failure.complete()
		if !failure.terminalFailure {
			t.Fatalf("terminal failure not detected: %s", event)
		}
	}
}

func TestUsageParserBoundedMemoryAndJSONCapture(t *testing.T) {
	var parser usageParser
	parser.begin(true)
	parser.observe([]byte("data: " + strings.Repeat("x", maxUsageLineBytes+256) + "\n"))
	parser.observe([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"))
	parser.complete()
	if totals := parser.totals(); !totals.Known || totals.Input != 1 || totals.Output != 2 {
		t.Fatalf("oversized line broke later parsing: %+v", totals)
	}

	var body usageParser
	body.begin(false)
	body.observe([]byte(`{"usage":{"input_tokens":7,"output_tokens`))
	body.observe([]byte(`":3}}`))
	body.complete()
	if totals := body.totals(); !totals.Known || totals.Input != 7 || totals.Output != 3 || totals.Total != 10 {
		t.Fatalf("non-stream capture: %+v", totals)
	}

	var overflow usageParser
	overflow.begin(false)
	overflow.observe([]byte(strings.Repeat("x", maxUsageCaptureBytes+1)))
	overflow.complete()
	if totals := overflow.totals(); totals.Known {
		t.Fatalf("oversized body reported known usage: %+v", totals)
	}
}

func TestUsageParserRejectsInvalidCountsAndMarksProviderIncomplete(t *testing.T) {
	var incomplete usageParser
	incomplete.begin(true)
	incomplete.observe([]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}}\n\n"))
	incomplete.complete()
	if !incomplete.providerIncomplete || !incomplete.terminal || incomplete.terminalFailure {
		t.Fatalf("response.incomplete state %+v", incomplete)
	}
	if totals := incomplete.totals(); !totals.Known || totals.Total != 6 {
		t.Fatalf("incomplete usage %+v", totals)
	}

	for _, sample := range []string{
		`{"type":"response.completed","response":{"usage":{"input_tokens":-1,"output_tokens":5}}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":-2}}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":9223372036854775807,"output_tokens":1}}}`,
		`{"type":"response.completed","response":{"usage":{"total_tokens":-9}}}`,
	} {
		var parser usageParser
		parser.begin(false)
		parser.observe([]byte(sample))
		parser.complete()
		if totals := parser.totals(); totals.Known {
			t.Fatalf("invalid counts trusted: %s -> %+v", sample, totals)
		}
	}

	// Saturating arithmetic keeps a valid but extreme sample bounded instead
	// of overflowing into a negative total.
	var extreme usageParser
	extreme.begin(false)
	extreme.observe([]byte(fmt.Sprintf(`{"usage":{"input_tokens":%d,"output_tokens":%d}}`, maxTokenCount, maxTokenCount)))
	extreme.complete()
	if totals := extreme.totals(); !totals.Known || totals.Total < totals.Input || totals.Total <= 0 {
		t.Fatalf("extreme totals %+v", totals)
	}
}

type telemetryEnvelope struct {
	Requests       []telemetryRecord `json:"requests"`
	Totals         telemetryTotals   `json:"totals"`
	RetentionLimit int               `json:"retentionLimit"`
}

func readTelemetry(t *testing.T, s *server) telemetryEnvelope {
	t.Helper()
	w := gatewayCall(s, "", "GET", "/api/telemetry", "")
	if w.Code != 200 {
		t.Fatalf("telemetry: %d %s", w.Code, w.Body)
	}
	var response telemetryEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func awaitTelemetry(t *testing.T, s *server, match func(telemetryEnvelope) bool) telemetryEnvelope {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		response := readTelemetry(t, s)
		if match(response) {
			return response
		}
		if time.Now().After(deadline) {
			t.Fatalf("telemetry did not settle: %+v", response)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTelemetryCapturesAliasUsageAndNeverStoresContent(t *testing.T) {
	s := oauthServer(t)
	a := nativeAccount("acct-1")
	nativeSeed(t, s, a)
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		return nativeSSE("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"total_tokens\":30,\"input_tokens_details\":{\"cached_tokens\":4}}}}\n\n"), nil
	}))

	// Alias the native model so the request uses the public name.
	settings := readModelSettings(t, s)
	body, _ := json.Marshal(map[string]any{"revision": settings.Revision, "models": []map[string]any{{"id": "model", "provider": "Codex", "enabled": true, "alias": "fast"}}})
	if w := oauthCall(s, "PUT", "/api/model-settings", string(body)); w.Code != 200 {
		t.Fatalf("alias: %d %s", w.Code, w.Body)
	}
	const prompt = "PROMPT-TEXT-MUST-NEVER-BE-PERSISTED"
	if w := nativeCall(s, "POST", "/v1/responses", "client", `{"model":"fast","input":"`+prompt+`","stream":true}`); w.Code != 200 {
		t.Fatalf("inference: %d %s", w.Code, w.Body)
	}
	response := readTelemetry(t, s)
	if response.RetentionLimit != telemetryRetention || len(response.Requests) != 1 {
		t.Fatalf("telemetry shape %+v", response)
	}
	record := response.Requests[0]
	if record.Model != "fast" || record.NativeModel != "model" || record.Provider != "codex" || record.AccountID != "acct-1" {
		t.Fatalf("routing fields %+v", record)
	}
	if record.GatewayID != defaultGatewayID || record.KeyID != "" || record.KeyName != "VROUTER_API_KEY" {
		t.Fatalf("key fields %+v", record)
	}
	if record.Status != 200 || record.Outcome != outcomeSuccess || !record.Stream {
		t.Fatalf("outcome fields %+v", record)
	}
	if !record.UsageKnown || record.InputTokens != 10 || record.OutputTokens != 20 || record.CachedTokens != 4 || record.TotalTokens != 30 {
		t.Fatalf("usage fields %+v", record)
	}
	if record.DurationMs < 0 || record.ID == "" || record.StartedAt.IsZero() {
		t.Fatalf("timing fields %+v", record)
	}
	if response.Totals.Requests != 1 || response.Totals.InputTokens != 10 || response.Totals.OutputTokens != 20 || response.Totals.TotalTokens != 30 {
		t.Fatalf("totals %+v", response.Totals)
	}
	if strings.Contains(readTelemetryRaw(t, s), prompt) {
		t.Fatal("telemetry persisted request content")
	}
}

func readTelemetryRaw(t *testing.T, s *server) string {
	t.Helper()
	w := gatewayCall(s, "", "GET", "/api/telemetry", "")
	return w.Body.String()
}

func readModelSettings(t *testing.T, s *server) modelSettings {
	t.Helper()
	w := oauthCall(s, "GET", "/api/model-settings", "")
	if w.Code != 200 {
		t.Fatalf("model settings: %d %s", w.Code, w.Body)
	}
	var settings modelSettings
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestTelemetryUnknownUsageManagedKeysAndOutcomes(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	var withUsage atomic.Bool
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		if withUsage.Load() {
			return nativeSSE("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":6}}}\n\n"), nil
		}
		return nativeSSE("data: done\n\n"), nil
	}))
	key, secret := createKeyOn(t, s, defaultGatewayID, "metered", nil, nil)

	// Unknown usage is recorded as unknown, never as known zero.
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("unknown usage request: %d %s", w.Code, w.Body)
	}
	response := readTelemetry(t, s)
	record := response.Requests[0]
	if record.UsageKnown || record.InputTokens != 0 || record.OutputTokens != 0 || record.TotalTokens != 0 {
		t.Fatalf("unknown usage %+v", record)
	}
	if record.KeyID != key.ID || record.KeyName != "metered" || record.Outcome != outcomeIncomplete {
		t.Fatalf("managed key fields %+v", record)
	}

	// A model policy refusal is an error outcome.
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"missing","stream":true}`); w.Code != 404 {
		t.Fatalf("blocked model: %d", w.Code)
	}
	response = readTelemetry(t, s)
	if response.Requests[0].Outcome != outcomeError || response.Requests[0].Status != 404 {
		t.Fatalf("error outcome %+v", response.Requests[0])
	}
	if response.Totals.Requests != 2 {
		t.Fatalf("totals rows %+v", response.Totals)
	}

	// Deleting the key preserves its past telemetry.
	if w := gatewayCall(s, defaultGatewayID, "DELETE", "/api/keys/"+key.ID, ""); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	response = readTelemetry(t, s)
	found := false
	for _, record := range response.Requests {
		if record.KeyID == key.ID && record.KeyName == "metered" {
			found = true
		}
	}
	if !found {
		t.Fatal("telemetry lost after key deletion")
	}
	withUsage.Store(true)
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 401 {
		t.Fatalf("deleted key reused: %d", w.Code)
	}
}

func TestTelemetryRetentionBound(t *testing.T) {
	s := oauthServer(t)
	err := s.manager.registry.update(func(registry *diskRegistry) error {
		records := make([]telemetryRecord, 0, telemetryRetention+5)
		for i := 0; i < telemetryRetention+5; i++ {
			records = append(records, telemetryRecord{ID: "r", StartedAt: time.Now(), GatewayID: defaultGatewayID, Outcome: outcomeSuccess, Status: 200})
		}
		registry.Telemetry[defaultGatewayID] = records
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	response := readTelemetry(t, s)
	if len(response.Requests) != telemetryRetention {
		t.Fatalf("retained %d records", len(response.Requests))
	}
	if response.Totals.Requests != telemetryRetention {
		t.Fatalf("totals %+v", response.Totals)
	}
}

type failingReader struct {
	payload []byte
	read    bool
}

const (
	modeTerminal int32 = iota
	modeReadError
	modeDisconnect
)

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.read {
		f.read = true
		n := copy(p, f.payload)
		return n, nil
	}
	return 0, errors.New("provider connection dropped")
}

func (f *failingReader) Close() error { return nil }

func TestTelemetryDisconnectReadErrorAndTerminalSSE(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	var mode atomic.Int32
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		switch mode.Load() {
		case modeReadError:
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &failingReader{payload: []byte("data: first\n\n")}}, nil
		case modeDisconnect:
			reader, writer := io.Pipe()
			go func() {
				defer writer.Close()
				_, _ = io.WriteString(writer, "data: first\n\n")
				// Enough data to overflow socket buffers after the client
				// disappears, so the handler observes a write failure instead
				// of a clean provider EOF.
				payload := "data: " + strings.Repeat("y", 64<<10) + "\n\n"
				for range 64 {
					if _, err := io.WriteString(writer, payload); err != nil {
						return
					}
				}
			}()
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
		default:
			return nativeSSE("data: {\"type\":\"response.failed\"}\n\n"), nil
		}
	}))

	mode.Store(modeTerminal)
	if w := nativeCall(s, "POST", "/v1/responses", "client", `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("terminal: %d %s", w.Code, w.Body)
	}
	response := awaitTelemetry(t, s, func(response telemetryEnvelope) bool {
		return len(response.Requests) == 1
	})
	if response.Requests[0].Outcome != outcomeError || response.Requests[0].Status != 200 {
		t.Fatalf("terminal SSE outcome %+v", response.Requests[0])
	}

	mode.Store(modeReadError)
	if w := nativeCall(s, "POST", "/v1/responses", "client", `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("read error request: %d", w.Code)
	}
	response = awaitTelemetry(t, s, func(response telemetryEnvelope) bool {
		return len(response.Requests) == 2
	})
	if response.Requests[0].Outcome != outcomeError {
		t.Fatalf("read error outcome %+v", response.Requests[0])
	}

	// A client that disappears mid-stream yields an incomplete outcome while
	// the provider body is still closed.
	mode.Store(modeDisconnect)
	host := httptest.NewServer(s)
	defer host.Close()
	req, _ := http.NewRequest("POST", host.URL+"/v1/responses", strings.NewReader(`{"model":"model","stream":true}`))
	req.Header.Set("Authorization", "Bearer client")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	if first, err := reader.ReadString('\n'); err != nil || first != "data: first\n" {
		resp.Body.Close()
		t.Fatalf("stream buffered: %q %v", first, err)
	}
	resp.Body.Close()
	response = awaitTelemetry(t, s, func(response telemetryEnvelope) bool {
		return len(response.Requests) == 3
	})
	if response.Requests[0].Outcome != outcomeIncomplete {
		t.Fatalf("disconnect outcome %+v", response.Requests[0])
	}
	if response.Totals.Requests != 3 {
		t.Fatalf("totals %+v", response.Totals)
	}
}

func TestTelemetryIsPerGateway(t *testing.T) {
	s := oauthServer(t)
	g := createGateway(t, s, "metrics")
	child, err := s.manager.engine(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	nativeSeed(t, child, nativeAccount("child"))
	nativeMock(child, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		return nativeSSE("data: done\n\n"), nil
	}))
	_, secret := createKeyOn(t, s, g.ID, "meter", nil, nil)
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("child inference: %d %s", w.Code, w.Body)
	}
	childTelemetry := gatewayCall(s, g.ID, "GET", "/api/telemetry", "")
	if !strings.Contains(childTelemetry.Body.String(), `"gatewayId":"`+g.ID+`"`) {
		t.Fatalf("child telemetry %s", childTelemetry.Body)
	}
	defaultTelemetry := gatewayCall(s, defaultGatewayID, "GET", "/api/telemetry", "")
	if strings.Contains(defaultTelemetry.Body.String(), `"gatewayId":"`+g.ID+`"`) || !strings.Contains(defaultTelemetry.Body.String(), `"requests":[]`) {
		t.Fatalf("telemetry crossed gateways %s", defaultTelemetry.Body)
	}
}
