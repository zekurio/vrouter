package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	payload, model, stream, ierr := readInferencePayload(w, r)
	if ierr != nil {
		return nil, ierr
	}
	attempt.model, attempt.stream = model, stream
	channel, native, accounts, ierr := s.inferenceAccountPool(r.Context(), model)
	if ierr != nil {
		return nil, ierr
	}
	p := &preparedInference{client: inferenceProtocol(r.URL.Path), provider: channel, native: native, stream: stream, upstreamStream: stream}
	p.includeUsage = object(payload["stream_options"])["include_usage"] == true
	// Keep each pool within one billing mode.
	for _, a := range accounts {
		if a.AuthMode == accounts[0].AuthMode {
			p.accounts = append(p.accounts, a)
		}
	}
	p.upstream = responsesProtocol
	if p.provider == "claude" {
		p.upstream = messagesProtocol
	}
	attempt.native = p.native
	if ierr := p.encodeUpstreamRequest(payload); ierr != nil {
		return nil, ierr
	}
	return p, nil
}

func readInferencePayload(w http.ResponseWriter, r *http.Request) (map[string]any, string, bool, *inferenceError) {
	var payload map[string]any
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	d.UseNumber()
	if d.Decode(&payload) != nil || payload == nil || d.Decode(&struct{}{}) != io.EOF {
		return nil, "", false, &inferenceError{status: 400, message: "Expected a JSON inference request, at most 16 MiB"}
	}
	model, ok := payload["model"].(string)
	if !ok || model == "" {
		return nil, "", false, &inferenceError{status: 400, message: "Choose a model"}
	}
	stream := false
	if value := payload["stream"]; value == nil {
		// JSON null means omitted, as for every other field.
		delete(payload, "stream")
	} else if stream, ok = value.(bool); !ok {
		return nil, "", false, &inferenceError{status: 400, message: "stream must be a boolean"}
	}
	return payload, model, stream, nil
}

// inferenceAccountPool returns the one provider channel that advertises the
// public model, its native model ID and the accounts that serve it.
func (s *server) inferenceAccountPool(ctx context.Context, model string) (string, string, []storedAccount, *inferenceError) {
	state := s.store.snapshot()
	byProvider := map[string][]storedAccount{}
	natives := map[string]string{}
	failed := false
	for _, a := range state.Accounts {
		channel := a.Provider
		if a.Disabled || !routableAuth(a) || (channel != "claude" && channel != "codex") {
			continue
		}
		native, ok := inferenceNativeModel(state.Policy, channel, model)
		if !ok {
			continue
		}
		catalog, err := s.accountModels(ctx, a)
		if err != nil {
			failed = true
			continue
		}
		if slices.ContainsFunc(catalog, func(m Model) bool { return m.ID == native }) {
			byProvider[channel] = append(byProvider[channel], a)
			natives[channel] = native
		}
	}
	if len(byProvider) > 1 {
		return "", "", nil, &inferenceError{status: 400, message: "This model ID is advertised by more than one provider. Set a unique public alias and use that alias."}
	}
	if len(byProvider) == 0 {
		if failed {
			return "", "", nil, &inferenceError{status: 502, message: "Provider catalogs unavailable; cannot select an account"}
		}
		return "", "", nil, &inferenceError{status: 404, message: "No enabled account advertises this public model. Check disabled models and public aliases."}
	}
	var channel string
	for c := range byProvider {
		channel = c
	}
	return channel, natives[channel], byProvider[channel], nil
}

// inferenceNativeModel resolves a public model to the channel's native ID. It
// reports false for excluded models and native IDs hidden behind an alias.
func inferenceNativeModel(policy modelPolicy, channel, model string) (string, bool) {
	native := model
	for _, alias := range policy.Aliases[channel] {
		if alias.Alias == model {
			native = alias.Name
			break
		}
	}
	if blocked, _ := excludedModel(policy, channel, native); blocked {
		return "", false
	}
	for _, alias := range policy.Aliases[channel] {
		if alias.Name == native && alias.Alias != model {
			return "", false
		}
	}
	return native, true
}

// encodeUpstreamRequest translates the client payload for the selected
// provider and stores the final body in p.payload.
func (p *preparedInference) encodeUpstreamRequest(clientPayload map[string]any) *inferenceError {
	payload := clientPayload
	if p.client != p.upstream {
		var err error
		payload, err = adaptInferenceRequest(clientPayload, p.client, p.upstream)
		if err != nil {
			return invalidRequest(err)
		}
	} else if err := unwrapNativeReasoning(payload, p.upstream); err != nil {
		return invalidRequest(err)
	}
	payload["model"] = p.native
	var err error
	p.fast, err = inferenceFastMode(payload, p.upstream)
	if err != nil {
		return invalidRequest(err)
	}
	if p.accounts[0].AuthMode == "codex" {
		if ierr := p.applyCodexRequestRules(payload, clientPayload); ierr != nil {
			return ierr
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
		return &inferenceError{status: 400, message: "Invalid inference request"}
	}
	if err := json.Unmarshal(raw, &p.payload); err != nil {
		return &inferenceError{status: 400, message: "Invalid inference request"}
	}
	return nil
}

func (p *preparedInference) applyCodexRequestRules(payload, clientPayload map[string]any) *inferenceError {
	if value := payload["store"]; value != nil && value != false {
		return &inferenceError{status: 400, message: "Codex does not support stored responses. Set store to false."}
	}
	var err error
	p.ignoredParameters, err = normalizeCodexParameters(payload, clientPayload, p.client)
	if err != nil {
		return invalidRequest(err)
	}
	payload["store"], payload["stream"] = false, true
	p.upstreamStream = true
	if _, ok := payload["instructions"]; !ok {
		payload["instructions"] = ""
	}
	return nil
}

// Codex subscription inference accepts output caps but rejects sampling values.
// Validate client controls, and report only sampling fields omitted upstream.
func normalizeCodexParameters(payload, clientPayload map[string]any, client wireProtocol) ([]string, error) {
	ignored := []string{}
	for _, field := range slices.Concat(codexTokenLimitFields(client), []string{"temperature", "top_p"}) {
		value := clientPayload[field]
		if value == nil {
			continue
		}
		if err := validateCodexParameter(field, value, client); err != nil {
			return nil, err
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

func codexTokenLimitFields(client wireProtocol) []string {
	if client == messagesProtocol {
		return []string{"max_tokens"}
	}
	if client == chatProtocol {
		return []string{"max_tokens", "max_completion_tokens"}
	}
	return []string{"max_output_tokens"}
}

func validateCodexParameter(field string, value any, client wireProtocol) error {
	if strings.HasPrefix(field, "max_") {
		return validateTokenLimit(field, value)
	}
	n, ok := protocolNumber(value)
	if !ok {
		return fmt.Errorf("%s must be a valid number", field)
	}
	upper := int64(1)
	if field == "temperature" && client != messagesProtocol {
		upper = 2
	}
	if n.Sign() < 0 || n.Cmp(new(big.Rat).SetInt64(upper)) > 0 {
		return fmt.Errorf("%s must be between 0 and %d", field, upper)
	}
	return nil
}

// validateTokenLimit checks an output cap. Nil means the field was omitted.
func validateTokenLimit(field string, value any) error {
	if value == nil {
		return nil
	}
	n, ok := protocolNumber(value)
	if !ok {
		return fmt.Errorf("%s must be a valid number", field)
	}
	if !n.IsInt() || n.Sign() <= 0 || !n.Num().IsInt64() {
		return fmt.Errorf("%s must be a positive integer", field)
	}
	return nil
}

// validateStopSequences accepts an array of strings, or a single string where
// the client protocol allows one.
func validateStopSequences(field string, value any, single bool) error {
	if value == nil {
		return nil
	}
	if _, ok := value.(string); ok && single {
		return nil
	}
	stops, ok := value.([]any)
	if ok {
		for _, stop := range stops {
			if _, isText := stop.(string); !isText {
				ok = false
				break
			}
		}
	}
	if ok {
		return nil
	}
	if single {
		return fmt.Errorf("%s must be a string or an array of strings", field)
	}
	return fmt.Errorf("%s must be an array of strings", field)
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
	// Sorted, so a request with several unsupported fields always names the same one.
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !set[k] && m[k] != nil {
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
	if err := checkRequestReasoningTargets(r, target); err != nil {
		return nil, err
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
		err = encodeMessagesRequest(r, out)
	} else {
		err = encodeResponsesRequest(r, out, target)
	}
	if err != nil {
		return nil, err
	}
	if len(r.tools) > 0 {
		out["tools"] = encodeRequestTools(r.tools, target)
	}
	encodeRequestToolChoice(r, out, target)
	return out, nil
}

func checkRequestReasoningTargets(r protocolRequest, target wireProtocol) error {
	for _, m := range r.messages {
		for _, b := range m.blocks {
			if b.kind != "reasoning" {
				continue
			}
			if err := reasoningTarget(b, target); err != nil {
				return err
			}
		}
	}
	return nil
}

func encodeMessagesRequest(r protocolRequest, out map[string]any) error {
	limit := r.maxTokens
	if limit == nil {
		limit = 4096
	}
	out["max_tokens"] = limit
	if system := encodeBlocks(r.system, messagesProtocol, "system"); len(system) > 0 {
		out["system"] = system
	}
	messages := make([]any, 0, len(r.messages))
	for _, m := range r.messages {
		messages = append(messages, map[string]any{"role": m.role, "content": encodeBlocks(m.blocks, messagesProtocol, m.role)})
	}
	out["messages"] = messages
	// Claude caches only on request, and the other protocols carry no
	// breakpoints. Let Claude place one and move it as the conversation grows.
	out["cache_control"] = map[string]any{"type": "ephemeral"}
	if r.stop != nil {
		out["stop_sequences"] = r.stop
	}
	return encodeClaudeEffort(r.effort, out)
}

func encodeClaudeEffort(effort string, out map[string]any) error {
	switch effort {
	case "":
		return nil
	case "none":
		out["thinking"] = map[string]any{"type": "disabled"}
		return nil
	case "ultra":
		return unsupported("Claude reasoning effort ultra")
	case "minimal":
		effort = "low"
	}
	out["thinking"] = map[string]any{"type": "adaptive"}
	out["output_config"] = map[string]any{"effort": effort}
	return nil
}

func encodeResponsesRequest(r protocolRequest, out map[string]any, target wireProtocol) error {
	out["store"] = false
	out["include"] = []any{"reasoning.encrypted_content"}
	if r.stop != nil {
		return unsupported("stop sequences")
	}
	if r.maxTokens != nil {
		out["max_output_tokens"] = r.maxTokens
	}
	if len(r.system) > 0 {
		instructions, err := responsesInstructions(r.system)
		if err != nil {
			return err
		}
		out["instructions"] = instructions
	}
	out["input"] = encodeResponsesInput(r.messages, target)
	if r.effort != "" {
		out["reasoning"] = map[string]any{"effort": r.effort}
	}
	if r.parallel != nil {
		out["parallel_tool_calls"] = *r.parallel
	}
	if r.cacheKey != "" {
		out["prompt_cache_key"] = r.cacheKey
	}
	return nil
}

func responsesInstructions(system []protocolBlock) (string, error) {
	parts := []string{}
	for _, b := range system {
		if b.kind != "text" {
			return "", unsupported("system images")
		}
		parts = append(parts, b.text)
	}
	return strings.Join(parts, "\n\n"), nil
}

func encodeResponsesInput(messages []protocolMessage, target wireProtocol) []any {
	input := []any{}
	reasoningSeen := map[string]bool{}
	for _, m := range messages {
		input = appendResponsesMessage(input, m, target, reasoningSeen)
	}
	return input
}

// appendResponsesMessage splits one message into Responses input items.
// Signed reasoning, tool calls and tool results become items of their own.
func appendResponsesMessage(input []any, m protocolMessage, target wireProtocol, reasoningSeen map[string]bool) []any {
	content := []any{}
	flush := func() {
		if len(content) > 0 {
			input = append(input, map[string]any{"type": "message", "role": m.role, "content": content})
			content = nil
		}
	}
	for _, b := range m.blocks {
		switch {
		case b.kind == "reasoning" && b.native != nil:
			flush()
			id := str(b.native["id"])
			if id == "" || !reasoningSeen[id] {
				input = append(input, b.native)
				reasoningSeen[id] = true
			}
		case b.kind == "reasoning":
			content = append(content, map[string]any{"type": "output_text", "text": b.text})
		case b.kind == "tool":
			flush()
			input = append(input, map[string]any{"type": "function_call", "call_id": b.id, "name": b.name, "arguments": b.arguments})
		case b.kind == "result":
			flush()
			input = append(input, responsesToolOutput(b, target))
		default:
			content = append(content, encodeBlocks([]protocolBlock{b}, target, m.role)...)
		}
	}
	flush()
	return input
}

func responsesToolOutput(b protocolBlock, target wireProtocol) map[string]any {
	result := encodeBlocks(b.result, target, "user")
	if b.isError {
		result = append([]any{map[string]any{"type": "input_text", "text": "Tool execution failed."}}, result...)
	}
	return map[string]any{"type": "function_call_output", "call_id": b.id, "output": result}
}

func encodeRequestTools(tools []any, target wireProtocol) []any {
	out := []any{}
	for _, v := range tools {
		t := object(v)
		if target != messagesProtocol {
			out = append(out, t)
			continue
		}
		tool := map[string]any{"name": t["name"], "input_schema": t["parameters"]}
		if t["description"] != nil {
			tool["description"] = t["description"]
		}
		if t["strict"] != nil {
			tool["strict"] = t["strict"]
		}
		out = append(out, tool)
	}
	return out
}

func encodeRequestToolChoice(r protocolRequest, out map[string]any, target wireProtocol) {
	if r.choice == nil {
		if target == messagesProtocol && r.parallel != nil && len(r.tools) > 0 {
			out["tool_choice"] = map[string]any{"type": "auto", "disable_parallel_tool_use": !*r.parallel}
		}
		return
	}
	c := object(r.choice)
	switch {
	case target == messagesProtocol:
		out["tool_choice"] = claudeToolChoice(c, r.parallel)
	case str(c["type"]) == "function":
		out["tool_choice"] = c
	default:
		out["tool_choice"] = c["type"]
	}
}

func claudeToolChoice(c map[string]any, parallel *bool) map[string]any {
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
	if parallel != nil && kind != "none" {
		choice["disable_parallel_tool_use"] = !*parallel
	}
	return choice
}

func parseProtocolRequest(p map[string]any, source wireProtocol) (protocolRequest, error) {
	r := protocolRequest{temperature: p["temperature"], topP: p["top_p"]}
	if err := fields(p, protocolRequestFields(source)); err != nil {
		return r, err
	}
	if err := parseRequestOptions(p, source, &r); err != nil {
		return r, err
	}
	var err error
	if source == responsesProtocol {
		err = parseResponsesRequest(p, &r)
	} else {
		err = parseMessagesRequest(p, source, &r)
	}
	if err != nil {
		return r, err
	}
	if !validRequestEffort(r.effort) {
		return r, unsupported("reasoning effort " + r.effort)
	}
	if p["tools"] != nil {
		if err := parseRequestTools(p["tools"], source, &r); err != nil {
			return r, err
		}
	}
	if choice := p["tool_choice"]; choice != nil {
		if err := parseRequestToolChoice(choice, source, &r); err != nil {
			return r, err
		}
	}
	return r, validateRequestPlacement(r)
}

func protocolRequestFields(source wireProtocol) string {
	allowed := "model stream temperature top_p tools tool_choice"
	switch source {
	case responsesProtocol:
		allowed += " input instructions max_output_tokens reasoning parallel_tool_calls store include text truncation background metadata service_tier prompt_cache_key"
	case messagesProtocol:
		allowed += " messages system max_tokens stop_sequences thinking output_config cache_control metadata service_tier speed"
	case chatProtocol:
		allowed += " messages max_tokens max_completion_tokens reasoning_effort parallel_tool_calls stop stream_options n logprobs top_logprobs response_format frequency_penalty presence_penalty service_tier store modalities verbosity prompt_cache_key"
	}
	return allowed
}

func validRequestEffort(effort string) bool {
	return slices.Contains([]string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, effort)
}

// parseRequestOptions checks the request-level options shared by the client
// protocols. Each protocol accepts only its own subset of them.
func parseRequestOptions(p map[string]any, source wireProtocol, r *protocolRequest) error {
	if v := p["prompt_cache_key"]; v != nil {
		key, ok := v.(string)
		if !ok {
			return errors.New("prompt_cache_key must be a string")
		}
		r.cacheKey = key
	}
	if err := validateRequestOptions(p, source); err != nil {
		return err
	}
	if v := p["parallel_tool_calls"]; v != nil {
		b, ok := v.(bool)
		if !ok {
			return errors.New("parallel_tool_calls must be a boolean")
		}
		r.parallel = &b
	}
	if source == chatProtocol && p["stream_options"] != nil {
		return validateChatStreamOptions(p["stream_options"])
	}
	return nil
}

func validateRequestOptions(p map[string]any, source wireProtocol) error {
	if v := p["store"]; v != nil && v != false {
		return unsupported("stored responses")
	}
	if err := validateRequestInclude(p["include"]); err != nil {
		return err
	}
	if err := validateRequestMetadata(p["metadata"], source); err != nil {
		return err
	}
	if source == messagesProtocol {
		if err := validateClaudeCacheControl(p["cache_control"]); err != nil {
			return err
		}
	}
	if err := validateRequestDefaults(p); err != nil {
		return err
	}
	if err := validateRequestText(p["text"]); err != nil {
		return err
	}
	if p["response_format"] != nil {
		return plainTextFormat(p["response_format"])
	}
	return nil
}

func validateRequestInclude(value any) error {
	if value == nil {
		return nil
	}
	a, ok := value.([]any)
	if !ok {
		return unsupported("include")
	}
	for _, v := range a {
		if v != "reasoning.encrypted_content" {
			return unsupported("include value")
		}
	}
	return nil
}

func validateRequestMetadata(value any, source wireProtocol) error {
	if value == nil {
		return nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return unsupported("metadata")
	}
	if source != messagesProtocol {
		if len(m) != 0 {
			return unsupported("metadata")
		}
		return nil
	}
	for field := range m {
		if field != "user_id" {
			return unsupported("metadata field")
		}
	}
	if value := m["user_id"]; value != nil {
		if _, ok := value.(string); !ok {
			return unsupported("metadata.user_id")
		}
	}
	return nil
}

// validateRequestDefaults accepts options only at the value that matches
// what the provider does without them.
func validateRequestDefaults(p map[string]any) error {
	for _, option := range []struct {
		field string
		want  any
	}{{"truncation", "disabled"}, {"background", false}, {"logprobs", false}, {"verbosity", "medium"}} {
		if v := p[option.field]; v != nil && v != option.want {
			return unsupported(option.field)
		}
	}
	if p["modalities"] != nil {
		a, ok := p["modalities"].([]any)
		if !ok || len(a) != 1 || a[0] != "text" {
			return unsupported("modalities")
		}
	}
	for _, field := range []string{"n", "frequency_penalty", "presence_penalty", "top_logprobs"} {
		want := int64(0)
		if field == "n" {
			want = 1
		}
		if v := p[field]; v != nil && !defaultProtocolNumber(v, want) {
			return unsupported(field)
		}
	}
	return nil
}

func validateRequestText(value any) error {
	if value == nil {
		return nil
	}
	o := object(value)
	if o == nil {
		return unsupported("text")
	}
	if err := fields(o, "format verbosity"); err != nil {
		return err
	}
	if v := o["verbosity"]; v != nil && v != "medium" {
		return unsupported("text.verbosity")
	}
	if o["format"] != nil {
		return plainTextFormat(o["format"])
	}
	return nil
}

func validateChatStreamOptions(value any) error {
	o := object(value)
	if o == nil {
		return unsupported("stream_options")
	}
	if err := fields(o, "include_usage"); err != nil {
		return err
	}
	if v := o["include_usage"]; v != nil {
		if _, ok := v.(bool); !ok {
			return unsupported("stream_options.include_usage")
		}
	}
	return nil
}

func parseResponsesRequest(p map[string]any, r *protocolRequest) error {
	if err := validateTokenLimit("max_output_tokens", p["max_output_tokens"]); err != nil {
		return err
	}
	r.maxTokens = p["max_output_tokens"]
	if p["instructions"] != nil {
		v, ok := p["instructions"].(string)
		if !ok {
			return unsupported("instructions")
		}
		r.system = append(r.system, protocolBlock{kind: "text", text: v})
	}
	if err := parseResponsesInput(p["input"], r); err != nil {
		return err
	}
	if p["reasoning"] != nil {
		effort, err := parseResponsesReasoningOptions(p["reasoning"])
		if err != nil {
			return err
		}
		r.effort = effort
	}
	return nil
}

func parseResponsesInput(value any, r *protocolRequest) error {
	if v, ok := value.(string); ok {
		r.messages = append(r.messages, protocolMessage{role: "user", blocks: []protocolBlock{{kind: "text", text: v}}})
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return errors.New("input must be text or an array")
	}
	for _, item := range items {
		if err := parseResponsesInputItem(item, r); err != nil {
			return err
		}
	}
	return nil
}

func parseResponsesInputItem(value any, r *protocolRequest) error {
	m := object(value)
	if m == nil {
		return unsupported("input item")
	}
	var (
		b    protocolBlock
		err  error
		role = "assistant"
	)
	switch t := str(m["type"]); t {
	case "reasoning":
		b, err = parseResponseReasoning(m)
	case "function_call":
		b, err = parseResponsesFunctionCall(m)
	case "function_call_output":
		b, err = parseResponsesFunctionOutput(m)
		role = "user"
	case "", "message":
		return parseResponsesInputMessage(m, r)
	default:
		return unsupported("input item " + t)
	}
	if err != nil {
		return err
	}
	r.messages = append(r.messages, protocolMessage{role: role, blocks: []protocolBlock{b}})
	return nil
}

func parseResponsesFunctionCall(m map[string]any) (protocolBlock, error) {
	if err := fields(m, "type id status call_id name arguments caller"); err != nil {
		return protocolBlock{}, err
	}
	if err := validateDirectCaller(m["caller"]); err != nil {
		return protocolBlock{}, err
	}
	b := protocolBlock{kind: "tool", id: str(m["call_id"]), name: str(m["name"]), arguments: str(m["arguments"])}
	if err := validToolBlock(b); err != nil {
		return protocolBlock{}, err
	}
	return b, nil
}

func parseResponsesFunctionOutput(m map[string]any) (protocolBlock, error) {
	if err := fields(m, "type id status call_id output caller"); err != nil {
		return protocolBlock{}, err
	}
	if err := validateDirectCaller(m["caller"]); err != nil {
		return protocolBlock{}, err
	}
	b, err := parseBlocks(m["output"], responsesProtocol)
	if err != nil {
		return protocolBlock{}, err
	}
	if str(m["call_id"]) == "" {
		return protocolBlock{}, errors.New("Tool results need a call ID")
	}
	return protocolBlock{kind: "result", id: str(m["call_id"]), result: b}, nil
}

func parseResponsesInputMessage(m map[string]any, r *protocolRequest) error {
	if err := fields(m, "type id status role content"); err != nil {
		return err
	}
	b, err := parseBlocks(m["content"], responsesProtocol)
	if err != nil {
		return err
	}
	role := str(m["role"])
	if role == "system" || role == "developer" {
		r.system = append(r.system, b...)
		return nil
	}
	if role != "user" && role != "assistant" {
		return unsupported("message role " + role)
	}
	r.messages = append(r.messages, protocolMessage{role: role, blocks: b})
	return nil
}

func parseResponsesReasoningOptions(value any) (string, error) {
	o := object(value)
	if o == nil {
		return "", unsupported("reasoning")
	}
	if err := fields(o, "effort summary"); err != nil {
		return "", err
	}
	if o["effort"] != nil {
		if _, ok := o["effort"].(string); !ok {
			return "", unsupported("reasoning.effort")
		}
	}
	if v := o["summary"]; v != nil && v != "auto" {
		return "", unsupported("reasoning.summary")
	}
	return str(o["effort"]), nil
}

// parseMessagesRequest reads the Messages and Chat request shapes, which share
// their message list and token limit fields.
func parseMessagesRequest(p map[string]any, source wireProtocol, r *protocolRequest) error {
	if err := parseMessagesLimits(p, r); err != nil {
		return err
	}
	if p["system"] != nil {
		b, err := parseBlocks(p["system"], source)
		if err != nil {
			return err
		}
		r.system = b
	}
	items, ok := p["messages"].([]any)
	if !ok {
		return errors.New("messages must be an array")
	}
	for _, item := range items {
		if err := parseMessagesItem(item, source, r); err != nil {
			return err
		}
	}
	if source == messagesProtocol {
		return parseClaudeReasoning(p, r)
	}
	if p["reasoning_effort"] != nil {
		if _, ok := p["reasoning_effort"].(string); !ok {
			return unsupported("reasoning_effort")
		}
	}
	r.effort = str(p["reasoning_effort"])
	return nil
}

func parseMessagesLimits(p map[string]any, r *protocolRequest) error {
	// Each protocol's allowed fields were checked already, so only Chat sends
	// max_completion_tokens and stop, and only Messages sends stop_sequences.
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		if err := validateTokenLimit(field, p[field]); err != nil {
			return err
		}
	}
	if err := validateStopSequences("stop_sequences", p["stop_sequences"], false); err != nil {
		return err
	}
	if err := validateStopSequences("stop", p["stop"], true); err != nil {
		return err
	}
	r.maxTokens = p["max_tokens"]
	if p["max_completion_tokens"] != nil {
		if r.maxTokens != nil {
			return errors.New("Use one max token field")
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
	return nil
}

func parseMessagesItem(value any, source wireProtocol, r *protocolRequest) error {
	m := object(value)
	if m == nil {
		return unsupported("message")
	}
	allowed := "role content"
	if source == chatProtocol {
		allowed += " tool_calls tool_call_id reasoning_content vrouter_reasoning refusal"
	}
	if err := fields(m, allowed); err != nil {
		return err
	}
	role := str(m["role"])
	b, err := parseMessagesItemContent(m, source)
	if err != nil {
		return err
	}
	if role == "system" || role == "developer" {
		r.system = append(r.system, b...)
		return nil
	}
	if source == chatProtocol && role == "tool" {
		if str(m["tool_call_id"]) == "" {
			return errors.New("Tool results need a call ID")
		}
		r.messages = append(r.messages, protocolMessage{role: "user", blocks: []protocolBlock{{kind: "result", id: str(m["tool_call_id"]), result: b}}})
		return nil
	}
	if role != "user" && role != "assistant" {
		return unsupported("message role " + role)
	}
	if b, err = appendChatToolCalls(b, m["tool_calls"]); err != nil {
		return err
	}
	if source == chatProtocol {
		if b, err = orderChatBlocks(m, b); err != nil {
			return err
		}
	}
	r.messages = append(r.messages, protocolMessage{role: role, blocks: b})
	return nil
}

// parseMessagesItemContent reads message content. Chat carries reasoning and
// refusals in fields of their own; they go before and after the content.
func parseMessagesItemContent(m map[string]any, source wireProtocol) ([]protocolBlock, error) {
	b, err := parseBlocks(m["content"], source)
	if err != nil || source != chatProtocol {
		return b, err
	}
	reasoning, err := parseChatReasoning(m)
	if err != nil {
		return nil, err
	}
	b = append(reasoning, b...)
	if m["refusal"] != nil {
		text, ok := m["refusal"].(string)
		if !ok {
			return nil, unsupported("refusal")
		}
		b = append(b, protocolBlock{kind: "text", text: text})
	}
	return b, nil
}

func parseChatReasoning(m map[string]any) ([]protocolBlock, error) {
	blocks := []protocolBlock{}
	if m["vrouter_reasoning"] != nil {
		items, ok := m["vrouter_reasoning"].([]any)
		if !ok {
			return nil, unsupported("vrouter_reasoning")
		}
		for _, value := range items {
			state, ok := value.(string)
			if !ok {
				return nil, unsupported("vrouter_reasoning")
			}
			block, err := decodeReasoningState(state)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		}
		return blocks, nil
	}
	if m["reasoning_content"] != nil {
		text, ok := m["reasoning_content"].(string)
		if !ok {
			return nil, unsupported("reasoning_content")
		}
		blocks = append(blocks, protocolBlock{kind: "reasoning", text: text})
	}
	return blocks, nil
}

func appendChatToolCalls(b []protocolBlock, value any) ([]protocolBlock, error) {
	if value == nil {
		return b, nil
	}
	calls, ok := value.([]any)
	if !ok {
		return nil, unsupported("tool_calls")
	}
	for _, v := range calls {
		call := object(v)
		f := object(call["function"])
		if str(call["type"]) != "function" || f == nil {
			return nil, unsupported("tool call")
		}
		if err := fields(call, "id type function"); err != nil {
			return nil, err
		}
		if err := fields(f, "name arguments"); err != nil {
			return nil, err
		}
		tool := protocolBlock{kind: "tool", id: str(call["id"]), name: str(f["name"]), arguments: str(f["arguments"])}
		if err := validToolBlock(tool); err != nil {
			return nil, err
		}
		b = append(b, tool)
	}
	return b, nil
}

func parseClaudeReasoning(p map[string]any, r *protocolRequest) error {
	if p["thinking"] != nil {
		effort, err := parseClaudeThinking(p["thinking"], r.maxTokens)
		if err != nil {
			return err
		}
		r.effort = effort
	}
	if p["output_config"] != nil {
		return parseClaudeOutputConfig(p["output_config"], r)
	}
	return nil
}

func parseClaudeThinking(value, maxTokens any) (string, error) {
	o := object(value)
	if o == nil {
		return "", unsupported("thinking")
	}
	if err := fields(o, "type budget_tokens display"); err != nil {
		return "", err
	}
	if display := o["display"]; display != nil && display != "summarized" {
		return "", unsupported("thinking.display")
	}
	switch str(o["type"]) {
	case "disabled":
		if o["budget_tokens"] != nil {
			return "", unsupported("disabled thinking budget")
		}
		return "none", nil
	case "adaptive":
		if o["budget_tokens"] != nil {
			return "", unsupported("adaptive thinking budget")
		}
		return "high", nil
	case "enabled":
		return parseClaudeThinkingBudget(o["budget_tokens"], maxTokens)
	default:
		return "", unsupported("thinking type")
	}
}

func parseClaudeThinkingBudget(value, maxTokens any) (string, error) {
	budget, ok := protocolNumber(value)
	if !ok || !budget.IsInt() || !budget.Num().IsInt64() || budget.Cmp(big.NewRat(1024, 1)) < 0 {
		return "", errors.New("thinking.budget_tokens must be an integer of at least 1024")
	}
	if maxTokens != nil {
		maximum, ok := protocolNumber(maxTokens)
		if !ok || budget.Cmp(maximum) >= 0 {
			return "", errors.New("thinking.budget_tokens must be less than max_tokens")
		}
	}
	return thinkingBudgetEffort(budget.Num().Int64()), nil
}

func parseClaudeOutputConfig(value any, r *protocolRequest) error {
	o := object(value)
	if o == nil {
		return unsupported("output_config")
	}
	if err := fields(o, "effort"); err != nil {
		return err
	}
	if o["effort"] == nil {
		return nil
	}
	effort, ok := o["effort"].(string)
	if !ok {
		return unsupported("output_config.effort")
	}
	if r.effort == "none" {
		return unsupported("effort with disabled thinking")
	}
	r.effort = effort
	return nil
}

func parseRequestTools(value any, source wireProtocol, r *protocolRequest) error {
	tools, ok := value.([]any)
	if !ok {
		return unsupported("tools")
	}
	for _, v := range tools {
		t, err := parseRequestTool(v, source)
		if err != nil {
			return err
		}
		r.tools = append(r.tools, t)
	}
	return nil
}

// parseRequestTool returns a client tool in the Responses function shape.
func parseRequestTool(value any, source wireProtocol) (map[string]any, error) {
	t := object(value)
	if t == nil {
		return nil, unsupported("tool definition")
	}
	if source == chatProtocol {
		if str(t["type"]) != "function" {
			return nil, unsupported("built-in tool")
		}
		if err := fields(t, "type function"); err != nil {
			return nil, err
		}
		t = object(t["function"])
		if t == nil {
			return nil, unsupported("function definition")
		}
	}
	var err error
	if source == messagesProtocol {
		t, err = parseClaudeToolDefinition(t)
	} else {
		t, err = parseOpenAIToolDefinition(t, source)
	}
	if err != nil {
		return nil, err
	}
	if str(t["name"]) == "" || object(t["parameters"]) == nil {
		return nil, errors.New("Tools need a name and an object schema")
	}
	return t, nil
}

func parseClaudeToolDefinition(t map[string]any) (map[string]any, error) {
	t, err := withoutClaudeCacheControl(t)
	if err != nil {
		return nil, err
	}
	if err := fields(t, "name description input_schema strict type defer_loading allowed_callers input_examples"); err != nil {
		return nil, err
	}
	if err := validateClaudeToolOptions(t); err != nil {
		return nil, err
	}
	return requestFunctionTool(t, t["input_schema"])
}

func validateClaudeToolOptions(t map[string]any) error {
	if t["type"] != nil && t["type"] != "custom" {
		return unsupported("server tool type")
	}
	if t["defer_loading"] != nil && t["defer_loading"] != false {
		return unsupported("deferred tool loading")
	}
	if t["allowed_callers"] != nil {
		callers, ok := t["allowed_callers"].([]any)
		if !ok || len(callers) != 1 || callers[0] != "direct" {
			return unsupported("tool.allowed_callers")
		}
	}
	if t["input_examples"] != nil {
		examples, ok := t["input_examples"].([]any)
		if !ok || len(examples) != 0 {
			return unsupported("tool.input_examples")
		}
	}
	return nil
}

func parseOpenAIToolDefinition(t map[string]any, source wireProtocol) (map[string]any, error) {
	if err := fields(t, "type name description parameters strict"); err != nil {
		return nil, err
	}
	if source == responsesProtocol && str(t["type"]) != "function" {
		return nil, unsupported("built-in tool")
	}
	if _, exists := t["parameters"]; !exists {
		t["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	}
	return requestFunctionTool(t, t["parameters"])
}

func requestFunctionTool(t map[string]any, parameters any) (map[string]any, error) {
	tool := map[string]any{"type": "function", "name": t["name"], "parameters": parameters, "strict": false}
	if t["description"] != nil {
		tool["description"] = t["description"]
	}
	if t["strict"] != nil {
		if _, ok := t["strict"].(bool); !ok {
			return nil, unsupported("tool.strict")
		}
		tool["strict"] = t["strict"]
	}
	return tool, nil
}

func parseRequestToolChoice(choice any, source wireProtocol, r *protocolRequest) error {
	if text, ok := choice.(string); ok {
		r.choice = map[string]any{"type": text}
	} else if err := parseToolChoiceObject(choice, source, r); err != nil {
		return err
	}
	kind := str(object(r.choice)["type"])
	if kind != "auto" && kind != "none" && kind != "required" && kind != "function" {
		return unsupported("tool_choice " + kind)
	}
	if kind == "function" && str(object(r.choice)["name"]) == "" {
		return unsupported("tool_choice without a function name")
	}
	return nil
}

func parseToolChoiceObject(choice any, source wireProtocol, r *protocolRequest) error {
	c := object(choice)
	if c == nil {
		return unsupported("tool_choice")
	}
	switch source {
	case messagesProtocol:
		return parseClaudeToolChoice(c, r)
	case chatProtocol:
		if err := fields(c, "type function"); err != nil {
			return err
		}
		f := object(c["function"])
		if err := fields(f, "name"); err != nil {
			return err
		}
		r.choice = map[string]any{"type": c["type"], "name": f["name"]}
	case responsesProtocol:
		fallthrough
	default:
		if err := fields(c, "type name"); err != nil {
			return err
		}
		r.choice = c
	}
	return nil
}

func parseClaudeToolChoice(c map[string]any, r *protocolRequest) error {
	if err := fields(c, "type name disable_parallel_tool_use"); err != nil {
		return err
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
			return unsupported("disable_parallel_tool_use")
		}
		b = !b
		r.parallel = &b
	}
	return nil
}

// validateRequestPlacement rejects content in a role that cannot carry it.
func validateRequestPlacement(r protocolRequest) error {
	for _, b := range r.system {
		if b.kind != "text" {
			return unsupported("non-text system content")
		}
	}
	for _, m := range r.messages {
		for _, b := range m.blocks {
			if err := validateBlockPlacement(b, m.role); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBlockPlacement(b protocolBlock, role string) error {
	switch {
	case b.kind == "reasoning" && role != "assistant":
		return unsupported("reasoning outside an assistant message")
	case b.kind == "tool" && role != "assistant":
		return unsupported("tool call outside an assistant message")
	case b.kind == "result" && role != "user":
		return unsupported("tool result outside a user message")
	}
	if b.kind != "result" {
		return nil
	}
	for _, content := range b.result {
		if content.kind != "text" && content.kind != "image" {
			return unsupported("nested tool result")
		}
	}
	return nil
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
	for _, item := range items {
		b, err := parseRequestContentBlock(item, source)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

func parseRequestContentBlock(value any, source wireProtocol) (protocolBlock, error) {
	b := object(value)
	if b == nil {
		return protocolBlock{}, unsupported("content block")
	}
	if source == messagesProtocol {
		var err error
		if b, err = withoutClaudeCacheControl(b); err != nil {
			return protocolBlock{}, err
		}
	}
	switch str(b["type"]) {
	case "refusal":
		return parseRequestRefusal(b)
	case "thinking":
		return parseRequestThinking(b)
	case "text", "input_text", "output_text":
		return parseRequestText(b)
	case "image", "image_url", "input_image":
		return parseRequestImage(b, source)
	case "tool_use":
		return parseRequestToolUse(b)
	case "tool_result":
		return parseRequestToolResult(b, source)
	default:
		return protocolBlock{}, unsupported("content block " + str(b["type"]))
	}
}

func parseRequestRefusal(b map[string]any) (protocolBlock, error) {
	if err := fields(b, "type refusal"); err != nil {
		return protocolBlock{}, err
	}
	text, ok := b["refusal"].(string)
	if !ok {
		return protocolBlock{}, unsupported("refusal text")
	}
	return protocolBlock{kind: "text", text: text}, nil
}

func parseRequestThinking(b map[string]any) (protocolBlock, error) {
	if err := fields(b, "type thinking signature"); err != nil {
		return protocolBlock{}, err
	}
	signature := str(b["signature"])
	if strings.HasPrefix(signature, reasoningStatePrefix) {
		return decodeReasoningState(signature)
	}
	block := protocolBlock{kind: "reasoning", text: str(b["thinking"])}
	if signature != "" {
		block.nativeProvider, block.native = "claude", b
	}
	return block, nil
}

func parseRequestText(b map[string]any) (protocolBlock, error) {
	if err := fields(b, "type text annotations"); err != nil {
		return protocolBlock{}, err
	}
	if len(list(b["annotations"])) > 0 {
		return protocolBlock{}, unsupported("text annotations")
	}
	text, ok := b["text"].(string)
	if !ok {
		return protocolBlock{}, unsupported("text content")
	}
	return protocolBlock{kind: "text", text: text}, nil
}

func parseRequestImage(b map[string]any, source wireProtocol) (protocolBlock, error) {
	image, detail, err := requestImageReference(b)
	if err != nil {
		return protocolBlock{}, err
	}
	if image == "" {
		return protocolBlock{}, unsupported("image without a URL or base64 data")
	}
	if source != messagesProtocol && detail != "" && detail != "auto" {
		return protocolBlock{}, unsupported("image detail " + detail)
	}
	return protocolBlock{kind: "image", image: image, detail: detail}, nil
}

// requestImageReference returns the image URL or data URL and its detail.
func requestImageReference(b map[string]any) (string, string, error) {
	switch str(b["type"]) {
	case "image":
		if err := fields(b, "type source"); err != nil {
			return "", "", err
		}
		image, err := claudeRequestImageURL(object(b["source"]))
		return image, "", err
	case "image_url":
		if err := fields(b, "type image_url"); err != nil {
			return "", "", err
		}
		s := object(b["image_url"])
		if err := fields(s, "url detail"); err != nil {
			return "", "", err
		}
		return str(s["url"]), str(s["detail"]), nil
	default:
		if err := fields(b, "type image_url detail"); err != nil {
			return "", "", err
		}
		return str(b["image_url"]), str(b["detail"]), nil
	}
}

func claudeRequestImageURL(s map[string]any) (string, error) {
	if err := fields(s, "type media_type data url"); err != nil {
		return "", err
	}
	switch str(s["type"]) {
	case "base64":
		return "data:" + str(s["media_type"]) + ";base64," + str(s["data"]), nil
	case "url":
		return str(s["url"]), nil
	default:
		return "", unsupported("image source")
	}
}

func parseRequestToolUse(b map[string]any) (protocolBlock, error) {
	if err := fields(b, "type id name input caller"); err != nil {
		return protocolBlock{}, err
	}
	if err := validateDirectCaller(b["caller"]); err != nil {
		return protocolBlock{}, err
	}
	args, err := json.Marshal(b["input"])
	if err != nil {
		return protocolBlock{}, unsupported("tool input")
	}
	tool := protocolBlock{kind: "tool", id: str(b["id"]), name: str(b["name"]), arguments: string(args)}
	if err := validToolBlock(tool); err != nil {
		return protocolBlock{}, err
	}
	return tool, nil
}

func parseRequestToolResult(b map[string]any, source wireProtocol) (protocolBlock, error) {
	if err := fields(b, "type tool_use_id content is_error"); err != nil {
		return protocolBlock{}, err
	}
	if value := b["is_error"]; value != nil {
		if _, ok := value.(bool); !ok {
			return protocolBlock{}, unsupported("tool_result.is_error")
		}
	}
	content, err := parseBlocks(b["content"], source)
	if err != nil {
		return protocolBlock{}, err
	}
	if str(b["tool_use_id"]) == "" {
		return protocolBlock{}, errors.New("Tool results need a call ID")
	}
	return protocolBlock{kind: "result", id: str(b["tool_use_id"]), result: content, isError: b["is_error"] == true}, nil
}

func encodeBlocks(blocks []protocolBlock, target wireProtocol, role string) []any {
	out := []any{}
	for _, b := range blocks {
		if target == messagesProtocol {
			out = appendClaudeRequestBlock(out, b)
		} else {
			out = appendResponsesRequestBlock(out, b, role)
		}
	}
	return out
}

func appendClaudeRequestBlock(out []any, b protocolBlock) []any {
	switch b.kind {
	case "reasoning", "text":
		if b.native != nil {
			return append(out, b.native)
		}
		// Claude rejects empty text blocks, and they carry nothing.
		if b.text != "" {
			return append(out, map[string]any{"type": "text", "text": b.text})
		}
	case "image":
		return append(out, map[string]any{"type": "image", "source": claudeRequestImageSource(b.image)})
	case "tool":
		return append(out, map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": objectFromJSON(b.arguments)})
	case "result":
		result := map[string]any{"type": "tool_result", "tool_use_id": b.id, "content": encodeBlocks(b.result, messagesProtocol, "user")}
		if b.isError {
			result["is_error"] = true
		}
		return append(out, result)
	}
	return out
}

func claudeRequestImageSource(image string) map[string]any {
	if after, ok := strings.CutPrefix(image, "data:"); ok {
		parts := strings.SplitN(after, ";base64,", 2)
		if len(parts) == 2 {
			return map[string]any{"type": "base64", "media_type": parts[0], "data": parts[1]}
		}
	}
	return map[string]any{"type": "url", "url": image}
}

func appendResponsesRequestBlock(out []any, b protocolBlock, role string) []any {
	switch b.kind {
	case "text":
		kind := "input_text"
		if role == "assistant" {
			kind = "output_text"
		}
		return append(out, map[string]any{"type": kind, "text": b.text})
	case "image":
		v := map[string]any{"type": "input_image", "image_url": b.image}
		if b.detail != "" {
			v["detail"] = b.detail
		}
		return append(out, v)
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
