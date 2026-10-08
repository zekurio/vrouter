package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const compatibilityMessages = `{"model":"model","max_tokens":8192,"temperature":0.2,"top_p":0.9,"metadata":{"user_id":"session-test"},"service_tier":"auto","cache_control":{"type":"ephemeral"},"system":[{"type":"text","text":"Caller rules Ω.","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"Find weather.","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"lookup","type":"custom","description":"Find weather","strict":false,"defer_loading":false,"allowed_callers":["direct"],"input_examples":[],"cache_control":{"type":"ephemeral"},"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":false},"thinking":{"type":"enabled","budget_tokens":2048,"display":"summarized"},"output_config":{},"stop_sequences":[]}`

func compatibilityRequest(t *testing.T, path string) map[string]any {
	t.Helper()
	body := compatibilityMessages
	if path == "/v1/responses" {
		body = `{"model":"model","input":"Find weather.","instructions":"Caller rules Ω.","max_output_tokens":8192,"temperature":0.2,"top_p":0.9,"include":["reasoning.encrypted_content"],"reasoning":{"effort":"max","summary":"auto"},"tools":[{"type":"function","name":"lookup","description":"Find weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	} else if path == "/v1/chat/completions" {
		body = `{"model":"model","messages":[{"role":"system","content":"Caller rules Ω."},{"role":"user","content":"Find weather."}],"max_completion_tokens":8192,"temperature":0.2,"top_p":0.9,"reasoning_effort":"ultra","stop":[],"tools":[{"type":"function","function":{"name":"lookup","description":"Find weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	}
	payload, err := decodeProtocolJSON([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func compatibilityServer(t *testing.T, provider, mode string) *server {
	t.Helper()
	s := testServer(t, Config{ExternalAuth: true})
	addProtocolAccount(t, s, "account", provider, mode)
	s.client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			if mode == "codex" {
				return jsonResponse(`{"models":[{"slug":"model","visibility":"list"}]}`, 200), nil
			}
			return jsonResponse(`{"data":[{"id":"model"}]}`, 200), nil
		}
		if strings.HasSuffix(r.URL.Path, "/usage") || strings.HasSuffix(r.URL.Path, "/profile") {
			return jsonResponse(`{}`, 200), nil
		}
		t.Errorf("unexpected provider request %s", r.URL.Path)
		return jsonResponse(`{}`, 404), nil
	})
	return s
}

func prepareCompatibility(t *testing.T, s *server, path string, payload map[string]any) (*preparedInference, int, string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return s.prepareInference(httptest.NewRecorder(), httptest.NewRequest("POST", path, strings.NewReader(string(body))), &inferenceAttempt{})
}

func TestCompatibilityCodexClientControls(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+"/stream="+strconv.FormatBool(stream), func(t *testing.T) {
				s := compatibilityServer(t, "codex", "codex")
				_, secret := newTestKey(t, s, `{}`)
				payload := compatibilityRequest(t, path)
				payload["stream"] = stream
				var sent map[string]any
				s.streamClient.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path != "/backend-api/codex/responses" {
						t.Fatalf("wrong provider path %s", r.URL.Path)
					}
					if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
						t.Fatal(err)
					}
					return sseResponse(opaqueResponsesStream()), nil
				})
				body, _ := json.Marshal(payload)
				w := call(t, s, "POST", path, string(body), secret)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "lookup") {
					t.Fatalf("status=%d reply=%s", w.Code, w.Body.String())
				}
				if sent["max_output_tokens"] != float64(8192) || sent["stream"] != true || sent["store"] != false {
					t.Fatalf("lost cap or Codex stream contract: %#v", sent)
				}
				for _, field := range []string{"temperature", "top_p", "metadata", "cache_control"} {
					if _, exists := sent[field]; exists {
						t.Fatalf("forwarded unsupported %s", field)
					}
				}
				if !reflect.DeepEqual(sent["include"], []any{"reasoning.encrypted_content"}) || sent["instructions"] != "Caller rules Ω." {
					t.Fatalf("lost stateless reasoning hint or system text: %#v", sent)
				}
				wantEffort := "max"
				if path == "/v1/messages" {
					wantEffort = "medium"
				} else if path == "/v1/chat/completions" {
					wantEffort = "ultra"
				}
				if object(sent["reasoning"])["effort"] != wantEffort || object(list(sent["tools"])[0])["name"] != "lookup" {
					t.Fatalf("lost reasoning or tool definition: %#v", sent)
				}
				ignored := "temperature, top_p"
				if path == "/v1/messages" {
					ignored += ", metadata.user_id"
				}
				if w.Header().Get("X-Vrouter-Ignored-Parameters") != ignored {
					t.Fatalf("ignored parameters = %q, want %q", w.Header().Get("X-Vrouter-Ignored-Parameters"), ignored)
				}
				rows := s.manager.telemetry(defaultGatewayID).Requests
				if len(rows) != 1 || !rows[0].UsageKnown || rows[0].TotalTokens != 14 || rows[0].Provider != "codex" {
					t.Fatalf("changed raw accounting: %#v", rows)
				}
			})
		}
	}
}

func TestCompatibilityPreservesOtherProviderControls(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		s := compatibilityServer(t, provider, "api_key")
		for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
			payload := compatibilityRequest(t, path)
			if provider == "claude" && path == "/v1/chat/completions" {
				payload["reasoning_effort"] = "max"
			}
			prepared, status, message := prepareCompatibility(t, s, path, payload)
			if status != 0 {
				t.Fatalf("%s %s status=%d error=%s", provider, path, status, message)
			}
			var upstream map[string]any
			raw, _ := json.Marshal(prepared.payload)
			_ = json.Unmarshal(raw, &upstream)
			cap := "max_output_tokens"
			if provider == "claude" {
				cap = "max_tokens"
			}
			if upstream[cap] != float64(8192) || upstream["temperature"] != 0.2 || upstream["top_p"] != 0.9 {
				t.Fatalf("changed %s controls: %#v", provider, upstream)
			}
			wantIgnored := []string(nil)
			if provider == "codex" && path == "/v1/messages" {
				wantIgnored = []string{"metadata.user_id"}
			}
			if !reflect.DeepEqual(prepared.ignoredParameters, wantIgnored) {
				t.Fatalf("ignored %s controls: %v", provider, prepared.ignoredParameters)
			}
			if provider == "claude" && path == "/v1/messages" {
				original, _ := json.Marshal(payload)
				var expected map[string]any
				_ = json.Unmarshal(original, &expected)
				if !reflect.DeepEqual(upstream, expected) {
					t.Fatalf("changed native Claude metadata, cache, or thinking: %#v", upstream)
				}
			}
		}
	}
}

func TestCompatibilityCodexRejectsMalformedControls(t *testing.T) {
	s := compatibilityServer(t, "codex", "codex")
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
		cap := "max_output_tokens"
		if path == "/v1/messages" {
			cap = "max_tokens"
		} else if path == "/v1/chat/completions" {
			cap = "max_completion_tokens"
		}
		for _, invalid := range []struct {
			field, value string
		}{{cap, `0`}, {cap, `-1`}, {cap, `1.5`}, {cap, `"256"`}, {cap, `1e999`}, {"temperature", `"1"`}, {"temperature", `-0.1`}, {"temperature", `3`}, {"top_p", `2`}, {"top_p", `false`}} {
			payload := compatibilityRequest(t, path)
			delete(payload, "thinking")
			value, _ := decodeProtocolJSON([]byte(`{"value":` + invalid.value + `}`))
			payload[invalid.field] = value["value"]
			if _, status, _ := prepareCompatibility(t, s, path, payload); status != 400 {
				t.Fatalf("%s accepted %s:%s status=%d", path, invalid.field, invalid.value, status)
			}
		}
	}
}

func TestCompatibilityThinkingSettings(t *testing.T) {
	for _, test := range []struct {
		settings string
		effort   string
	}{
		{`"thinking":{"type":"adaptive"},"output_config":{}`, "high"},
		{`"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":null}`, "high"},
		{`"thinking":{"type":"disabled"},"output_config":{}`, "none"},
		{`"thinking":{"type":"enabled","budget_tokens":1024}`, "low"},
		{`"thinking":{"type":"enabled","budget_tokens":1025}`, "medium"},
		{`"thinking":{"type":"enabled","budget_tokens":8192}`, "medium"},
		{`"thinking":{"type":"enabled","budget_tokens":8193}`, "high"},
		{`"thinking":{"type":"enabled","budget_tokens":24576}`, "high"},
		{`"thinking":{"type":"enabled","budget_tokens":24577}`, "xhigh"},
		{`"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"max"}`, "max"},
		{`"thinking":{"type":"adaptive"},"output_config":{"effort":"ultra"}`, "ultra"},
	} {
		payload, _ := decodeProtocolJSON([]byte(`{"max_tokens":65536,"messages":[{"role":"user","content":"question"}],` + test.settings + `}`))
		translated, err := adaptInferenceRequest(payload, messagesProtocol, responsesProtocol)
		if err != nil || object(translated["reasoning"])["effort"] != test.effort {
			t.Fatalf("settings=%s effort=%#v error=%v", test.settings, translated["reasoning"], err)
		}
	}
	for _, settings := range []string{
		`"thinking":{"type":"enabled","budget_tokens":1023}`,
		`"thinking":{"type":"enabled","budget_tokens":2048.5}`,
		`"thinking":{"type":"enabled","budget_tokens":"2048"}`,
		`"thinking":{"type":"enabled","budget_tokens":65536}`,
		`"thinking":{"type":"adaptive","budget_tokens":2048}`,
		`"thinking":{"type":"disabled","budget_tokens":2048}`,
		`"thinking":{"type":"adaptive","display":"omitted"}`,
		`"thinking":{"type":"disabled"},"output_config":{"effort":"max"}`,
		`"output_config":{"effort":42}`,
	} {
		payload, _ := decodeProtocolJSON([]byte(`{"max_tokens":65536,"messages":[{"role":"user","content":"question"}],` + settings + `}`))
		if _, err := adaptInferenceRequest(payload, messagesProtocol, responsesProtocol); err == nil {
			t.Fatalf("accepted invalid or unrepresentable settings %s", settings)
		}
	}
	for _, effort := range []string{"max", "ultra"} {
		payload, _ := decodeProtocolJSON([]byte(`{"input":"question","reasoning":{"effort":"` + effort + `"}}`))
		translated, err := adaptInferenceRequest(payload, responsesProtocol, messagesProtocol)
		if effort == "ultra" {
			if err == nil {
				t.Fatal("silently reduced ultra effort for Claude")
			}
		} else if err != nil || object(translated["output_config"])["effort"] != "max" {
			t.Fatalf("changed max effort: %#v %v", translated, err)
		}
	}
}

func TestCompatibilityRejectsUnsafeDefaults(t *testing.T) {
	for _, field := range []string{
		`"cache_control":{"type":"persistent"}`,
		`"cache_control":{"type":"ephemeral","ttl":"forever"}`,
		`"metadata":{"user_id":42}`,
		`"metadata":{"unknown":null}`,
		`"service_tier":"standard_only"`,
		`"stop_sequences":["STOP"]`,
	} {
		payload, _ := decodeProtocolJSON([]byte(`{"messages":[{"role":"user","content":"question"}],` + field + `}`))
		if _, err := adaptInferenceRequest(payload, messagesProtocol, responsesProtocol); err == nil {
			t.Fatalf("accepted unsupported setting %s", field)
		}
	}
	for _, field := range []string{
		`"type":"bash_20250124"`,
		`"defer_loading":true`,
		`"allowed_callers":["code_execution_20250825"]`,
		`"input_examples":[{}]`,
		`"cache_control":{"type":"ephemeral","ttl":"forever"}`,
	} {
		payload, _ := decodeProtocolJSON([]byte(`{"messages":[{"role":"user","content":"question"}],"tools":[{"name":"lookup","input_schema":{"type":"object"},` + field + `}]}`))
		if _, err := adaptInferenceRequest(payload, messagesProtocol, responsesProtocol); err == nil {
			t.Fatalf("accepted unsupported tool setting %s", field)
		}
	}
}

func TestCompatibilityDirectToolCallers(t *testing.T) {
	for _, caller := range []string{`{"type":"direct"}`, `{"type":"code_execution_20250825","tool_id":"server-tool"}`} {
		for _, source := range []wireProtocol{messagesProtocol, responsesProtocol} {
			body := `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_test","name":"lookup","input":{},"caller":` + caller + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_test","content":"result"}]}]}`
			target := responsesProtocol
			if source == responsesProtocol {
				target = messagesProtocol
				body = `{"input":[{"type":"function_call","call_id":"call_test","name":"lookup","arguments":"{}","caller":` + caller + `},{"type":"function_call_output","call_id":"call_test","output":"result","caller":` + caller + `}]}`
			}
			payload, _ := decodeProtocolJSON([]byte(body))
			translated, err := adaptInferenceRequest(payload, source, target)
			if strings.Contains(caller, "code_execution") {
				if err == nil {
					t.Fatal("lost server tool execution state")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(translated)
				if strings.Count(string(encoded), "call_test") != 2 || !strings.Contains(string(encoded), "result") {
					t.Fatalf("changed direct tool call/result: %s", encoded)
				}
			}
		}
	}
}

func TestCompatibilityFailedToolResultCycle(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, content := range []string{`[]`, `[{"type":"text","text":"Network unavailable Ω."},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]`} {
			reply := &protocolReply{model: "m", usage: map[string]any{}}
			w := httptest.NewRecorder()
			sink := &replySink{reply: reply}
			if stream {
				sink.emitter = &protocolEmitter{w: w, protocol: messagesProtocol, reply: reply}
			}
			if err := readProtocolSSE(&tinyProtocolReader{data: opaqueResponsesStream(), size: 3}, responsesProtocol, sink); err != nil {
				t.Fatal(err)
			}
			clientReply := reply.encode(messagesProtocol)
			if stream {
				clientReply = replayFromClientSSE(t, w.Body.String(), messagesProtocol)
			}
			next := nextProtocolToolRequest(t, clientReply, messagesProtocol)
			results := list(object(list(next["messages"])[1])["content"])
			result := object(results[0])
			parsed, _ := decodeProtocolJSON([]byte(`{"content":` + content + `}`))
			result["content"], result["is_error"] = parsed["content"], true
			upstream, err := adaptInferenceRequest(next, messagesProtocol, responsesProtocol)
			if err != nil {
				t.Fatal(err)
			}
			input := list(upstream["input"])
			if object(input[0])["encrypted_content"] != "opaque-openai-state" {
				t.Fatalf("lost signed reasoning after tool failure: %#v", input)
			}
			output := object(input[len(input)-1])
			if output["call_id"] != "call_test" || str(output["type"]) != "function_call_output" {
				t.Fatalf("lost error result call ID: %#v", output)
			}
			parts := list(output["output"])
			if str(object(parts[0])["text"]) != "Tool execution failed." {
				t.Fatalf("lost error semantics: %#v", parts)
			}
			if content != `[]` && (len(parts) != 3 || object(parts[1])["text"] != "Network unavailable Ω." || object(parts[2])["image_url"] != "data:image/png;base64,aGVsbG8=") {
				t.Fatalf("changed failed tool result text/image: %#v", parts)
			}
			blocks, err := parseBlocks(results, messagesProtocol)
			if err != nil || object(encodeBlocks(blocks, messagesProtocol, "user")[0])["is_error"] != true {
				t.Fatal("did not preserve native Messages tool error flag")
			}
		}
	}
}

func TestCompatibilityNativeReasoningContentReplay(t *testing.T) {
	for _, content := range []string{`[]`, `[{"type":"reasoning_text","text":"Native content Ω.\n "}]`} {
		item, _ := decodeProtocolJSON([]byte(`{"id":"reason_live","type":"reasoning","summary":[],"content":` + content + `,"encrypted_content":"opaque-live-state"}`))
		tool, _ := decodeProtocolJSON([]byte(`{"id":"fc_live","type":"function_call","call_id":"call_test","name":"lookup","arguments":"{\"city\":\"Vienna\"}"}`))
		response := map[string]any{"id": "resp_live", "status": "completed", "output": []any{item, tool}, "usage": map[string]any{"input_tokens": 9, "output_tokens": 5, "total_tokens": 14}}
		for _, client := range []wireProtocol{messagesProtocol, chatProtocol} {
			for _, stream := range []bool{false, true} {
				t.Run(string(client)+"/"+strconv.FormatBool(stream)+"/"+content, func(t *testing.T) {
					reply := &protocolReply{model: "m", usage: map[string]any{}}
					w := httptest.NewRecorder()
					sink := &replySink{reply: reply}
					if stream {
						sink.emitter = &protocolEmitter{w: w, protocol: client, reply: reply}
						rawItem, _ := json.Marshal(item)
						rawTool, _ := json.Marshal(tool)
						rawResponse, _ := json.Marshal(response)
						data := frame("response.created", `{"type":"response.created","response":{"id":"resp_live","status":"in_progress"}}`) +
							frame("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"reason_live","type":"reasoning","summary":[]}}`) +
							frame("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":`+string(rawItem)+`}`) +
							frame("response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":`+string(rawTool)+`}`) +
							frame("response.completed", `{"type":"response.completed","response":`+string(rawResponse)+`}`)
						if err := readProtocolSSE(&tinyProtocolReader{data: data, size: 3}, responsesProtocol, sink); err != nil {
							t.Fatal(err)
						}
					} else if err := consumeProtocolJSON(response, responsesProtocol, sink); err != nil {
						t.Fatal(err)
					}
					clientReply := reply.encode(client)
					if stream {
						clientReply = replayFromClientSSE(t, w.Body.String(), client)
					}
					next := nextProtocolToolRequest(t, clientReply, client)
					upstream, err := adaptInferenceRequest(next, client, responsesProtocol)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(object(list(upstream["input"])[0]), item) {
						t.Fatalf("changed native signed reasoning: %#v", upstream["input"])
					}
					if !reflect.DeepEqual(upstream["include"], []any{"reasoning.encrypted_content"}) {
						t.Fatal("did not request stateless reasoning state on the next turn")
					}
				})
			}
		}
		state, err := encodeReasoningState("codex", item)
		if err != nil {
			t.Fatal(err)
		}
		block, err := parseResponseReasoning(map[string]any{"type": "reasoning", "summary": []any{}, "content": []any{}, "encrypted_content": state})
		if err != nil || !reflect.DeepEqual(block.native, item) {
			t.Fatalf("changed native state in Responses wrapper: %#v %v", block.native, err)
		}
	}
	for _, content := range []string{`{}`, `["text"]`, `[{"type":"input_text","text":"wrong type"}]`, `[{"type":"reasoning_text","text":42}]`} {
		item, _ := decodeProtocolJSON([]byte(`{"type":"reasoning","summary":[],"content":` + content + `}`))
		if _, err := parseResponseReasoning(item); err == nil {
			t.Fatalf("accepted invalid reasoning content %s", content)
		}
	}
}

func TestCompatibilityEncryptedReasoningHint(t *testing.T) {
	payload, _ := decodeProtocolJSON([]byte(`{"model":"m","input":"question","include":["reasoning.encrypted_content"],"reasoning":{"effort":"high","summary":"auto"},"max_output_tokens":8192}`))
	translated, err := adaptInferenceRequest(payload, responsesProtocol, messagesProtocol)
	if err != nil {
		t.Fatal(err)
	}
	if object(translated["output_config"])["effort"] != "high" || translated["max_tokens"] != json.Number("8192") {
		t.Fatalf("lost reasoning or cap: %#v", translated)
	}
	for _, invalid := range []string{`"reasoning.encrypted_content"`, `["web_search_call.action.sources"]`, `["reasoning.encrypted_content","message.output_text.logprobs"]`, `[42]`} {
		payload, _ := decodeProtocolJSON([]byte(`{"input":"question","include":` + invalid + `}`))
		if _, err := adaptInferenceRequest(payload, responsesProtocol, messagesProtocol); err == nil {
			t.Fatalf("accepted unsupported include %s", invalid)
		}
	}
}

func TestCompatibilityEncryptedReasoningHintToolCycle(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			reply := &protocolReply{model: "m", usage: map[string]any{}}
			w := httptest.NewRecorder()
			sink := &replySink{reply: reply}
			if stream {
				sink.emitter = &protocolEmitter{w: w, protocol: responsesProtocol, reply: reply}
				if err := readProtocolSSE(&tinyProtocolReader{data: signedClaudeStream(), size: 3}, messagesProtocol, sink); err != nil {
					t.Fatal(err)
				}
			} else {
				payload, _ := decodeProtocolJSON([]byte(`{"id":"msg","content":[{"type":"thinking","thinking":"I should call lookup.","signature":"signed-claude-state"},{"type":"tool_use","id":"call_test","name":"lookup","input":{"city":"Vienna"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`))
				if err := consumeProtocolJSON(payload, messagesProtocol, sink); err != nil {
					t.Fatal(err)
				}
			}
			clientReply := reply.encode(responsesProtocol)
			if stream {
				clientReply = replayFromClientSSE(t, w.Body.String(), responsesProtocol)
			}
			next := nextProtocolToolRequest(t, clientReply, responsesProtocol)
			next["include"] = []any{"reasoning.encrypted_content"}
			upstream, err := adaptInferenceRequest(next, responsesProtocol, messagesProtocol)
			if err != nil {
				t.Fatal(err)
			}
			thinking := object(list(object(list(upstream["messages"])[0])["content"])[0])
			want := map[string]any{"type": "thinking", "thinking": "I should call lookup.", "signature": "signed-claude-state"}
			if !reflect.DeepEqual(thinking, want) {
				t.Fatalf("changed native signed state: %#v", thinking)
			}
			encoded, _ := json.Marshal(upstream)
			for _, text := range []string{"call_test", "Vienna", "Sunny"} {
				if !strings.Contains(string(encoded), text) {
					t.Fatalf("lost %s in %s", text, encoded)
				}
			}
		})
	}
}
