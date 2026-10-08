package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
)

type wireProtocol string

const (
	responsesProtocol wireProtocol = "responses"
	messagesProtocol  wireProtocol = "messages"
	chatProtocol      wireProtocol = "chat"
)

func inferenceProtocol(path string) wireProtocol {
	switch path {
	case "/v1/responses":
		return responsesProtocol
	case "/v1/messages":
		return messagesProtocol
	case "/v1/chat/completions":
		return chatProtocol
	}
	return ""
}

func protocolError(message string) map[string]any {
	return map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "unsupported_protocol_feature", "message": message}}
}

type preparedInference struct {
	client, upstream       wireProtocol
	provider, native       string
	stream, upstreamStream bool
	includeUsage           bool
	payload                map[string]json.RawMessage
	accounts               []storedAccount
}

// Select the model before quota admission. The endpoint only selects the
// client protocol. It does not select the provider or its quota.
func (s *server) prepareInference(w http.ResponseWriter, r *http.Request, attempt *inferenceAttempt) (*preparedInference, int, string) {
	var payload map[string]any
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	d.UseNumber()
	if d.Decode(&payload) != nil || payload == nil || d.Decode(&struct{}{}) != io.EOF {
		return nil, 400, "Expected a JSON inference request, at most 16 MiB"
	}
	model, ok := payload["model"].(string)
	if !ok || model == "" {
		return nil, 400, "Choose a model"
	}
	stream := false
	if value, exists := payload["stream"]; exists {
		stream, ok = value.(bool)
		if !ok {
			return nil, 400, "stream must be a boolean"
		}
	}
	attempt.model, attempt.stream = model, stream
	state := s.store.snapshot()
	byProvider := map[string][]storedAccount{}
	natives := map[string]string{}
	failed := false
	for _, a := range state.Accounts {
		if a.Disabled || !routableAuth(a) {
			continue
		}
		channel := a.Provider
		if channel != "claude" && channel != "codex" {
			continue
		}
		native := model
		for _, alias := range state.Policy.Aliases[channel] {
			if alias.Alias == model {
				native = alias.Name
				break
			}
		}
		if blocked, _ := excludedModel(state.Policy, channel, native); blocked {
			continue
		}
		hidden := false
		for _, alias := range state.Policy.Aliases[channel] {
			if alias.Name == native && alias.Alias != model {
				hidden = true
			}
		}
		if hidden {
			continue
		}
		catalog, err := s.accountModels(r.Context(), a)
		if err != nil {
			failed = true
			continue
		}
		for _, m := range catalog {
			if m.ID == native {
				byProvider[channel] = append(byProvider[channel], a)
				natives[channel] = native
				break
			}
		}
	}
	if len(byProvider) > 1 {
		return nil, 400, "This model ID is advertised by more than one provider. Set a unique public alias and use that alias."
	}
	if len(byProvider) == 0 {
		if failed {
			return nil, 502, "Provider catalogs unavailable; cannot select an account"
		}
		return nil, 404, "No enabled account advertises this public model. Check disabled models and public aliases."
	}
	p := &preparedInference{client: inferenceProtocol(r.URL.Path), stream: stream, upstreamStream: stream}
	p.includeUsage = object(payload["stream_options"])["include_usage"] == true
	for channel, accounts := range byProvider {
		p.provider, p.native = channel, natives[channel]
		// Keep each pool within one billing mode.
		mode := accounts[0].AuthMode
		for _, a := range accounts {
			if a.AuthMode == mode {
				p.accounts = append(p.accounts, a)
			}
		}
	}
	p.upstream = responsesProtocol
	if p.provider == "claude" {
		p.upstream = messagesProtocol
	}
	attempt.native = p.native
	if p.client != p.upstream {
		var err error
		payload, err = adaptInferenceRequest(payload, p.client, p.upstream)
		if err != nil {
			return nil, 400, err.Error()
		}
	} else if err := unwrapNativeReasoning(payload, p.upstream); err != nil {
		return nil, 400, err.Error()
	}
	payload["model"] = p.native
	if p.accounts[0].AuthMode == "codex" {
		if value, exists := payload["store"]; exists && value != false {
			return nil, 400, "Codex does not support stored responses. Set store to false."
		}
		for _, field := range []string{"max_output_tokens", "temperature", "top_p"} {
			if payload[field] != nil {
				return nil, 400, "Codex subscription access does not support " + field + ". Omit this field."
			}
		}
		payload["store"], payload["stream"] = false, true
		p.upstreamStream = true
		if _, ok := payload["instructions"]; !ok {
			payload["instructions"] = ""
		}
	}
	if p.upstream == responsesProtocol {
		if text, ok := payload["input"].(string); ok {
			payload["input"] = []any{map[string]any{"role": "user", "content": text}}
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 400, "Invalid inference request"
	}
	if err := json.Unmarshal(raw, &p.payload); err != nil {
		return nil, 400, "Invalid inference request"
	}
	return p, 0, ""
}

type protocolBlock struct {
	kind, text, image, detail, id, name, arguments string
	result                                         []protocolBlock
	nativeProvider                                 string
	native                                         map[string]any
	projectedID                                    string
	order                                          []any
}
type protocolMessage struct {
	role   string
	blocks []protocolBlock
}
type protocolRequest struct {
	messages                     []protocolMessage
	system                       []protocolBlock
	tools                        []any
	choice                       any
	maxTokens, temperature, topP any
	effort                       string
	parallel                     *bool
	stop                         any
}

func unsupported(field string) error {
	return fmt.Errorf("Cannot translate %s to the selected provider protocol", field)
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any            { a, _ := v.([]any); return a }
func str(v any) string            { s, _ := v.(string); return s }
func fields(m map[string]any, allowed string) error {
	set := map[string]bool{}
	for _, k := range strings.Fields(allowed) {
		set[k] = true
	}
	for k, v := range m {
		if !set[k] && v != nil {
			return unsupported(k)
		}
	}
	return nil
}

func adaptInferenceRequest(payload map[string]any, source, target wireProtocol) (map[string]any, error) {
	r, err := parseProtocolRequest(payload, source)
	if err != nil {
		return nil, err
	}
	for _, m := range r.messages {
		for _, b := range m.blocks {
			if b.kind == "reasoning" {
				if err := reasoningTarget(b, target); err != nil {
					return nil, err
				}
			}
		}
	}
	out := map[string]any{"model": payload["model"], "stream": payload["stream"] == true}
	if r.temperature != nil {
		out["temperature"] = r.temperature
	}
	if r.topP != nil {
		out["top_p"] = r.topP
	}
	if target == messagesProtocol {
		max := r.maxTokens
		if max == nil {
			max = 4096
		}
		out["max_tokens"] = max
		if len(r.system) > 0 {
			out["system"] = encodeBlocks(r.system, target, "system")
		}
		messages := []any{}
		for _, m := range r.messages {
			messages = append(messages, map[string]any{"role": m.role, "content": encodeBlocks(m.blocks, target, m.role)})
		}
		out["messages"] = messages
		if r.stop != nil {
			out["stop_sequences"] = r.stop
		}
		if r.effort != "" {
			if r.effort == "none" {
				out["thinking"] = map[string]any{"type": "disabled"}
			} else {
				out["thinking"] = map[string]any{"type": "adaptive"}
				out["output_config"] = map[string]any{"effort": r.effort}
			}
		}
	} else {
		out["store"] = false
		if r.stop != nil {
			return nil, unsupported("stop sequences")
		}
		if r.maxTokens != nil {
			out["max_output_tokens"] = r.maxTokens
		}
		if len(r.system) > 0 {
			parts := []string{}
			for _, b := range r.system {
				if b.kind != "text" {
					return nil, unsupported("system images")
				}
				parts = append(parts, b.text)
			}
			out["instructions"] = strings.Join(parts, "\n\n")
		}
		input := []any{}
		reasoningSeen := map[string]bool{}
		for _, m := range r.messages {
			content := []any{}
			flush := func() {
				if len(content) > 0 {
					input = append(input, map[string]any{"type": "message", "role": m.role, "content": content})
					content = nil
				}
			}
			for _, b := range m.blocks {
				switch b.kind {
				case "reasoning":
					if b.native != nil {
						flush()
						id := str(b.native["id"])
						if id == "" || !reasoningSeen[id] {
							input = append(input, b.native)
							reasoningSeen[id] = true
						}
					} else {
						content = append(content, map[string]any{"type": "output_text", "text": b.text})
					}
				case "tool":
					flush()
					input = append(input, map[string]any{"type": "function_call", "call_id": b.id, "name": b.name, "arguments": b.arguments})
				case "result":
					flush()
					input = append(input, map[string]any{"type": "function_call_output", "call_id": b.id, "output": encodeBlocks(b.result, target, "user")})
				default:
					content = append(content, encodeBlocks([]protocolBlock{b}, target, m.role)...)
				}
			}
			flush()
		}
		out["input"] = input
		if r.effort != "" {
			out["reasoning"] = map[string]any{"effort": r.effort}
		}
		if r.parallel != nil {
			out["parallel_tool_calls"] = *r.parallel
		}
	}
	if len(r.tools) > 0 {
		tools := []any{}
		for _, v := range r.tools {
			t := object(v)
			if target == messagesProtocol {
				tool := map[string]any{"name": t["name"], "input_schema": t["parameters"]}
				if t["description"] != nil {
					tool["description"] = t["description"]
				}
				if t["strict"] != nil {
					tool["strict"] = t["strict"]
				}
				tools = append(tools, tool)
			} else {
				tools = append(tools, t)
			}
		}
		out["tools"] = tools
	}
	if r.choice != nil {
		c := object(r.choice)
		if target == messagesProtocol {
			kind := str(c["type"])
			if kind == "required" {
				kind = "any"
			}
			if kind == "function" {
				kind = "tool"
			}
			choice := map[string]any{"type": kind}
			if c["name"] != nil {
				choice["name"] = c["name"]
			}
			if r.parallel != nil {
				choice["disable_parallel_tool_use"] = !*r.parallel
			}
			out["tool_choice"] = choice
		} else {
			if str(c["type"]) == "function" {
				out["tool_choice"] = c
			} else {
				out["tool_choice"] = c["type"]
			}
		}
	} else if target == messagesProtocol && r.parallel != nil {
		out["tool_choice"] = map[string]any{"type": "auto", "disable_parallel_tool_use": !*r.parallel}
	}
	return out, nil
}

func parseProtocolRequest(p map[string]any, source wireProtocol) (protocolRequest, error) {
	r := protocolRequest{temperature: p["temperature"], topP: p["top_p"]}
	allowed := "model stream temperature top_p tools tool_choice"
	switch source {
	case responsesProtocol:
		allowed += " input instructions max_output_tokens reasoning parallel_tool_calls store include text truncation background metadata service_tier"
	case messagesProtocol:
		allowed += " messages system max_tokens stop_sequences thinking output_config"
	case chatProtocol:
		allowed += " messages max_tokens max_completion_tokens reasoning_effort parallel_tool_calls stop stream_options n logprobs top_logprobs response_format frequency_penalty presence_penalty service_tier store modalities verbosity"
	}
	if err := fields(p, allowed); err != nil {
		return r, err
	}
	if v, exists := p["store"]; exists && v != false {
		return r, unsupported("stored responses")
	}
	if p["include"] != nil {
		a, ok := p["include"].([]any)
		if !ok || len(a) != 0 {
			return r, unsupported("include")
		}
	}
	if p["metadata"] != nil {
		m, ok := p["metadata"].(map[string]any)
		if !ok || len(m) != 0 {
			return r, unsupported("metadata")
		}
	}
	for _, field := range []string{"truncation", "service_tier"} {
		if v := p[field]; v != nil {
			expected := "disabled"
			if field == "service_tier" {
				expected = "auto"
			}
			if v != expected {
				return r, unsupported(field)
			}
		}
	}
	if v := p["background"]; v != nil && v != false {
		return r, unsupported("background")
	}
	if v := p["logprobs"]; v != nil && v != false {
		return r, unsupported("logprobs")
	}
	if v := p["verbosity"]; v != nil && v != "medium" {
		return r, unsupported("verbosity")
	}
	if p["modalities"] != nil {
		a, ok := p["modalities"].([]any)
		if !ok || len(a) != 1 || a[0] != "text" {
			return r, unsupported("modalities")
		}
	}
	for _, field := range []string{"n", "frequency_penalty", "presence_penalty", "top_logprobs"} {
		if v := p[field]; v != nil {
			want := int64(0)
			if field == "n" {
				want = 1
			}
			if !defaultProtocolNumber(v, want) {
				return r, unsupported(field)
			}
		}
	}
	if p["text"] != nil {
		o := object(p["text"])
		if o == nil {
			return r, unsupported("text")
		}
		if err := fields(o, "format verbosity"); err != nil {
			return r, err
		}
		if v := o["verbosity"]; v != nil && v != "medium" {
			return r, unsupported("text.verbosity")
		}
		if o["format"] != nil {
			if err := plainTextFormat(o["format"]); err != nil {
				return r, err
			}
		}
	}
	if p["response_format"] != nil {
		if err := plainTextFormat(p["response_format"]); err != nil {
			return r, err
		}
	}
	if v, exists := p["parallel_tool_calls"]; exists {
		b, ok := v.(bool)
		if !ok {
			return r, fmt.Errorf("parallel_tool_calls must be a boolean")
		}
		r.parallel = &b
	}
	if source == chatProtocol && p["stream_options"] != nil {
		o := object(p["stream_options"])
		if o == nil {
			return r, unsupported("stream_options")
		}
		if err := fields(o, "include_usage"); err != nil {
			return r, err
		}
		if v, exists := o["include_usage"]; exists {
			if _, ok := v.(bool); !ok {
				return r, unsupported("stream_options.include_usage")
			}
		}
	}
	if source == responsesProtocol {
		r.maxTokens = p["max_output_tokens"]
		if p["instructions"] != nil {
			v, ok := p["instructions"].(string)
			if !ok {
				return r, unsupported("instructions")
			}
			r.system = append(r.system, protocolBlock{kind: "text", text: v})
		}
		if v, ok := p["input"].(string); ok {
			r.messages = append(r.messages, protocolMessage{role: "user", blocks: []protocolBlock{{kind: "text", text: v}}})
		} else {
			items, ok := p["input"].([]any)
			if !ok {
				return r, fmt.Errorf("input must be text or an array")
			}
			for _, value := range items {
				m := object(value)
				if m == nil {
					return r, unsupported("input item")
				}
				t := str(m["type"])
				if t == "reasoning" {
					b, err := parseResponseReasoning(m)
					if err != nil {
						return r, err
					}
					r.messages = append(r.messages, protocolMessage{role: "assistant", blocks: []protocolBlock{b}})
					continue
				}
				if t == "function_call" {
					if err := fields(m, "type id status call_id name arguments"); err != nil {
						return r, err
					}
					b := protocolBlock{kind: "tool", id: str(m["call_id"]), name: str(m["name"]), arguments: str(m["arguments"])}
					if err := validToolBlock(b); err != nil {
						return r, err
					}
					r.messages = append(r.messages, protocolMessage{role: "assistant", blocks: []protocolBlock{b}})
					continue
				}
				if t == "function_call_output" {
					if err := fields(m, "type id status call_id output"); err != nil {
						return r, err
					}
					b, err := parseBlocks(m["output"], responsesProtocol)
					if err != nil {
						return r, err
					}
					if str(m["call_id"]) == "" {
						return r, fmt.Errorf("Tool results need a call ID")
					}
					r.messages = append(r.messages, protocolMessage{role: "user", blocks: []protocolBlock{{kind: "result", id: str(m["call_id"]), result: b}}})
					continue
				}
				if t != "" && t != "message" {
					return r, unsupported("input item " + t)
				}
				if err := fields(m, "type id status role content"); err != nil {
					return r, err
				}
				b, err := parseBlocks(m["content"], source)
				if err != nil {
					return r, err
				}
				role := str(m["role"])
				if role == "system" || role == "developer" {
					r.system = append(r.system, b...)
					continue
				}
				if role != "user" && role != "assistant" {
					return r, unsupported("message role " + role)
				}
				r.messages = append(r.messages, protocolMessage{role: role, blocks: b})
			}
		}
		if p["reasoning"] != nil {
			o := object(p["reasoning"])
			if o == nil {
				return r, unsupported("reasoning")
			}
			if err := fields(o, "effort summary"); err != nil {
				return r, err
			}
			if o["effort"] != nil {
				if _, ok := o["effort"].(string); !ok {
					return r, unsupported("reasoning.effort")
				}
			}
			if v := o["summary"]; v != nil && v != "auto" {
				return r, unsupported("reasoning.summary")
			}
			r.effort = str(o["effort"])
		}
	} else {
		r.maxTokens = p["max_tokens"]
		if p["max_completion_tokens"] != nil {
			if r.maxTokens != nil {
				return r, fmt.Errorf("Use one max token field")
			}
			r.maxTokens = p["max_completion_tokens"]
		}
		r.stop = p["stop_sequences"]
		if p["stop"] != nil {
			r.stop = p["stop"]
			if text, ok := r.stop.(string); ok {
				r.stop = []any{text}
			}
		}
		if p["system"] != nil {
			b, err := parseBlocks(p["system"], source)
			if err != nil {
				return r, err
			}
			r.system = b
		}
		items, ok := p["messages"].([]any)
		if !ok {
			return r, fmt.Errorf("messages must be an array")
		}
		for _, value := range items {
			m := object(value)
			if m == nil {
				return r, unsupported("message")
			}
			allowed := "role content"
			if source == chatProtocol {
				allowed += " tool_calls tool_call_id reasoning_content vrouter_reasoning refusal"
			}
			if err := fields(m, allowed); err != nil {
				return r, err
			}
			role := str(m["role"])
			b, err := parseBlocks(m["content"], source)
			if err != nil {
				return r, err
			}
			if source == chatProtocol {
				reasoningBlocks := []protocolBlock{}
				if m["vrouter_reasoning"] != nil {
					items, ok := m["vrouter_reasoning"].([]any)
					if !ok {
						return r, unsupported("vrouter_reasoning")
					}
					for _, value := range items {
						state, ok := value.(string)
						if !ok {
							return r, unsupported("vrouter_reasoning")
						}
						block, err := decodeReasoningState(state)
						if err != nil {
							return r, err
						}
						reasoningBlocks = append(reasoningBlocks, block)
					}
				} else if m["reasoning_content"] != nil {
					text, ok := m["reasoning_content"].(string)
					if !ok {
						return r, unsupported("reasoning_content")
					}
					reasoningBlocks = append(reasoningBlocks, protocolBlock{kind: "reasoning", text: text})
				}
				b = append(reasoningBlocks, b...)
				if m["refusal"] != nil {
					text, ok := m["refusal"].(string)
					if !ok {
						return r, unsupported("refusal")
					}
					b = append(b, protocolBlock{kind: "text", text: text})
				}
			}
			if role == "system" || role == "developer" {
				r.system = append(r.system, b...)
				continue
			}
			if source == chatProtocol && role == "tool" {
				if str(m["tool_call_id"]) == "" {
					return r, fmt.Errorf("Tool results need a call ID")
				}
				r.messages = append(r.messages, protocolMessage{role: "user", blocks: []protocolBlock{{kind: "result", id: str(m["tool_call_id"]), result: b}}})
				continue
			}
			if role != "user" && role != "assistant" {
				return r, unsupported("message role " + role)
			}
			if m["tool_calls"] != nil {
				calls, ok := m["tool_calls"].([]any)
				if !ok {
					return r, unsupported("tool_calls")
				}
				for _, value := range calls {
					call := object(value)
					f := object(call["function"])
					if str(call["type"]) != "function" || f == nil {
						return r, unsupported("tool call")
					}
					if err := fields(call, "id type function"); err != nil {
						return r, err
					}
					if err := fields(f, "name arguments"); err != nil {
						return r, err
					}
					tool := protocolBlock{kind: "tool", id: str(call["id"]), name: str(f["name"]), arguments: str(f["arguments"])}
					if err := validToolBlock(tool); err != nil {
						return r, err
					}
					b = append(b, tool)
				}
			}
			if source == chatProtocol {
				b, err = orderChatBlocks(m, b)
				if err != nil {
					return r, err
				}
			}
			r.messages = append(r.messages, protocolMessage{role: role, blocks: b})
		}
		if source == messagesProtocol {
			if p["thinking"] != nil {
				o := object(p["thinking"])
				if o == nil {
					return r, unsupported("thinking")
				}
				if err := fields(o, "type"); err != nil {
					return r, err
				}
				switch str(o["type"]) {
				case "disabled":
					r.effort = "none"
				case "adaptive":
					r.effort = "high"
				default:
					return r, unsupported("thinking type or fixed thinking budget")
				}
			}
			if p["output_config"] != nil {
				o := object(p["output_config"])
				if o == nil {
					return r, unsupported("output_config")
				}
				if err := fields(o, "effort"); err != nil {
					return r, err
				}
				if o["effort"] != nil {
					if _, ok := o["effort"].(string); !ok {
						return r, unsupported("output_config.effort")
					}
				}
				if r.effort == "none" {
					return r, unsupported("effort with disabled thinking")
				}
				r.effort = str(o["effort"])
			}
		} else {
			if p["reasoning_effort"] != nil {
				if _, ok := p["reasoning_effort"].(string); !ok {
					return r, unsupported("reasoning_effort")
				}
			}
			r.effort = str(p["reasoning_effort"])
		}
	}
	if r.effort != "" && r.effort != "none" && r.effort != "low" && r.effort != "medium" && r.effort != "high" && r.effort != "xhigh" {
		return r, unsupported("reasoning effort " + r.effort)
	}
	if p["tools"] != nil {
		tools, ok := p["tools"].([]any)
		if !ok {
			return r, unsupported("tools")
		}
		for _, value := range tools {
			t := object(value)
			if t == nil {
				return r, unsupported("tool definition")
			}
			if source == chatProtocol {
				if str(t["type"]) != "function" {
					return r, unsupported("built-in tool")
				}
				if err := fields(t, "type function"); err != nil {
					return r, err
				}
				t = object(t["function"])
				if t == nil {
					return r, unsupported("function definition")
				}
			}
			if source == messagesProtocol {
				if err := fields(t, "name description input_schema strict"); err != nil {
					return r, err
				}
				tool := map[string]any{"type": "function", "name": t["name"], "parameters": t["input_schema"], "strict": false}
				if t["description"] != nil {
					tool["description"] = t["description"]
				}
				if t["strict"] != nil {
					if _, ok := t["strict"].(bool); !ok {
						return r, unsupported("tool.strict")
					}
					tool["strict"] = t["strict"]
				}
				t = tool
			} else {
				if err := fields(t, "type name description parameters strict"); err != nil {
					return r, err
				}
				if source == responsesProtocol && str(t["type"]) != "function" {
					return r, unsupported("built-in tool")
				}
				if _, exists := t["parameters"]; !exists {
					t["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
				}
				clone := map[string]any{"type": "function", "name": t["name"], "parameters": t["parameters"], "strict": false}
				if t["description"] != nil {
					clone["description"] = t["description"]
				}
				if t["strict"] != nil {
					if _, ok := t["strict"].(bool); !ok {
						return r, unsupported("tool.strict")
					}
					clone["strict"] = t["strict"]
				}
				t = clone
			}
			if str(t["name"]) == "" || object(t["parameters"]) == nil {
				return r, fmt.Errorf("Tools need a name and an object schema")
			}
			r.tools = append(r.tools, t)
		}
	}
	if choice := p["tool_choice"]; choice != nil {
		if text, ok := choice.(string); ok {
			r.choice = map[string]any{"type": text}
		} else {
			c := object(choice)
			if c == nil {
				return r, unsupported("tool_choice")
			}
			if source == messagesProtocol {
				if err := fields(c, "type name disable_parallel_tool_use"); err != nil {
					return r, err
				}
				kind := str(c["type"])
				if kind == "any" {
					kind = "required"
				}
				if kind == "tool" {
					kind = "function"
				}
				r.choice = map[string]any{"type": kind, "name": c["name"]}
				if v, exists := c["disable_parallel_tool_use"]; exists {
					b, ok := v.(bool)
					if !ok {
						return r, unsupported("disable_parallel_tool_use")
					}
					b = !b
					r.parallel = &b
				}
			} else if source == chatProtocol {
				if err := fields(c, "type function"); err != nil {
					return r, err
				}
				f := object(c["function"])
				if err := fields(f, "name"); err != nil {
					return r, err
				}
				r.choice = map[string]any{"type": c["type"], "name": f["name"]}
			} else {
				if err := fields(c, "type name"); err != nil {
					return r, err
				}
				r.choice = c
			}
		}
		kind := str(object(r.choice)["type"])
		if kind != "auto" && kind != "none" && kind != "required" && kind != "function" {
			return r, unsupported("tool_choice " + kind)
		}
		if kind == "function" && str(object(r.choice)["name"]) == "" {
			return r, unsupported("tool_choice without a function name")
		}
	}
	for _, b := range r.system {
		if b.kind != "text" {
			return r, unsupported("non-text system content")
		}
	}
	for _, m := range r.messages {
		for _, b := range m.blocks {
			if b.kind == "reasoning" && m.role != "assistant" {
				return r, unsupported("reasoning outside an assistant message")
			}
			if b.kind == "tool" && m.role != "assistant" {
				return r, unsupported("tool call outside an assistant message")
			}
			if b.kind == "result" {
				if m.role != "user" {
					return r, unsupported("tool result outside a user message")
				}
				for _, content := range b.result {
					if content.kind != "text" && content.kind != "image" {
						return r, unsupported("nested tool result")
					}
				}
			}
		}
	}
	return r, nil
}

func defaultProtocolNumber(value any, want int64) bool {
	n, ok := value.(json.Number)
	if !ok {
		return false
	}
	text := n.String()
	if len(text) > 128 {
		return false
	}
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(text[i+1:])
		if err != nil || exponent < -1000 || exponent > 1000 {
			return false
		}
	}
	rat, ok := new(big.Rat).SetString(text)
	return ok && rat.Cmp(new(big.Rat).SetInt64(want)) == 0
}

func plainTextFormat(value any) error {
	f := object(value)
	if f == nil {
		return unsupported("text format")
	}
	if err := fields(f, "type"); err != nil {
		return err
	}
	if str(f["type"]) != "text" {
		return unsupported("text format " + str(f["type"]))
	}
	return nil
}

func validToolBlock(b protocolBlock) error {
	if b.id == "" || b.name == "" || !json.Valid([]byte(b.arguments)) {
		return fmt.Errorf("Tool calls need an ID, a name, and valid JSON arguments")
	}
	if objectFromJSON(b.arguments) == nil {
		return unsupported("non-object tool arguments")
	}
	return nil
}
func objectFromJSON(text string) map[string]any {
	var value map[string]any
	d := json.NewDecoder(strings.NewReader(text))
	d.UseNumber()
	if d.Decode(&value) != nil {
		return nil
	}
	return value
}

func parseBlocks(value any, source wireProtocol) ([]protocolBlock, error) {
	if value == nil {
		return nil, nil
	}
	if text, ok := value.(string); ok {
		return []protocolBlock{{kind: "text", text: text}}, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, unsupported("content")
	}
	blocks := []protocolBlock{}
	for _, value := range items {
		b := object(value)
		if b == nil {
			return nil, unsupported("content block")
		}
		switch str(b["type"]) {
		case "refusal":
			if err := fields(b, "type refusal"); err != nil {
				return nil, err
			}
			text, ok := b["refusal"].(string)
			if !ok {
				return nil, unsupported("refusal text")
			}
			blocks = append(blocks, protocolBlock{kind: "text", text: text})
		case "thinking":
			if err := fields(b, "type thinking signature"); err != nil {
				return nil, err
			}
			if strings.HasPrefix(str(b["signature"]), reasoningStatePrefix) {
				block, err := decodeReasoningState(str(b["signature"]))
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, block)
			} else {
				provider := ""
				var native map[string]any
				if str(b["signature"]) != "" {
					provider, native = "claude", b
				}
				blocks = append(blocks, protocolBlock{kind: "reasoning", text: str(b["thinking"]), nativeProvider: provider, native: native})
			}
		case "text", "input_text", "output_text":
			if err := fields(b, "type text annotations"); err != nil {
				return nil, err
			}
			if len(list(b["annotations"])) > 0 {
				return nil, unsupported("text annotations")
			}
			text, ok := b["text"].(string)
			if !ok {
				return nil, unsupported("text content")
			}
			blocks = append(blocks, protocolBlock{kind: "text", text: text})
		case "image", "image_url", "input_image":
			image, detail := "", ""
			if str(b["type"]) == "image" {
				if err := fields(b, "type source"); err != nil {
					return nil, err
				}
				s := object(b["source"])
				if err := fields(s, "type media_type data url"); err != nil {
					return nil, err
				}
				if str(s["type"]) == "base64" {
					image = "data:" + str(s["media_type"]) + ";base64," + str(s["data"])
				} else if str(s["type"]) == "url" {
					image = str(s["url"])
				} else {
					return nil, unsupported("image source")
				}
			} else if str(b["type"]) == "image_url" {
				if err := fields(b, "type image_url"); err != nil {
					return nil, err
				}
				s := object(b["image_url"])
				if err := fields(s, "url detail"); err != nil {
					return nil, err
				}
				image, detail = str(s["url"]), str(s["detail"])
			} else {
				if err := fields(b, "type image_url detail"); err != nil {
					return nil, err
				}
				image, detail = str(b["image_url"]), str(b["detail"])
			}
			if image == "" {
				return nil, unsupported("image without a URL or base64 data")
			}
			if source != messagesProtocol && detail != "" && detail != "auto" {
				return nil, unsupported("image detail " + detail)
			}
			blocks = append(blocks, protocolBlock{kind: "image", image: image, detail: detail})
		case "tool_use":
			if err := fields(b, "type id name input"); err != nil {
				return nil, err
			}
			args, err := json.Marshal(b["input"])
			if err != nil {
				return nil, unsupported("tool input")
			}
			tool := protocolBlock{kind: "tool", id: str(b["id"]), name: str(b["name"]), arguments: string(args)}
			if err := validToolBlock(tool); err != nil {
				return nil, err
			}
			blocks = append(blocks, tool)
		case "tool_result":
			if err := fields(b, "type tool_use_id content is_error"); err != nil {
				return nil, err
			}
			if b["is_error"] == true {
				return nil, unsupported("tool_result.is_error")
			}
			content, err := parseBlocks(b["content"], source)
			if err != nil {
				return nil, err
			}
			if str(b["tool_use_id"]) == "" {
				return nil, fmt.Errorf("Tool results need a call ID")
			}
			blocks = append(blocks, protocolBlock{kind: "result", id: str(b["tool_use_id"]), result: content})
		default:
			return nil, unsupported("content block " + str(b["type"]))
		}
	}
	return blocks, nil
}

func encodeBlocks(blocks []protocolBlock, target wireProtocol, role string) []any {
	out := []any{}
	for _, b := range blocks {
		if target == messagesProtocol {
			switch b.kind {
			case "reasoning":
				if b.native != nil {
					out = append(out, b.native)
				} else {
					out = append(out, map[string]any{"type": "text", "text": b.text})
				}
			case "text":
				out = append(out, map[string]any{"type": "text", "text": b.text})
			case "image":
				source := map[string]any{"type": "url", "url": b.image}
				if strings.HasPrefix(b.image, "data:") {
					parts := strings.SplitN(strings.TrimPrefix(b.image, "data:"), ";base64,", 2)
					if len(parts) == 2 {
						source = map[string]any{"type": "base64", "media_type": parts[0], "data": parts[1]}
					}
				}
				out = append(out, map[string]any{"type": "image", "source": source})
			case "tool":
				out = append(out, map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": objectFromJSON(b.arguments)})
			case "result":
				out = append(out, map[string]any{"type": "tool_result", "tool_use_id": b.id, "content": encodeBlocks(b.result, target, "user")})
			}
		} else {
			if b.kind == "text" {
				kind := "input_text"
				if role == "assistant" {
					kind = "output_text"
				}
				out = append(out, map[string]any{"type": kind, "text": b.text})
			}
			if b.kind == "image" {
				v := map[string]any{"type": "input_image", "image_url": b.image}
				if b.detail != "" {
					v["detail"] = b.detail
				}
				out = append(out, v)
			}
		}
	}
	return out
}

func decodeProtocolJSON(data []byte) (map[string]any, error) {
	var result map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&result); err != nil {
		return nil, err
	}
	if result == nil || d.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("Invalid provider JSON")
	}
	return result, nil
}
