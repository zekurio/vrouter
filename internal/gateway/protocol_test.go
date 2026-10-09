package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func addProtocolAccount(t *testing.T, s *server, id, provider, mode string) {
	t.Helper()
	if err := s.store.update(func(d *diskState) error {
		d.Accounts = append(d.Accounts, storedAccount{ID: id, Provider: provider, AuthMode: mode, AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func sseResponse(data string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(data))}
}
func frame(kind, data string) string { return "event: " + kind + "\ndata: " + data + "\n\n" }

func claudeToolStream(stop string) string {
	return frame("message_start", `{"type":"message_start","message":{"id":"msg_test","usage":{"input_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4,"output_tokens":0}}}`) +
		frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`) +
		frame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		frame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_test","name":"lookup","input":{}}}`) +
		frame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`) +
		frame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Vienna\"}"}}`) +
		frame("content_block_stop", `{"type":"content_block_stop","index":1}`) +
		frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`"},"usage":{"output_tokens":5}}`) +
		frame("message_stop", `{"type":"message_stop"}`)
}

func responsesToolStream(status string) string {
	return frame("response.created", `{"type":"response.created","response":{"id":"resp_test","status":"in_progress"}}`) +
		frame("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_test","type":"function_call","call_id":"call_test","name":"lookup","arguments":""}}`) +
		frame("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_test","delta":"{\"city\":"}`) +
		frame("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_test","delta":"\"Vienna\"}"}`) +
		frame("response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_test","arguments":"{\"city\":\"Vienna\"}"}`) +
		frame("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_test","type":"function_call","call_id":"call_test","name":"lookup","arguments":"{\"city\":\"Vienna\"}"}}`) +
		frame("response."+status, `{"type":"response.`+status+`","response":{"id":"resp_test","status":"`+status+`","output":[{"id":"fc_test","type":"function_call","call_id":"call_test","name":"lookup","arguments":"{\"city\":\"Vienna\"}"}],"usage":{"input_tokens":9,"output_tokens":5,"total_tokens":14,"input_tokens_details":{"cached_tokens":3}}}}`)
}

func TestProtocolsRouteByCatalog(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
			t.Run(provider+path, func(t *testing.T) {
				s := testServer(t, Config{ExternalAuth: true})
				addProtocolAccount(t, s, "account", provider, "api_key")
				_, secret := newTestKey(t, s, `{}`)
				var sent map[string]any
				transport := roundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/v1/models" {
						return jsonResponse(`{"data":[{"id":"catalog-model"}]}`, 200), nil
					}
					target := "/v1/responses"
					if provider == "claude" {
						target = "/v1/messages"
					}
					if r.URL.Path != target {
						t.Errorf("routed %s to %s", provider, r.URL.Path)
					}
					_ = json.NewDecoder(r.Body).Decode(&sent)
					if provider == "claude" {
						return jsonResponse(`{"id":"msg","type":"message","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`, 200), nil
					}
					return jsonResponse(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","id":"m","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`, 200), nil
				})
				s.client.Transport, s.streamClient.Transport = transport, transport
				body := `{"model":"catalog-model","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`
				if path == "/v1/responses" {
					body = `{"model":"catalog-model","input":"hi","max_output_tokens":256,"store":false}`
				}
				if path == "/v1/chat/completions" {
					body = `{"model":"catalog-model","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":256}`
				}
				w := call(t, s, "POST", path, body, secret)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "hello") {
					t.Fatalf("reply %d %s", w.Code, w.Body.String())
				}
				capField := "max_output_tokens"
				if provider == "claude" {
					capField = "max_tokens"
				}
				if sent[capField] != float64(256) {
					t.Fatalf("token cap lost: %+v", sent)
				}
				rows := s.manager.telemetry(defaultGatewayID).Requests
				if len(rows) != 1 || rows[0].Provider != provider || !rows[0].UsageKnown || rows[0].TotalTokens != 5 {
					t.Fatalf("accounting %+v", rows)
				}
			})
		}
	}
}

func TestProtocolsPreserveConversationAndRejectLoss(t *testing.T) {
	source := `{"model":"m","input":[{"role":"system","content":"system"},{"role":"user","content":[{"type":"input_text","text":"question"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]},{"type":"function_call","call_id":"call_test","name":"lookup","arguments":"{\"city\":\"Vienna\"}"},{"type":"function_call_output","call_id":"call_test","output":"result"}],"tools":[{"type":"function","name":"lookup","description":"find city","strict":true,"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"function","name":"lookup"},"reasoning":{"effort":"high"},"max_output_tokens":256,"store":false}`
	p, err := decodeProtocolJSON([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	claude, err := adaptInferenceRequest(p, responsesProtocol, messagesProtocol)
	if err != nil {
		t.Fatal(err)
	}
	if object(list(claude["tools"])[0])["strict"] != true {
		t.Fatal("strict tool control was lost")
	}
	if object(claude["output_config"])["effort"] != "high" || object(claude["tool_choice"])["name"] != "lookup" {
		t.Fatalf("controls lost %+v", claude)
	}
	back, err := adaptInferenceRequest(claude, messagesProtocol, responsesProtocol)
	if err != nil {
		t.Fatal(err)
	}
	if object(list(back["tools"])[0])["strict"] != true {
		t.Fatal("strict tool control was lost on return")
	}
	roundtrip, _ := json.Marshal(back)
	for _, text := range []string{"system", "question", "data:image/png;base64,aGVsbG8=", "function_call_output", "call_test", "Vienna", "find city"} {
		if !strings.Contains(string(roundtrip), text) {
			t.Errorf("lost %s in %s", text, roundtrip)
		}
	}
	for _, input := range []string{
		`{"input":"hi","previous_response_id":"prior"}`,
		`{"input":"hi","tools":[{"type":"web_search"}]}`,
		`{"input":"hi","text":{"format":{"type":"json_schema"}}}`,
		`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`,
	} {
		p, _ := decodeProtocolJSON([]byte(input))
		if _, err := adaptInferenceRequest(p, responsesProtocol, messagesProtocol); err == nil {
			t.Errorf("silent loss for %s", input)
		}
	}
	p, _ = decodeProtocolJSON([]byte(`{"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":2048},"max_tokens":4096}`))
	if adapted, err := adaptInferenceRequest(p, messagesProtocol, responsesProtocol); err != nil || object(adapted["reasoning"])["effort"] != "medium" {
		t.Fatalf("fixed reasoning budget was not mapped to effort: %+v %v", adapted, err)
	}
}

type tinyProtocolReader struct {
	data string
	size int
}

func (r *tinyProtocolReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, io.EOF
	}
	n := min(len(p), len(r.data), r.size)
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestProtocolStreamsTranslateToolFragments(t *testing.T) {
	for _, source := range []wireProtocol{messagesProtocol, responsesProtocol} {
		for _, target := range []wireProtocol{messagesProtocol, responsesProtocol, chatProtocol} {
			if source == target {
				continue
			}
			t.Run(string(source)+"-"+string(target), func(t *testing.T) {
				w := httptest.NewRecorder()
				reply := &protocolReply{model: "public", usage: map[string]any{}}
				sink := &replySink{reply: reply, emitter: &protocolEmitter{w: w, protocol: target, reply: reply}}
				data := claudeToolStream("tool_use")
				if source == responsesProtocol {
					data = responsesToolStream("completed")
				}
				if err := readProtocolSSE(&tinyProtocolReader{data: strings.ReplaceAll(data, "\n", "\r\n"), size: 3}, source, sink); err != nil {
					t.Fatal(err)
				}
				if !reply.terminal || reply.status != "completed" {
					t.Fatalf("terminal %+v", reply)
				}
				if got := reply.blocks[len(reply.blocks)-1]; got.callID != "call_test" || got.name != "lookup" || got.text != `{"city":"Vienna"}` {
					t.Fatalf("tool %+v", got)
				}
				body := w.Body.String()
				if !strings.Contains(body, "Vienna") || !strings.Contains(body, "call_test") {
					t.Fatalf("lost tool %s", body)
				}
				terminal := "response.completed"
				if target == messagesProtocol {
					terminal = "message_stop"
				}
				if target == chatProtocol {
					terminal = "[DONE]"
				}
				if !strings.Contains(body, terminal) {
					t.Fatalf("missing terminal %s", body)
				}
				in, out, cache, write, _ := reply.counts()
				if in != 9 || out != 5 || cache != 3 {
					t.Fatalf("usage %d %d %d", in, out, cache)
				}
				if source == messagesProtocol && write != 4 {
					t.Fatalf("cache writes lost %d", write)
				}
			})
		}
	}
}

func TestProtocolStreamIsIncremental(t *testing.T) {
	reader, writer := io.Pipe()
	w := httptest.NewRecorder()
	reply := &protocolReply{model: "m", usage: map[string]any{}}
	sink := &replySink{reply: reply, emitter: &protocolEmitter{w: w, protocol: chatProtocol, reply: reply}}
	complete := make(chan error, 1)
	go func() { complete <- readProtocolSSE(reader, messagesProtocol, sink) }()
	first := frame("message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`) + frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) + frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first"}}`)
	if _, err := io.WriteString(writer, first); err != nil {
		t.Fatal(err)
	}
	// A second write makes the first batch finish before this assertion.
	if _, err := io.WriteString(writer, ": keepalive\n\n"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), "first") {
		t.Fatal("adapter buffered the whole stream")
	}
	_ = writer.Close()
	if err := <-complete; err != nil {
		t.Fatal(err)
	}
	if reply.terminal {
		t.Fatal("truncated stream became complete")
	}
}

func TestCodexNonstreamKeepsNativeResponse(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	addProtocolAccount(t, s, "a", "codex", "codex")
	_, secret := newTestKey(t, s, `{}`)
	transport := roundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			return jsonResponse(`{"models":[{"slug":"model","visibility":"list"}]}`, 200), nil
		}
		if strings.HasSuffix(r.URL.Path, "/usage") {
			return jsonResponse(`{}`, 200), nil
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p["stream"] != true || p["store"] != false {
			t.Errorf("Codex contract %+v", p)
		}
		return sseResponse(responsesToolStream("completed")), nil
	})
	s.client.Transport, s.streamClient.Transport = transport, transport
	w := call(t, s, "POST", "/v1/responses", `{"model":"model","input":"hello"}`, secret)
	if w.Code != 200 || strings.Contains(w.Body.String(), "data:") || !strings.Contains(w.Body.String(), "function_call") {
		t.Fatalf("reply %d %s", w.Code, w.Body.String())
	}
	row := s.manager.telemetry(defaultGatewayID).Requests[0]
	if row.Stream || !row.UsageKnown || row.TotalTokens != 14 {
		t.Fatalf("raw accounting %+v", row)
	}
	w = call(t, s, "POST", "/v1/responses", `{"model":"model","input":"hello","max_output_tokens":256}`, secret)
	if w.Code != 200 || w.Header().Get("X-Vrouter-Ignored-Parameters") != "" {
		t.Fatalf("valid cap rejected or ignored %d %s", w.Code, w.Body.String())
	}
}

func TestCrossProtocolQuotaUsesSelectedProvider(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	addProtocolAccount(t, s, "a", "claude", "api_key")
	_, secret := newTestKey(t, s, `{"claude":{"fiveHour":0},"codex":{}}`)
	sent := false
	transport := roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/models" {
			return jsonResponse(`{"data":[{"id":"claude-model"}]}`, 200), nil
		}
		sent = true
		return jsonResponse(`{}`, 200), nil
	})
	s.client.Transport, s.streamClient.Transport = transport, transport
	w := call(t, s, "POST", "/v1/responses", `{"model":"claude-model","input":"hi"}`, secret)
	if w.Code != 429 || sent {
		t.Fatalf("wrong provider admission %d sent=%v %s", w.Code, sent, w.Body.String())
	}
}

func TestAmbiguousModelsNeedAnAlias(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	addProtocolAccount(t, s, "a", "claude", "api_key")
	addProtocolAccount(t, s, "b", "codex", "api_key")
	_, secret := newTestKey(t, s, `{}`)
	transport := roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/models" {
			return jsonResponse(`{"data":[{"id":"shared"}]}`, 200), nil
		}
		return jsonResponse(`{"content":[],"usage":{"input_tokens":0,"output_tokens":0}}`, 200), nil
	})
	s.client.Transport, s.streamClient.Transport = transport, transport
	w := call(t, s, "POST", "/v1/responses", `{"model":"shared","input":"hi"}`, secret)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "more than one provider") {
		t.Fatalf("ambiguity %d %s", w.Code, w.Body.String())
	}
	if err := s.store.update(func(d *diskState) error {
		d.Policy.Aliases["claude"] = []modelAlias{{Name: "shared", Alias: "claude-public"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w = call(t, s, "POST", "/v1/responses", `{"model":"claude-public","input":"hi"}`, secret)
	if w.Code != 200 {
		t.Fatalf("alias %d %s", w.Code, w.Body.String())
	}
	// Refusals carry the error type of their status, not one catch-all code.
	for body, want := range map[string][2]any{
		`{"model":"absent","input":"hi"}`:                                        {404, "not_found_error"},
		`{"model":"claude-public","input":"hi","previous_response_id":"resp_1"}`: {400, "invalid_request_error"},
	} {
		w = call(t, s, "POST", "/v1/responses", body, secret)
		var refused struct {
			Error struct{ Type, Code string }
		}
		if json.Unmarshal(w.Body.Bytes(), &refused) != nil || w.Code != want[0] || refused.Error.Type != want[1] {
			t.Fatalf("refusal %d %s", w.Code, w.Body.String())
		}
		if unsupported := refused.Error.Code == "unsupported_protocol_feature"; unsupported != (w.Code == 400) {
			t.Fatalf("refusal code %s", w.Body.String())
		}
	}
}

type brokenProtocolReader struct{ data string }

func (r *brokenProtocolReader) Read(p []byte) (int, error) {
	if r.data != "" {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, errors.New("upstream disconnected")
}

func TestProtocolFailureOutcomes(t *testing.T) {
	for _, scenario := range []string{"incomplete", "missing-terminal", "read-error", "provider-error"} {
		t.Run(scenario, func(t *testing.T) {
			s := testServer(t, Config{ExternalAuth: true})
			addProtocolAccount(t, s, "a", "codex", "api_key")
			_, secret := newTestKey(t, s, `{}`)
			transport := roundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/v1/models" {
					return jsonResponse(`{"data":[{"id":"model"}]}`, 200), nil
				}
				data := responsesToolStream("incomplete")
				if scenario == "provider-error" {
					data = frame("error", `{"type":"error","code":"broken","message":"provider failed for test-token"}`)
				}
				if scenario == "missing-terminal" || scenario == "read-error" {
					data = strings.Split(data, "event: response.incomplete")[0]
				}
				resp := sseResponse(data)
				if scenario == "read-error" {
					resp.Body = io.NopCloser(&brokenProtocolReader{data: data})
				}
				return resp, nil
			})
			s.client.Transport, s.streamClient.Transport = transport, transport
			w := call(t, s, "POST", "/v1/chat/completions", `{"model":"model","messages":[{"role":"user","content":"hi"}],"stream":true}`, secret)
			if w.Code != 200 {
				t.Fatalf("status %d %s", w.Code, w.Body.String())
			}
			if scenario == "provider-error" && (strings.Contains(w.Body.String(), "test-token") || !strings.Contains(w.Body.String(), providerRedacted)) {
				t.Fatal("streamed provider error did not redact the account credential")
			}
			row := s.manager.telemetry(defaultGatewayID).Requests[0]
			if scenario == "incomplete" {
				if row.Outcome != outcomeIncomplete || !row.UsageKnown || !strings.Contains(w.Body.String(), `"finish_reason":"length"`) {
					t.Fatalf("incomplete %+v %s", row, w.Body.String())
				}
			} else {
				if row.Outcome == outcomeSuccess || row.UsageKnown || !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), "[DONE]") {
					t.Fatalf("failure %+v %s", row, w.Body.String())
				}
			}
		})
	}
}

func TestProtocolDefaultOptionsAndStrictTools(t *testing.T) {
	defaults, _ := decodeProtocolJSON([]byte(`{"messages":[{"role":"user","content":"hi"}],"store":false,"modalities":["text"],"verbosity":"medium","logprobs":false,"top_logprobs":0.0,"tools":[{"type":"function","function":{"name":"clock"}}]}`))
	for _, target := range []wireProtocol{messagesProtocol, responsesProtocol} {
		out, err := adaptInferenceRequest(defaults, chatProtocol, target)
		if err != nil {
			t.Fatal(err)
		}
		field := "parameters"
		if target == messagesProtocol {
			field = "input_schema"
		}
		if object(object(list(out["tools"])[0])[field])["type"] != "object" {
			t.Fatal("no-arg function has no schema")
		}
		if target == responsesProtocol && out["store"] != false {
			t.Fatal("store false was lost")
		}
	}
	for _, numeric := range []string{`"n":1,"frequency_penalty":0,"presence_penalty":0`, `"n":1.0,"frequency_penalty":0.0,"presence_penalty":0e0`, `"n":1e0,"frequency_penalty":-0.0,"presence_penalty":0.000`} {
		p, _ := decodeProtocolJSON([]byte(`{"messages":[{"role":"user","content":"hi"}],` + numeric + `,"logprobs":false,"service_tier":"auto","response_format":{"type":"text"},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"},"strict":true}}]}`))
		for _, target := range []wireProtocol{responsesProtocol, messagesProtocol} {
			out, err := adaptInferenceRequest(p, chatProtocol, target)
			if err != nil {
				t.Fatalf("numeric defaults %s: %v", numeric, err)
			}
			tool := object(list(out["tools"])[0])
			if tool["strict"] != true {
				t.Fatal("strict flag lost")
			}
			if tool["description"] != nil {
				t.Fatal("missing description became null")
			}
		}
	}
	p, _ := decodeProtocolJSON([]byte(`{"input":"hi","include":[],"metadata":{},"background":false,"truncation":"disabled","text":{"format":{"type":"text"},"verbosity":"medium"},"reasoning":{"effort":"medium","summary":"auto"}}`))
	if _, err := adaptInferenceRequest(p, responsesProtocol, messagesProtocol); err != nil {
		t.Fatal(err)
	}
	for _, numeric := range []string{`"n":2`, `"n":1.000000000000000000000000000001`, `"frequency_penalty":0.01`, `"presence_penalty":1e-100`} {
		p, _ := decodeProtocolJSON([]byte(`{"messages":[{"role":"user","content":"hi"}],` + numeric + `}`))
		if _, err := adaptInferenceRequest(p, chatProtocol, messagesProtocol); err == nil {
			t.Fatalf("unsupported number dropped: %s", numeric)
		}
	}
}

func TestProtocolsDoNotCrossBillingModes(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	addProtocolAccount(t, s, "subscription", "claude", "oauth")
	addProtocolAccount(t, s, "paid", "claude", "api_key")
	_, secret := newTestKey(t, s, `{}`)
	sent := 0
	transport := roundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/models":
			return jsonResponse(`{"data":[{"id":"model"}]}`, 200), nil
		case "/api/oauth/profile", "/api/oauth/usage":
			return jsonResponse(`{}`, 200), nil
		case "/v1/messages":
			sent++
			if r.Header.Get("X-Api-Key") != "" {
				t.Error("used a paid API key fallback")
			}
			return jsonResponse(`{"error":{"type":"overloaded_error","message":"Busy"}}`, 503), nil
		}
		t.Errorf("unexpected path %s", r.URL.Path)
		return jsonResponse(`{}`, 404), nil
	})
	s.client.Transport, s.streamClient.Transport = transport, transport
	w := call(t, s, "POST", "/v1/responses", `{"model":"model","input":"hi"}`, secret)
	if w.Code != 503 || sent != 1 {
		t.Fatalf("billing boundary status=%d sent=%d %s", w.Code, sent, w.Body.String())
	}
}

type disconnectProtocolWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *disconnectProtocolWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(data)
}

func TestProtocolClientDisconnectSettlesAttempt(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	addProtocolAccount(t, s, "a", "claude", "api_key")
	id, secret := newTestKey(t, s, `{}`)
	transport := roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/models" {
			return jsonResponse(`{"data":[{"id":"model"}]}`, 200), nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&tinyProtocolReader{data: claudeToolStream("tool_use"), size: 3})}, nil
	})
	s.client.Transport, s.streamClient.Transport = transport, transport
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	r.Header.Set("Authorization", "Bearer "+secret)
	w := &disconnectProtocolWriter{ResponseRecorder: httptest.NewRecorder()}
	s.ServeHTTP(w, r)
	rows := s.manager.telemetry(defaultGatewayID).Requests
	if len(rows) != 1 || rows[0].Outcome != outcomeIncomplete || rows[0].UsageKnown {
		t.Fatalf("disconnect accounting %+v", rows)
	}
	registry := s.manager.registry.snapshot()
	if findKey(&registry, id).InFlight != 0 {
		t.Fatal("disconnect kept the reservation")
	}
}

func TestNativeProtocolPassthroughKeepsProviderFields(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	addProtocolAccount(t, s, "a", "codex", "api_key")
	_, secret := newTestKey(t, s, `{}`)
	result := `{"id":"resp","status":"completed","output":[{"type":"image_generation_call","result":"image-data"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`
	transport := roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/models" {
			return jsonResponse(`{"data":[{"id":"model"}]}`, 200), nil
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if str(object(object(p["text"])["format"])["type"]) != "json_schema" {
			t.Error("native format changed")
		}
		return jsonResponse(result, 200), nil
	})
	s.client.Transport, s.streamClient.Transport = transport, transport
	w := call(t, s, "POST", "/v1/responses", `{"model":"model","input":"hi","text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}}`, secret)
	if w.Code != 200 || w.Body.String() != result {
		t.Fatalf("native response changed %d %s", w.Code, w.Body.String())
	}
}

func TestNativeCodexAssemblyUsesStreamItems(t *testing.T) {
	data := strings.Split(responsesToolStream("completed"), "event: response.completed")[0] + frame("response.completed", `{"type":"response.completed","response":{"id":"resp_test","status":"completed","usage":{"input_tokens":9,"output_tokens":5,"total_tokens":14}}}`)
	reply := &protocolReply{usage: map[string]any{}}
	if err := readProtocolSSE(strings.NewReader(data), responsesProtocol, &replySink{reply: reply, native: true}); err != nil {
		t.Fatal(err)
	}
	output := list(reply.raw["output"])
	if len(output) != 1 || object(output[0])["arguments"] != `{"city":"Vienna"}` {
		t.Fatalf("stream items lost %+v", reply.raw)
	}
}

func TestProtocolNonstreamToolReplies(t *testing.T) {
	for _, target := range []wireProtocol{messagesProtocol, responsesProtocol, chatProtocol} {
		t.Run(string(target), func(t *testing.T) {
			reply := &protocolReply{usage: map[string]any{}}
			body, _ := decodeProtocolJSON([]byte(`{"id":"msg","content":[{"type":"tool_use","id":"call","name":"lookup","input":{"city":"Vienna"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`))
			if err := consumeProtocolJSON(body, messagesProtocol, &replySink{reply: reply}); err != nil {
				t.Fatal(err)
			}
			result := reply.encode(target)
			var arguments any
			switch target {
			case messagesProtocol:
				arguments = object(list(result["content"])[0])["input"]
			case responsesProtocol:
				arguments = object(list(result["output"])[0])["arguments"]
			default:
				message := object(object(list(result["choices"])[0])["message"])
				arguments = object(object(list(message["tool_calls"])[0])["function"])["arguments"]
			}
			if target == messagesProtocol {
				if object(arguments)["city"] != "Vienna" {
					t.Fatalf("tool args %+v", arguments)
				}
			} else if arguments != `{"city":"Vienna"}` {
				t.Fatalf("tool args %+v", arguments)
			}
		})
	}
}

func TestProtocolChatUsageOption(t *testing.T) {
	for _, include := range []bool{false, true} {
		w := httptest.NewRecorder()
		reply := &protocolReply{usage: map[string]any{}}
		if err := readProtocolSSE(strings.NewReader(claudeToolStream("tool_use")), messagesProtocol, &replySink{reply: reply, emitter: &protocolEmitter{w: w, protocol: chatProtocol, reply: reply, includeUsage: include}}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(w.Body.String(), `"choices":[]`) != include {
			t.Fatalf("include_usage=%v %s", include, w.Body.String())
		}
	}
}

func nextProtocolToolRequest(t *testing.T, reply map[string]any, protocol wireProtocol) map[string]any {
	t.Helper()
	p := map[string]any{"model": "m"}
	switch protocol {
	case responsesProtocol:
		p["input"] = append(list(reply["output"]), map[string]any{"type": "function_call_output", "call_id": "call_test", "output": "Sunny"})
	case messagesProtocol:
		p["messages"] = []any{map[string]any{"role": "assistant", "content": reply["content"]}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_test", "content": "Sunny"}}}}
	case chatProtocol:
		message := object(object(list(reply["choices"])[0])["message"])
		p["messages"] = []any{message, map[string]any{"role": "tool", "tool_call_id": "call_test", "content": "Sunny"}}
	}
	raw, _ := json.Marshal(p)
	p, err := decodeProtocolJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func signedClaudeStream() string {
	start, rest, _ := strings.Cut(claudeToolStream("tool_use"), "\n\n")
	rest = strings.ReplaceAll(strings.ReplaceAll(rest, `"index":1`, `"index":2`), `"index":0`, `"index":1`)
	return start + "\n\n" + frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) +
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"I should call lookup."}}`) +
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed-"}}`) +
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"claude-state"}}`) +
		frame("content_block_stop", `{"type":"content_block_stop","index":0}`) + rest
}

func opaqueResponsesStream() string {
	item := `{"id":"reason_test","type":"reasoning","content":[],"summary":[{"type":"summary_text","text":"I should call lookup."}],"encrypted_content":"opaque-openai-state"}`
	tool := `{"id":"fc_test","type":"function_call","call_id":"call_test","name":"lookup","arguments":"{\"city\":\"Vienna\"}"}`
	return frame("response.created", `{"type":"response.created","response":{"id":"resp_test","status":"in_progress"}}`) +
		frame("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"reason_test","type":"reasoning","summary":[]}}`) +
		frame("response.reasoning_summary_part.added", `{"type":"response.reasoning_summary_part.added","output_index":0,"item_id":"reason_test","summary_index":0,"part":{"type":"summary_text","text":""}}`) +
		frame("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"reason_test","summary_index":0,"delta":"I should call lookup."}`) +
		frame("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":`+item+`}`) +
		frame("response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_test","type":"function_call","call_id":"call_test","name":"lookup","arguments":""}}`) +
		frame("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_test","delta":"{\"city\":\"Vienna\"}"}`) +
		frame("response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":`+tool+`}`) +
		frame("response.completed", `{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[`+item+`,`+tool+`],"usage":{"input_tokens":9,"output_tokens":5,"total_tokens":14}}}`)
}

func replayFromClientSSE(t *testing.T, data string, protocol wireProtocol) map[string]any {
	t.Helper()
	if protocol == chatProtocol {
		message := map[string]any{"role": "assistant", "content": ""}
		calls := []any{}
		states := []any{}
		for _, line := range strings.Split(data, "\n") {
			if !strings.HasPrefix(line, "data: ") || strings.HasSuffix(line, "[DONE]") {
				continue
			}
			body, err := decodeProtocolJSON([]byte(strings.TrimPrefix(line, "data: ")))
			if err != nil {
				t.Fatal(err)
			}
			if len(list(body["choices"])) == 0 {
				continue
			}
			delta := object(object(list(body["choices"])[0])["delta"])
			message["content"] = str(message["content"]) + str(delta["content"])
			if v := delta["reasoning_content"]; v != nil {
				message["reasoning_content"] = str(message["reasoning_content"]) + str(v)
			}
			states = append(states, list(delta["vrouter_reasoning"])...)
			for _, value := range list(delta["tool_calls"]) {
				call := object(value)
				index, _ := strconv.Atoi(replyIndex(call["index"]))
				for len(calls) <= index {
					calls = append(calls, map[string]any{"type": "function", "function": map[string]any{"arguments": ""}})
				}
				stored := object(calls[index])
				function := object(stored["function"])
				if call["id"] != nil {
					stored["id"] = call["id"]
				}
				if object(call["function"])["name"] != nil {
					function["name"] = object(call["function"])["name"]
				}
				function["arguments"] = str(function["arguments"]) + str(object(call["function"])["arguments"])
			}
		}
		message["tool_calls"], message["vrouter_reasoning"] = calls, states
		return map[string]any{"choices": []any{map[string]any{"message": message}}}
	}
	if protocol == responsesProtocol {
		for _, line := range strings.Split(data, "\n") {
			if strings.HasPrefix(line, "data: ") {
				body, _ := decodeProtocolJSON([]byte(strings.TrimPrefix(line, "data: ")))
				if str(body["type"]) == "response.completed" {
					return object(body["response"])
				}
			}
		}
		t.Fatal("no completed Responses event")
	}
	content := []any{}
	for _, line := range strings.Split(data, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		body, _ := decodeProtocolJSON([]byte(strings.TrimPrefix(line, "data: ")))
		index, _ := strconv.Atoi(replyIndex(body["index"]))
		if str(body["type"]) == "content_block_start" {
			for len(content) <= index {
				content = append(content, nil)
			}
			content[index] = body["content_block"]
		}
		if str(body["type"]) == "content_block_delta" {
			block := object(content[index])
			delta := object(body["delta"])
			switch str(delta["type"]) {
			case "text_delta":
				block["text"] = str(block["text"]) + str(delta["text"])
			case "thinking_delta":
				block["thinking"] = str(block["thinking"]) + str(delta["thinking"])
			case "signature_delta":
				block["signature"] = str(block["signature"]) + str(delta["signature"])
			case "input_json_delta":
				block["arguments"] = str(block["arguments"]) + str(delta["partial_json"])
			}
		}
		if str(body["type"]) == "content_block_stop" {
			block := object(content[index])
			if str(block["type"]) == "tool_use" {
				block["input"] = objectFromJSON(str(block["arguments"]))
				delete(block, "arguments")
			}
		}
	}
	return map[string]any{"content": content}
}

func TestProtocolSignedReasoningToolCycle(t *testing.T) {
	for _, source := range []wireProtocol{messagesProtocol, responsesProtocol} {
		for _, client := range []wireProtocol{messagesProtocol, responsesProtocol, chatProtocol} {
			if source == client {
				continue
			}
			for _, stream := range []bool{false, true} {
				t.Run(string(source)+"-"+string(client)+"-"+strconv.FormatBool(stream), func(t *testing.T) {
					reply := &protocolReply{model: "m", usage: map[string]any{}}
					w := httptest.NewRecorder()
					sink := &replySink{reply: reply}
					if stream {
						sink.emitter = &protocolEmitter{w: w, protocol: client, reply: reply}
					}
					data := signedClaudeStream()
					if source == responsesProtocol {
						data = opaqueResponsesStream()
					}
					if stream {
						if err := readProtocolSSE(&tinyProtocolReader{data: data, size: 3}, source, sink); err != nil {
							t.Fatal(err)
						}
					} else {
						body := `{"id":"msg","content":[{"type":"thinking","thinking":"I should call lookup.","signature":"signed-claude-state"},{"type":"tool_use","id":"call_test","name":"lookup","input":{"city":"Vienna"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`
						if source == responsesProtocol {
							body = `{"id":"resp","status":"completed","output":[{"id":"reason_test","type":"reasoning","content":[],"summary":[{"type":"summary_text","text":"I should call lookup."}],"encrypted_content":"opaque-openai-state"},{"id":"fc_test","type":"function_call","call_id":"call_test","name":"lookup","arguments":"{\"city\":\"Vienna\"}"}],"usage":{"input_tokens":1,"output_tokens":2}}`
						}
						parsed, _ := decodeProtocolJSON([]byte(body))
						if err := consumeProtocolJSON(parsed, source, sink); err != nil {
							t.Fatal(err)
						}
					}
					clientReply := reply.encode(client)
					if stream {
						clientReply = replayFromClientSSE(t, w.Body.String(), client)
					}
					next := nextProtocolToolRequest(t, clientReply, client)
					upstream, err := adaptInferenceRequest(next, client, source)
					if err != nil {
						t.Fatal(err)
					}
					if source == messagesProtocol {
						first := object(list(object(list(upstream["messages"])[0])["content"])[0])
						if first["signature"] != "signed-claude-state" || first["thinking"] != "I should call lookup." {
							t.Fatalf("signed state changed %+v", upstream)
						}
					} else {
						first := object(list(upstream["input"])[0])
						if first["encrypted_content"] != "opaque-openai-state" || first["id"] != "reason_test" {
							t.Fatalf("opaque state changed %+v", upstream)
						}
					}
					encoded, _ := json.Marshal(upstream)
					for _, text := range []string{"call_test", "Vienna", "Sunny"} {
						if !strings.Contains(string(encoded), text) {
							t.Fatalf("tool cycle lost %s in %s", text, encoded)
						}
					}
				})
			}
		}
	}
}

func TestProtocolPortableReasoningAndRefusalReplay(t *testing.T) {
	for _, client := range []wireProtocol{messagesProtocol, responsesProtocol, chatProtocol} {
		reply := &protocolReply{id: "reply", model: "m", status: "completed", terminal: true, usage: map[string]any{}, blocks: []*replyBlock{{kind: "reasoning", id: "reason", text: "I should call lookup."}, {kind: "refusal", id: "refuse", text: "Cannot do that."}, {kind: "tool", id: "fc", callID: "call_test", name: "lookup", text: `{"city":"Vienna"}`}}}
		next := nextProtocolToolRequest(t, reply.encode(client), client)
		for _, target := range []wireProtocol{messagesProtocol, responsesProtocol} {
			out, err := adaptInferenceRequest(next, client, target)
			if err != nil {
				t.Fatalf("%s to %s: %v", client, target, err)
			}
			data, _ := json.Marshal(out)
			if !strings.Contains(string(data), "Cannot do that.") || !strings.Contains(string(data), "I should call lookup.") {
				t.Fatalf("visible content lost %s", data)
			}
		}
	}
}

func TestProtocolRejectsForeignSignedReasoning(t *testing.T) {
	state, err := encodeReasoningState("claude", map[string]any{"type": "thinking", "thinking": "think", "signature": "signed"}, "")
	if err != nil {
		t.Fatal(err)
	}
	p := map[string]any{"input": []any{map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": state}}}
	if err := unwrapNativeReasoning(p, responsesProtocol); err == nil {
		t.Fatal("Claude signed state sent to OpenAI")
	}
	if _, err := decodeReasoningState("opaque-other-provider-state"); err == nil {
		t.Fatal("foreign opaque state accepted")
	}
}

func TestProtocolResponsesStreamItemShapes(t *testing.T) {
	w := httptest.NewRecorder()
	reply := &protocolReply{usage: map[string]any{}}
	if err := readProtocolSSE(strings.NewReader(signedClaudeStream()), messagesProtocol, &replySink{reply: reply, emitter: &protocolEmitter{w: w, protocol: responsesProtocol, reply: reply}}); err != nil {
		t.Fatal(err)
	}
	added, done := 0, 0
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		body, _ := decodeProtocolJSON([]byte(strings.TrimPrefix(line, "data: ")))
		if str(body["type"]) != "response.output_item.added" && str(body["type"]) != "response.output_item.done" {
			continue
		}
		item := object(body["item"])
		field := "content"
		if str(item["type"]) == "reasoning" {
			field = "summary"
		}
		if str(item["type"]) == "function_call" {
			continue
		}
		if str(body["type"]) == "response.output_item.added" {
			added++
			if len(list(item[field])) != 0 {
				t.Fatalf("added item already has a part %+v", item)
			}
		} else {
			done++
			if len(list(item[field])) != 1 {
				t.Fatalf("done item has wrong parts %+v", item)
			}
		}
	}
	if added != 2 || done != 2 {
		t.Fatalf("item events added=%d done=%d", added, done)
	}
}

func TestProtocolChatKeepsNativeReasoningOrder(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(strconv.FormatBool(stream), func(t *testing.T) {
			body, _ := decodeProtocolJSON([]byte(`{"id":"resp","status":"completed","output":[{"id":"r1","type":"reasoning","status":"completed","summary":[],"encrypted_content":"opaque-1"},{"id":"m","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"I will look it up. Grüße aus Wien.","annotations":[]}]},{"id":"r2","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"Check weather."},{"type":"summary_text","text":"Call lookup."}],"encrypted_content":"opaque-2"},{"id":"fc","type":"function_call","call_id":"call_test","name":"lookup","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":2}}`))
			reply := &protocolReply{model: "m", usage: map[string]any{}}
			w := httptest.NewRecorder()
			sink := &replySink{reply: reply}
			if stream {
				sink.emitter = &protocolEmitter{w: w, protocol: chatProtocol, reply: reply}
			}
			if err := consumeProtocolJSON(body, responsesProtocol, sink); err != nil {
				t.Fatal(err)
			}
			clientReply := reply.encode(chatProtocol)
			if stream {
				clientReply = replayFromClientSSE(t, w.Body.String(), chatProtocol)
			}
			next := nextProtocolToolRequest(t, clientReply, chatProtocol)
			out, err := adaptInferenceRequest(next, chatProtocol, responsesProtocol)
			if err != nil {
				t.Fatal(err)
			}
			input := list(out["input"])
			want := []string{"reasoning", "message", "reasoning", "function_call", "function_call_output"}
			if len(input) != len(want) {
				t.Fatalf("native item count changed %+v", out)
			}
			for i, kind := range want {
				if str(object(input[i])["type"]) != kind {
					t.Fatalf("item %d: want %s, got %+v", i, kind, input[i])
				}
			}
			original := list(body["output"])
			if !reflect.DeepEqual(input[0], original[0]) || !reflect.DeepEqual(input[2], original[2]) {
				t.Fatalf("native reasoning state changed %+v", input)
			}
			text := str(object(list(object(input[1])["content"])[0])["text"])
			if text != "I will look it up. Grüße aus Wien." {
				t.Fatalf("visible text changed %q", text)
			}
		})
	}
}

// OpenAI clients send empty strings where Claude rejects empty text blocks.
func TestProtocolClaudeRequestOmitsEmptyText(t *testing.T) {
	p, _ := decodeProtocolJSON([]byte(`{"messages":[{"role":"system","content":""},{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"call_test","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_test","content":""}],"tools":[{"type":"function","function":{"name":"lookup"}}],"tool_choice":"none","parallel_tool_calls":false}`))
	out, err := adaptInferenceRequest(p, chatProtocol, messagesProtocol)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := out["system"]; exists {
		t.Fatalf("empty system was sent: %#v", out["system"])
	}
	messages := list(out["messages"])
	call := list(object(messages[1])["content"])
	if len(call) != 1 || object(call[0])["type"] != "tool_use" {
		t.Fatalf("assistant turn %#v", call)
	}
	result := object(list(object(messages[2])["content"])[0])
	if result["tool_use_id"] != "call_test" || len(list(result["content"])) != 0 {
		t.Fatalf("tool result %#v", result)
	}
	if choice := object(out["tool_choice"]); !reflect.DeepEqual(choice, map[string]any{"type": "none"}) {
		t.Fatalf("tool choice %#v", choice)
	}
	// Without tools there is nothing for the parallel flag to configure.
	delete(p, "tools")
	delete(p, "tool_choice")
	if out, err = adaptInferenceRequest(p, chatProtocol, messagesProtocol); err != nil || out["tool_choice"] != nil {
		t.Fatalf("tool choice without tools %#v %v", out["tool_choice"], err)
	}
	p["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}}
	p["tool_choice"] = "required"
	out, err = adaptInferenceRequest(p, chatProtocol, messagesProtocol)
	if err != nil || !reflect.DeepEqual(out["tool_choice"], map[string]any{"type": "any", "disable_parallel_tool_use": true}) {
		t.Fatalf("required tool choice %#v %v", out["tool_choice"], err)
	}
}
