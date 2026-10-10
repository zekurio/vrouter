package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
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

// unsupportedFeatureError marks a valid request the selected provider protocol
// cannot express.
type unsupportedFeatureError string

func (e unsupportedFeatureError) Error() string {
	return "Cannot translate " + string(e) + " to the selected provider protocol"
}

// inferenceError is a request refused before any provider call.
type inferenceError struct {
	status  int
	message string
	code    string
}

func invalidRequest(err error) *inferenceError {
	e := &inferenceError{status: 400, message: err.Error()}
	if _, ok := errors.AsType[unsupportedFeatureError](err); ok {
		e.code = "unsupported_protocol_feature"
	}
	return e
}

func (e *inferenceError) body() map[string]any {
	kind, code := "invalid_request_error", "invalid_request"
	switch e.status {
	case 404:
		kind, code = "not_found_error", "model_not_found"
	case 502:
		kind, code = "api_error", "provider_catalog_unavailable"
	}
	if e.code != "" {
		code = e.code
	}
	return map[string]any{"error": map[string]any{"type": kind, "code": code, "message": e.message}}
}

type preparedInference struct {
	client, upstream       wireProtocol
	provider, native       string
	stream, upstreamStream bool
	includeUsage           bool
	fast                   bool
	ignoredParameters      []string
	payload                map[string]json.RawMessage
	accounts               []storedAccount
}

// Select the model before quota admission. The endpoint only selects the
// client protocol. It does not select the provider or its quota.
func (s *server) prepareInference(w http.ResponseWriter, r *http.Request, attempt *inferenceAttempt) (*preparedInference, *inferenceError) {
	var payload map[string]any
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	d.UseNumber()
	if d.Decode(&payload) != nil || payload == nil || d.Decode(&struct{}{}) != io.EOF {
		return nil, &inferenceError{status: 400, message: "Expected a JSON inference request, at most 16 MiB"}
	}
	model, ok := payload["model"].(string)
	if !ok || model == "" {
		return nil, &inferenceError{status: 400, message: "Choose a model"}
	}
	stream := false
	if value, exists := payload["stream"]; exists {
		stream, ok = value.(bool)
		if !ok {
			return nil, &inferenceError{status: 400, message: "stream must be a boolean"}
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
		return nil, &inferenceError{status: 400, message: "This model ID is advertised by more than one provider. Set a unique public alias and use that alias."}
	}
	if len(byProvider) == 0 {
		if failed {
			return nil, &inferenceError{status: 502, message: "Provider catalogs unavailable; cannot select an account"}
		}
		return nil, &inferenceError{status: 404, message: "No enabled account advertises this public model. Check disabled models and public aliases."}
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
	clientPayload := payload
	if p.client != p.upstream {
		var err error
		payload, err = adaptInferenceRequest(payload, p.client, p.upstream)
		if err != nil {
			return nil, invalidRequest(err)
		}
	} else if err := unwrapNativeReasoning(payload, p.upstream); err != nil {
		return nil, invalidRequest(err)
	}
	payload["model"] = p.native
	var speedErr error
	p.fast, speedErr = inferenceFastMode(payload, p.upstream)
	if speedErr != nil {
		return nil, invalidRequest(speedErr)
	}
	if p.accounts[0].AuthMode == "codex" {
		if value, exists := payload["store"]; exists && value != false {
			return nil, &inferenceError{status: 400, message: "Codex does not support stored responses. Set store to false."}
		}
		var err error
		p.ignoredParameters, err = normalizeCodexParameters(payload, clientPayload, p.client)
		if err != nil {
			return nil, invalidRequest(err)
		}
		payload["store"], payload["stream"] = false, true
		p.upstreamStream = true
		if _, ok := payload["instructions"]; !ok {
			payload["instructions"] = ""
		}
	}
	if p.client == messagesProtocol && p.upstream != messagesProtocol && object(clientPayload["metadata"])["user_id"] != nil {
		p.ignoredParameters = append(p.ignoredParameters, "metadata.user_id")
	}
	if p.upstream == responsesProtocol {
		if text, ok := payload["input"].(string); ok {
			payload["input"] = []any{map[string]any{"role": "user", "content": text}}
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, &inferenceError{status: 400, message: "Invalid inference request"}
	}
	if err := json.Unmarshal(raw, &p.payload); err != nil {
		return nil, &inferenceError{status: 400, message: "Invalid inference request"}
	}
	return p, nil
}

// Codex subscription inference accepts output caps but rejects sampling values.
// Validate client controls, and report only sampling fields omitted upstream.
func normalizeCodexParameters(payload, clientPayload map[string]any, client wireProtocol) ([]string, error) {
	ignored := []string{}
	limits := []string{"max_output_tokens"}
	if client == messagesProtocol {
		limits = []string{"max_tokens"}
	} else if client == chatProtocol {
		limits = []string{"max_tokens", "max_completion_tokens"}
	}
	for _, field := range slices.Concat(limits, []string{"temperature", "top_p"}) {
		value := clientPayload[field]
		if value == nil {
			continue
		}
		n, ok := protocolNumber(value)
		if !ok {
			return nil, fmt.Errorf("%s must be a valid number", field)
		}
		if strings.HasPrefix(field, "max_") {
			if !n.IsInt() || n.Sign() <= 0 || !n.Num().IsInt64() {
				return nil, fmt.Errorf("%s must be a positive integer", field)
			}
		} else {
			upper := int64(1)
			if field == "temperature" && client != messagesProtocol {
				upper = 2
			}
			if n.Sign() < 0 || n.Cmp(new(big.Rat).SetInt64(upper)) > 0 {
				return nil, fmt.Errorf("%s must be between 0 and %d", field, upper)
			}
		}
		if field == "temperature" || field == "top_p" {
			ignored = append(ignored, field)
		}
	}
	for _, field := range []string{"temperature", "top_p"} {
		delete(payload, field)
	}
	return ignored, nil
}

type protocolBlock struct {
	kind, text, image, detail, id, name, arguments string
	result                                         []protocolBlock
	nativeProvider                                 string
	native                                         map[string]any
	projectedID                                    string
	order                                          []any
	isError                                        bool
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
	// cacheKey is a Responses and Chat routing hint. Claude has no equivalent.
	cacheKey string
}

func unsupported(field string) error { return unsupportedFeatureError(field) }
func object(v any) map[string]any    { m, _ := v.(map[string]any); return m }
func list(v any) []any               { a, _ := v.([]any); return a }
func str(v any) string               { s, _ := v.(string); return s }
func fields(m map[string]any, allowed string) error {
	set := map[string]bool{}
	for k := range strings.FieldsSeq(allowed) {
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
	if err := translateInferenceSpeed(payload, out, source, target); err != nil {
		return nil, err
	}
	if r.temperature != nil {
		out["temperature"] = r.temperature
	}
	if r.topP != nil {
		out["top_p"] = r.topP
	}
	if target == messagesProtocol {
		limit := r.maxTokens
		if limit == nil {
			limit = 4096
		}
		out["max_tokens"] = limit
		if system := encodeBlocks(r.system, target, "system"); len(system) > 0 {
			out["system"] = system
		}
		messages := []any{}
		for _, m := range r.messages {
			messages = append(messages, map[string]any{"role": m.role, "content": encodeBlocks(m.blocks, target, m.role)})
		}
		out["messages"] = messages
		// Claude caches only on request, and the other protocols carry no
		// breakpoints. Let Claude place one and move it as the conversation grows.
		out["cache_control"] = map[string]any{"type": "ephemeral"}
		if r.stop != nil {
			out["stop_sequences"] = r.stop
		}
		if r.effort != "" {
			if r.effort == "none" {
				out["thinking"] = map[string]any{"type": "disabled"}
			} else {
				if r.effort == "ultra" {
					return nil, unsupported("Claude reasoning effort ultra")
				}
				effort := r.effort
				if effort == "minimal" {
					effort = "low"
				}
				out["thinking"] = map[string]any{"type": "adaptive"}
				out["output_config"] = map[string]any{"effort": effort}
			}
		}
	} else {
		out["store"] = false
		out["include"] = []any{"reasoning.encrypted_content"}
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
					result := encodeBlocks(b.result, target, "user")
					if b.isError {
						result = append([]any{map[string]any{"type": "input_text", "text": "Tool execution failed."}}, result...)
					}
					input = append(input, map[string]any{"type": "function_call_output", "call_id": b.id, "output": result})
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
		if r.cacheKey != "" {
			out["prompt_cache_key"] = r.cacheKey
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
			// Claude accepts this flag only on choices that can call a tool.
			if r.parallel != nil && kind != "none" {
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
	} else if target == messagesProtocol && r.parallel != nil && len(r.tools) > 0 {
		out["tool_choice"] = map[string]any{"type": "auto", "disable_parallel_tool_use": !*r.parallel}
	}
	return out, nil
}

func parseProtocolRequest(p map[string]any, source wireProtocol) (protocolRequest, error) {
	r := protocolRequest{temperature: p["temperature"], topP: p["top_p"]}
	allowed := "model stream temperature top_p tools tool_choice"
	switch source {
	case responsesProtocol:
		allowed += " input instructions max_output_tokens reasoning parallel_tool_calls store include text truncation background metadata service_tier prompt_cache_key"
	case messagesProtocol:
		allowed += " messages system max_tokens stop_sequences thinking output_config cache_control metadata service_tier speed"
	case chatProtocol:
		allowed += " messages max_tokens max_completion_tokens reasoning_effort parallel_tool_calls stop stream_options n logprobs top_logprobs response_format frequency_penalty presence_penalty service_tier store modalities verbosity prompt_cache_key"
	}
	if err := fields(p, allowed); err != nil {
		return r, err
	}
	if v := p["prompt_cache_key"]; v != nil {
		key, ok := v.(string)
		if !ok {
			return r, errors.New("prompt_cache_key must be a string")
		}
		r.cacheKey = key
	}
	if v, exists := p["store"]; exists && v != false {
		return r, unsupported("stored responses")
	}
	if p["include"] != nil {
		a, ok := p["include"].([]any)
		if !ok {
			return r, unsupported("include")
		}
		for _, value := range a {
			if value != "reasoning.encrypted_content" {
				return r, unsupported("include value")
			}
		}
	}
	if p["metadata"] != nil {
		m, ok := p["metadata"].(map[string]any)
		if !ok {
			return r, unsupported("metadata")
		}
		if source == messagesProtocol {
			for field := range m {
				if field != "user_id" {
					return r, unsupported("metadata field")
				}
			}
			if value := m["user_id"]; value != nil {
				if _, ok := value.(string); !ok {
					return r, unsupported("metadata.user_id")
				}
			}
		} else if len(m) != 0 {
			return r, unsupported("metadata")
		}
	}
	if source == messagesProtocol {
		if err := validateClaudeCacheControl(p["cache_control"]); err != nil {
			return r, err
		}
	}
	if v := p["truncation"]; v != nil && v != "disabled" {
		return r, unsupported("truncation")
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
			return r, errors.New("parallel_tool_calls must be a boolean")
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
				return r, errors.New("input must be text or an array")
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
					if err := fields(m, "type id status call_id name arguments caller"); err != nil {
						return r, err
					}
					if err := validateDirectCaller(m["caller"]); err != nil {
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
					if err := fields(m, "type id status call_id output caller"); err != nil {
						return r, err
					}
					if err := validateDirectCaller(m["caller"]); err != nil {
						return r, err
					}
					b, err := parseBlocks(m["output"], responsesProtocol)
					if err != nil {
						return r, err
					}
					if str(m["call_id"]) == "" {
						return r, errors.New("Tool results need a call ID")
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
				return r, errors.New("Use one max token field")
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
		if stops, ok := r.stop.([]any); ok && len(stops) == 0 {
			r.stop = nil
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
			return r, errors.New("messages must be an array")
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
					return r, errors.New("Tool results need a call ID")
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
				if err := fields(o, "type budget_tokens display"); err != nil {
					return r, err
				}
				if display := o["display"]; display != nil && display != "summarized" {
					return r, unsupported("thinking.display")
				}
				switch str(o["type"]) {
				case "disabled":
					if o["budget_tokens"] != nil {
						return r, unsupported("disabled thinking budget")
					}
					r.effort = "none"
				case "adaptive":
					if o["budget_tokens"] != nil {
						return r, unsupported("adaptive thinking budget")
					}
					r.effort = "high"
				case "enabled":
					budget, ok := protocolNumber(o["budget_tokens"])
					if !ok || !budget.IsInt() || !budget.Num().IsInt64() || budget.Cmp(big.NewRat(1024, 1)) < 0 {
						return r, errors.New("thinking.budget_tokens must be an integer of at least 1024")
					}
					if r.maxTokens != nil {
						maximum, ok := protocolNumber(r.maxTokens)
						if !ok || budget.Cmp(maximum) >= 0 {
							return r, errors.New("thinking.budget_tokens must be less than max_tokens")
						}
					}
					r.effort = thinkingBudgetEffort(budget.Num().Int64())
				default:
					return r, unsupported("thinking type")
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
				if o["effort"] != nil {
					if r.effort == "none" {
						return r, unsupported("effort with disabled thinking")
					}
					r.effort = str(o["effort"])
				}
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
	if r.effort != "" && r.effort != "none" && r.effort != "minimal" && r.effort != "low" && r.effort != "medium" && r.effort != "high" && r.effort != "xhigh" && r.effort != "max" && r.effort != "ultra" {
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
				var err error
				t, err = withoutClaudeCacheControl(t)
				if err != nil {
					return r, err
				}
				if err := fields(t, "name description input_schema strict type defer_loading allowed_callers input_examples"); err != nil {
					return r, err
				}
				if t["type"] != nil && t["type"] != "custom" {
					return r, unsupported("server tool type")
				}
				if t["defer_loading"] != nil && t["defer_loading"] != false {
					return r, unsupported("deferred tool loading")
				}
				if t["allowed_callers"] != nil {
					callers, ok := t["allowed_callers"].([]any)
					if !ok || len(callers) != 1 || callers[0] != "direct" {
						return r, unsupported("tool.allowed_callers")
					}
				}
				if t["input_examples"] != nil {
					examples, ok := t["input_examples"].([]any)
					if !ok || len(examples) != 0 {
						return r, unsupported("tool.input_examples")
					}
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
				return r, errors.New("Tools need a name and an object schema")
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
			switch source {
			case messagesProtocol:
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
			case chatProtocol:
				if err := fields(c, "type function"); err != nil {
					return r, err
				}
				f := object(c["function"])
				if err := fields(f, "name"); err != nil {
					return r, err
				}
				r.choice = map[string]any{"type": c["type"], "name": f["name"]}
			case responsesProtocol:
				fallthrough
			default:
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
	rat, ok := protocolNumber(value)
	return ok && rat.Cmp(new(big.Rat).SetInt64(want)) == 0
}

func protocolNumber(value any) (*big.Rat, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return nil, false
	}
	text := n.String()
	if len(text) > 128 {
		return nil, false
	}
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(text[i+1:])
		if err != nil || exponent < -1000 || exponent > 1000 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(text)
}

// A fixed Claude budget becomes an approximate effort hint in Responses.
// These ranges do not impose a reasoning-token budget on the target provider.
func thinkingBudgetEffort(budget int64) string {
	switch {
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	default:
		return "xhigh"
	}
}

func validateClaudeCacheControl(value any) error {
	if value == nil {
		return nil
	}
	cache := object(value)
	if cache == nil {
		return unsupported("cache_control")
	}
	if err := fields(cache, "type ttl"); err != nil {
		return err
	}
	if cache["type"] != "ephemeral" {
		return unsupported("cache_control.type")
	}
	if ttl := cache["ttl"]; ttl != nil && ttl != "5m" && ttl != "1h" {
		return unsupported("cache_control.ttl")
	}
	return nil
}

func withoutClaudeCacheControl(value map[string]any) (map[string]any, error) {
	cache, exists := value["cache_control"]
	if !exists {
		return value, nil
	}
	if err := validateClaudeCacheControl(cache); err != nil {
		return nil, err
	}
	cloned := make(map[string]any, len(value))
	for field, v := range value {
		if field != "cache_control" {
			cloned[field] = v
		}
	}
	return cloned, nil
}

func validateDirectCaller(value any) error {
	if value == nil {
		return nil
	}
	caller := object(value)
	if caller == nil {
		return unsupported("tool caller")
	}
	if err := fields(caller, "type"); err != nil {
		return err
	}
	if caller["type"] != "direct" {
		return unsupported("tool caller")
	}
	return nil
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
		return errors.New("Tool calls need an ID, a name, and valid JSON arguments")
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
		if source == messagesProtocol {
			var err error
			b, err = withoutClaudeCacheControl(b)
			if err != nil {
				return nil, err
			}
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
			var image, detail string
			switch str(b["type"]) {
			case "image":
				if err := fields(b, "type source"); err != nil {
					return nil, err
				}
				s := object(b["source"])
				if err := fields(s, "type media_type data url"); err != nil {
					return nil, err
				}
				switch str(s["type"]) {
				case "base64":
					image = "data:" + str(s["media_type"]) + ";base64," + str(s["data"])
				case "url":
					image = str(s["url"])
				default:
					return nil, unsupported("image source")
				}
			case "image_url":
				if err := fields(b, "type image_url"); err != nil {
					return nil, err
				}
				s := object(b["image_url"])
				if err := fields(s, "url detail"); err != nil {
					return nil, err
				}
				image, detail = str(s["url"]), str(s["detail"])
			default:
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
			if err := fields(b, "type id name input caller"); err != nil {
				return nil, err
			}
			if err := validateDirectCaller(b["caller"]); err != nil {
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
			if value := b["is_error"]; value != nil {
				if _, ok := value.(bool); !ok {
					return nil, unsupported("tool_result.is_error")
				}
			}
			content, err := parseBlocks(b["content"], source)
			if err != nil {
				return nil, err
			}
			if str(b["tool_use_id"]) == "" {
				return nil, errors.New("Tool results need a call ID")
			}
			blocks = append(blocks, protocolBlock{kind: "result", id: str(b["tool_use_id"]), result: content, isError: b["is_error"] == true})
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
			case "reasoning", "text":
				if b.native != nil {
					out = append(out, b.native)
				} else if b.text != "" {
					// Claude rejects empty text blocks, and they carry nothing.
					out = append(out, map[string]any{"type": "text", "text": b.text})
				}
			case "image":
				source := map[string]any{"type": "url", "url": b.image}
				if after, ok := strings.CutPrefix(b.image, "data:"); ok {
					parts := strings.SplitN(after, ";base64,", 2)
					if len(parts) == 2 {
						source = map[string]any{"type": "base64", "media_type": parts[0], "data": parts[1]}
					}
				}
				out = append(out, map[string]any{"type": "image", "source": source})
			case "tool":
				out = append(out, map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": objectFromJSON(b.arguments)})
			case "result":
				result := map[string]any{"type": "tool_result", "tool_use_id": b.id, "content": encodeBlocks(b.result, target, "user")}
				if b.isError {
					result["is_error"] = true
				}
				out = append(out, result)
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
		return nil, errors.New("Invalid provider JSON")
	}
	return result, nil
}
