package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClaudeOAuthPayload(t *testing.T) {
	identity := map[string]any{"type": "text", "text": claudeOAuthIdentity}
	for _, test := range []struct {
		name   string
		system string
		want   any
	}{
		{name: "absent", want: []any{identity}},
		{name: "null", system: " null ", want: []any{identity}},
		{name: "empty-string", system: `""`, want: []any{identity}},
		{name: "string", system: `"Keep Ω and newlines.\n "`, want: []any{identity, map[string]any{"type": "text", "text": "Keep Ω and newlines.\n "}}},
		{name: "array", system: `[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}},{"type":"text","text":"second Ω\n "}]`, want: []any{identity, map[string]any{"type": "text", "text": "first", "cache_control": map[string]any{"type": "ephemeral"}}, map[string]any{"type": "text", "text": "second Ω\n "}}},
		{name: "native-array", system: `[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"caller"}]`, want: []any{map[string]any{"type": "text", "text": claudeOAuthIdentity, "cache_control": map[string]any{"type": "ephemeral"}}, map[string]any{"type": "text", "text": "caller"}}},
		{name: "native-string", system: `"You are Claude Code, Anthropic's official CLI for Claude."`, want: claudeOAuthIdentity},
		{name: "empty-array", system: `[]`, want: []any{identity}},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := map[string]json.RawMessage{
				"model":      json.RawMessage(`"claude-haiku-test"`),
				"stream":     json.RawMessage(`true`),
				"max_tokens": json.RawMessage(`256`),
				"messages":   json.RawMessage(`[{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"lookup","input":{"city":"Vienna"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call","content":"Sunny Ω"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]`),
				"tools":      json.RawMessage(`[{"name":"lookup","strict":true,"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]`),
			}
			if test.system != "" {
				payload["system"] = json.RawMessage(test.system)
			}
			original, _ := json.Marshal(payload)
			prepared, err := claudeOAuthPayload(payload, storedAccount{Provider: "claude", AuthMode: "oauth"})
			if err != nil {
				t.Fatal(err)
			}
			var system any
			if err := json.Unmarshal(prepared["system"], &system); err != nil || !reflect.DeepEqual(system, test.want) {
				t.Fatalf("system = %#v, want %#v; error %v", system, test.want, err)
			}
			for name, value := range payload {
				if name != "system" && !reflect.DeepEqual(prepared[name], value) {
					t.Fatalf("changed caller field %s", name)
				}
			}
			after, _ := json.Marshal(payload)
			if string(original) != string(after) {
				t.Fatal("changed the input map")
			}
			second, err := claudeOAuthPayload(prepared, storedAccount{Provider: "claude", AuthMode: "oauth"})
			if err != nil || !reflect.DeepEqual(second, prepared) {
				t.Fatal("added the identity twice")
			}
		})
	}
	for _, system := range []string{`{"private_caller_content":"secret system text"}`, `true`, `42`} {
		if _, err := claudeOAuthPayload(map[string]json.RawMessage{"system": json.RawMessage(system)}, storedAccount{Provider: "claude", AuthMode: "oauth"}); err == nil {
			t.Fatalf("accepted invalid system %s", system)
		} else if strings.Contains(err.Error(), "private_caller_content") || strings.Contains(err.Error(), "secret system text") {
			t.Fatal("included caller content in a preparation error")
		}
	}
}

func TestClaudeOAuthPayloadLeavesOtherAccountsUnchanged(t *testing.T) {
	payload := map[string]json.RawMessage{"system": json.RawMessage(`"caller"`), "messages": json.RawMessage(`[{"role":"user","content":"hi"}]`)}
	want, _ := json.Marshal(payload)
	for _, account := range []storedAccount{{Provider: "claude", AuthMode: "api_key"}, {Provider: "codex", AuthMode: "codex"}, {Provider: "codex", AuthMode: "api_key"}} {
		prepared, err := claudeOAuthPayload(payload, account)
		got, _ := json.Marshal(prepared)
		if err != nil || string(got) != string(want) {
			t.Fatalf("changed %s/%s payload: %s, %v", account.Provider, account.AuthMode, got, err)
		}
	}
}

func TestClaudeOAuthInferenceAllProtocols(t *testing.T) {
	for _, mode := range []string{"oauth", "api_key"} {
		for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
			for _, stream := range []bool{false, true} {
				t.Run(mode+path+"/stream="+strconv.FormatBool(stream), func(t *testing.T) {
					s := testServer(t, Config{ExternalAuth: true})
					addProtocolAccount(t, s, "account", "claude", mode)
					_, secret := newTestKey(t, s, `{}`)
					sent := 0
					transport := roundTripper(func(r *http.Request) (*http.Response, error) {
						switch r.URL.Path {
						case "/v1/models":
							return jsonResponse(`{"data":[{"id":"claude-haiku-test"}]}`, 200), nil
						case "/api/oauth/profile", "/api/oauth/usage":
							return jsonResponse(`{}`, 200), nil
						case "/v1/messages":
							sent++
							var payload map[string]any
							if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
								t.Fatal(err)
							}
							if payload["stream"] != stream || payload["max_tokens"] != float64(256) {
								t.Fatalf("changed stream or cap: %#v", payload)
							}
							if mode == "oauth" {
								want := []any{map[string]any{"type": "text", "text": claudeOAuthIdentity}, map[string]any{"type": "text", "text": "Caller system Ω."}}
								if !reflect.DeepEqual(payload["system"], want) {
									t.Fatalf("system = %#v, want %#v", payload["system"], want)
								}
								if r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
									t.Fatalf("changed OAuth beta: %s", r.Header.Get("Anthropic-Beta"))
								}
							} else if raw, _ := json.Marshal(payload["system"]); strings.Contains(string(raw), claudeOAuthIdentity) {
								t.Fatal("added CLI identity to a paid request")
							}
							if r.Header.Get("User-Agent") != "vrouter/0.2" || r.Header.Get("X-App") != "" || r.Header.Get("Anthropic-Dangerous-Direct-Browser-Access") != "" {
								t.Fatalf("changed provider header profile: %#v", r.Header)
							}
							if stream {
								return sseResponse(claudeToolStream("tool_use")), nil
							}
							return jsonResponse(`{"id":"msg","type":"message","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`, 200), nil
						}
						return jsonResponse(`{}`, 404), nil
					})
					s.client.Transport, s.streamClient.Transport = transport, transport
					payload := map[string]any{"model": "claude-haiku-test", "stream": stream}
					switch path {
					case "/v1/messages":
						payload["system"], payload["max_tokens"] = "Caller system Ω.", 256
						payload["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
					case "/v1/responses":
						payload["instructions"], payload["max_output_tokens"], payload["input"] = "Caller system Ω.", 256, "hi"
					case "/v1/chat/completions":
						payload["max_completion_tokens"] = 256
						payload["messages"] = []any{map[string]any{"role": "system", "content": "Caller system Ω."}, map[string]any{"role": "user", "content": "hi"}}
					}
					body, _ := json.Marshal(payload)
					w := call(t, s, "POST", path, string(body), secret)
					if w.Code != 200 || sent != 1 || !strings.Contains(w.Body.String(), "hello") {
						t.Fatalf("status=%d sent=%d reply=%s", w.Code, sent, w.Body.String())
					}
				})
			}
		}
	}
}

func TestClaudeOAuthWindowTrigger(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	addClaude(t, s, "account")
	reset := time.Now().UTC().Add(5 * time.Hour).Truncate(time.Second)
	sent := 0
	transport := roundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/oauth/profile":
			return jsonResponse(`{}`, 200), nil
		case "/api/oauth/usage":
			var boundary any
			if sent > 0 {
				boundary = reset.Format(time.RFC3339)
			}
			data, _ := json.Marshal(map[string]any{"five_hour": map[string]any{"utilization": 0, "resets_at": boundary}, "seven_day": map[string]any{"utilization": 0}})
			return jsonResponse(string(data), 200), nil
		case "/v1/models":
			return jsonResponse(`{"data":[{"id":"claude-haiku-test"}]}`, 200), nil
		case "/v1/messages":
			sent++
			raw, _ := io.ReadAll(r.Body)
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			want := []any{map[string]any{"type": "text", "text": claudeOAuthIdentity}}
			if !reflect.DeepEqual(payload["system"], want) {
				t.Fatalf("trigger system = %#v, want %#v", payload["system"], want)
			}
			delete(payload, "system")
			original, _ := json.Marshal(windowStartPayload(Model{ID: "claude-haiku-test"}, "claude"))
			var expected map[string]any
			_ = json.Unmarshal(original, &expected)
			if !reflect.DeepEqual(payload, expected) {
				t.Fatalf("changed trigger prompt or controls: %#v, want %#v", payload, expected)
			}
			return jsonResponse(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`, 200), nil
		}
		return jsonResponse(`{}`, 404), nil
	})
	s.client.Transport, s.streamClient.Transport = transport, transport
	next, err := s.ensureWindow(context.Background(), s.store.snapshot().Accounts[0])
	if err != nil || sent != 1 || next == nil || !next.Equal(reset) {
		t.Fatalf("sent=%d next=%v error=%v", sent, next, err)
	}
}
