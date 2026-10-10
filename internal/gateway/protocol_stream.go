package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Read accounting from the upstream bytes. Translated client events must not
// change provider token counts or make a partial report trustworthy.
type inferenceBodyReader struct {
	source  io.Reader
	attempt *inferenceAttempt
}

func (r inferenceBodyReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if r.attempt != nil {
		r.attempt.usage.observe(p[:n])
		if err == io.EOF {
			r.attempt.bodyComplete = true
			r.attempt.usage.complete()
		}
	}
	return n, err
}

// deliverInference relays a successful provider response and closes its body.
func (s *server) deliverInference(w http.ResponseWriter, r *http.Request, resp *http.Response, attempt *inferenceAttempt, p *preparedInference, account storedAccount) {
	defer resp.Body.Close()
	reader := inferenceBodyReader{source: resp.Body, attempt: attempt}
	if retry := resp.Header.Get("Retry-After"); retry != "" {
		w.Header().Set("Retry-After", retry)
	}
	if id := providerRequestID(resp, account); id != "" {
		w.Header().Set("X-Request-ID", id)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if p.client == p.upstream && p.stream == p.upstreamStream {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		relayNativeInference(w, r, reader, attempt, p, account)
		return
	}
	reply := &protocolReply{model: attempt.model, status: "in_progress", usage: map[string]any{}}
	sink := &replySink{reply: reply, native: p.client == p.upstream, account: account}
	if p.stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(resp.StatusCode)
		sink.emitter = &protocolEmitter{w: w, protocol: p.client, reply: reply, includeUsage: p.includeUsage}
	}
	var err error
	if p.upstreamStream {
		err = readProtocolSSE(reader, p.upstream, sink)
	} else {
		err = readProtocolBody(reader, p.upstream, sink)
	}
	if err != nil || !reply.terminal {
		failInferenceReply(w, r, err, attempt, p.stream, sink)
		return
	}
	if reply.errorBody != nil {
		attempt.outcome = outcomeError
		if !p.stream {
			writeJSON(w, 502, map[string]any{"error": reply.errorBody})
		}
		return
	}
	if reply.status == "incomplete" {
		attempt.outcome = outcomeIncomplete
	}
	if !p.stream {
		writeInferenceReply(w, resp.StatusCode, attempt, p, reply)
	}
}

// relayNativeInference copies a response in the client's own format. Only
// error payloads change, redacted by passthroughScrubber.
func relayNativeInference(w http.ResponseWriter, r *http.Request, reader io.Reader, attempt *inferenceAttempt, p *preparedInference, account storedAccount) {
	scrubber := &passthroughScrubber{account: account, stream: p.stream}
	buf := make([]byte, 32<<10)
	for {
		n, err := reader.Read(buf)
		if n > 0 && !relayInferenceChunk(w, scrubber.next(buf[:n]), p.stream) {
			attempt.outcome = outcomeIncomplete
			return
		}
		// The provider's terminal event ends the stream. Clients may
		// close immediately after receiving it, cancelling the upstream
		// read before HTTP EOF. The delivered response is still complete.
		if p.upstreamStream && attempt.usage.terminal {
			_ = relayInferenceChunk(w, scrubber.flush(), p.stream)
			return
		}
		if err == nil {
			continue
		}
		if !relayInferenceChunk(w, scrubber.flush(), p.stream) {
			attempt.outcome = outcomeIncomplete
			return
		}
		switch {
		case r.Context().Err() != nil:
			attempt.outcome = outcomeIncomplete
		case !errors.Is(err, io.EOF):
			attempt.outcome = outcomeError
		case p.upstreamStream && !attempt.usage.terminal:
			attempt.outcome = outcomeIncomplete
		}
		return
	}
}

func relayInferenceChunk(w http.ResponseWriter, data []byte, flush bool) bool {
	if len(data) == 0 {
		return true
	}
	if _, err := w.Write(data); err != nil {
		return false
	}
	return !flush || http.NewResponseController(w).Flush() == nil
}

func readProtocolBody(reader io.Reader, protocol wireProtocol, sink *replySink) error {
	data, err := io.ReadAll(io.LimitReader(reader, (32<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 32<<20 {
		return errors.New("Provider response exceeds 32 MiB")
	}
	body, err := decodeProtocolJSON(data)
	if err != nil {
		return err
	}
	return consumeProtocolJSON(body, protocol, sink)
}

// failInferenceReply reports a provider response that failed or ended early.
func failInferenceReply(w http.ResponseWriter, r *http.Request, err error, attempt *inferenceAttempt, stream bool, sink *replySink) {
	if err == nil {
		err = errors.New("Provider stream ended before its terminal event")
		attempt.outcome = outcomeIncomplete
	} else {
		attempt.outcome = outcomeError
	}
	var writeErr protocolWriteError
	if r.Context().Err() != nil || errors.As(err, &writeErr) {
		attempt.outcome = outcomeIncomplete
		return
	}
	reply := sink.reply
	reply.status = "failed"
	reply.errorBody = safeProtocolError(map[string]any{"type": "provider_stream_error", "message": err.Error()}, sink.account)
	if stream {
		_ = sink.emitter.finish()
		return
	}
	writeJSON(w, 502, map[string]any{"error": reply.errorBody})
}

func writeInferenceReply(w http.ResponseWriter, status int, attempt *inferenceAttempt, p *preparedInference, reply *protocolReply) {
	if p.client == messagesProtocol && replyHasPartialTool(reply) {
		attempt.outcome = outcomeIncomplete
		writeJSON(w, 502, map[string]any{"error": map[string]any{"type": "provider_incomplete_error", "message": "Provider ended with partial tool arguments. Use streaming to receive these fragments."}})
		return
	}
	// Native Codex nonstream replies keep the complete Responses object.
	if p.client == p.upstream && reply.raw != nil {
		writeJSON(w, status, reply.raw)
		return
	}
	writeJSON(w, status, reply.encode(p.client))
}

func replyHasPartialTool(reply *protocolReply) bool {
	for _, b := range reply.blocks {
		if b.kind == "tool" && objectFromJSON(b.text) == nil {
			return true
		}
	}
	return false
}

// protocolSSEEvent collects the fields of one SSE event until its blank line.
type protocolSSEEvent struct {
	name string
	data bytes.Buffer
}

func (e *protocolSSEEvent) field(line string) error {
	if strings.HasPrefix(line, ":") {
		return nil
	}
	field, value, found := strings.Cut(line, ":")
	if !found {
		return nil
	}
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		e.name = value
	case "data":
		if e.data.Len()+len(value)+1 > 2<<20 {
			return errors.New("Provider SSE event exceeds 2 MiB")
		}
		e.data.WriteString(value)
		e.data.WriteByte('\n')
	}
	return nil
}

// dispatch consumes the collected event. Events after the terminal one are
// dropped.
func (e *protocolSSEEvent) dispatch(protocol wireProtocol, sink *replySink) error {
	name := e.name
	e.name = ""
	if e.data.Len() == 0 {
		return nil
	}
	payload := bytes.TrimSuffix(e.data.Bytes(), []byte("\n"))
	if bytes.Equal(payload, []byte("[DONE]")) {
		e.data.Reset()
		return nil
	}
	body, err := decodeProtocolJSON(payload)
	e.data.Reset()
	if err != nil {
		return errors.New("Provider sent invalid SSE JSON")
	}
	if str(body["type"]) == "" {
		body["type"] = name
	}
	if sink.reply.terminal {
		return nil
	}
	if protocol == messagesProtocol {
		return consumeMessagesEvent(body, sink)
	}
	return consumeResponsesEvent(body, sink)
}

// The SSE parser accepts split reads, CRLF, comments, and multiline data. It
// bounds each event without buffering the complete stream.
func readProtocolSSE(reader io.Reader, protocol wireProtocol, sink *replySink) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var event protocolSSEEvent
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			if err := event.field(line); err != nil {
				return err
			}
			continue
		}
		if err := event.dispatch(protocol, sink); err != nil {
			return err
		}
		if sink.reply.terminal {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.New("Provider stream read failed")
	}
	return event.dispatch(protocol, sink)
}

func mergeReplyUsage(reply *protocolReply, usage any) {
	maps.Copy(reply.usage, object(usage))
}

func replyIndex(v any) string {
	switch n := v.(type) {
	case interface{ String() string }:
		return n.String()
	case int:
		return strconv.Itoa(n)
	case float64:
		return strconv.Itoa(int(n))
	}
	return "0"
}
func messageKey(v any) string { return "m:" + replyIndex(v) }
func responseKey(body map[string]any, kind string) string {
	key := "r:" + replyIndex(body["output_index"])
	if kind == "text" || kind == "refusal" {
		key += ":c:" + replyIndex(body["content_index"])
	}
	if kind == "reasoning" {
		key += ":s:" + replyIndex(body["summary_index"])
	}
	return key
}

func consumeMessagesEvent(body map[string]any, sink *replySink) error {
	r := sink.reply
	switch str(body["type"]) {
	case "ping":
		return nil
	case "message_start":
		m := object(body["message"])
		r.id = str(m["id"])
		mergeReplyUsage(r, m["usage"])
		return sink.begin()
	case "content_block_start":
		return startMessagesBlock(body, sink)
	case "content_block_delta":
		return consumeMessagesDelta(r.find(messageKey(body["index"])), object(body["delta"]), sink)
	case "content_block_stop":
		return stopMessagesBlock(r.find(messageKey(body["index"])), sink)
	case "message_delta":
		mergeReplyUsage(r, body["usage"])
		r.stop = str(object(body["delta"])["stop_reason"])
		if r.stop == "max_tokens" {
			r.status = "incomplete"
		} else {
			r.status = "completed"
		}
		if r.stop == "pause_turn" {
			return unsupported("provider pause_turn")
		}
		return nil
	case "message_stop":
		if r.status == "in_progress" {
			r.status = "completed"
		}
		return sink.finish()
	case "error":
		r.errorBody, r.status = body["error"], "failed"
		return sink.finish()
	default:
		return unsupported("provider event " + str(body["type"]))
	}
}

// messagesBlockKind maps a Claude content block type to a reply block kind.
func messagesBlockKind(b map[string]any) (string, error) {
	switch str(b["type"]) {
	case "text":
		if len(list(b["citations"])) > 0 {
			return "", unsupported("provider text citations")
		}
		return "text", nil
	case "tool_use":
		return "tool", nil
	case "thinking":
		return "reasoning", nil
	}
	return "", unsupported("provider content block " + str(b["type"]))
}

// openMessagesBlock starts a Claude content block with its initial text.
func openMessagesBlock(sink *replySink, key, kind, text string, b map[string]any) error {
	block, err := sink.start(key, kind, "", str(b["id"]), str(b["name"]))
	if err != nil {
		return err
	}
	if kind == "reasoning" {
		block.nativeProvider, block.signature = "claude", str(b["signature"])
	}
	return sink.append(block, text)
}

func startMessagesBlock(body map[string]any, sink *replySink) error {
	b := object(body["content_block"])
	kind, err := messagesBlockKind(b)
	if err != nil {
		return err
	}
	text := str(b["text"])
	switch kind {
	case "tool":
		text = ""
		if len(object(b["input"])) > 0 {
			var raw []byte
			if raw, err = json.Marshal(b["input"]); err != nil {
				return errors.New("Provider sent invalid tool input")
			}
			text = string(raw)
		}
	case "reasoning":
		text = str(b["thinking"])
	}
	return openMessagesBlock(sink, messageKey(body["index"]), kind, text, b)
}

// A tool call without input deltas has empty arguments.
func stopMessagesBlock(b *replyBlock, sink *replySink) error {
	if b != nil && b.kind == "tool" && b.text == "" {
		if err := sink.append(b, "{}"); err != nil {
			return err
		}
	}
	return sink.stop(b)
}

func consumeMessagesDelta(b *replyBlock, d map[string]any, sink *replySink) error {
	switch str(d["type"]) {
	case "text_delta":
		return sink.append(b, str(d["text"]))
	case "input_json_delta":
		return sink.append(b, str(d["partial_json"]))
	case "thinking_delta":
		return sink.append(b, str(d["thinking"]))
	case "signature_delta":
		if b == nil {
			return errors.New("Provider sent a signature without a thinking block")
		}
		if len(b.signature)+len(str(d["signature"])) > maxReasoningStateBytes {
			return errors.New("Provider reasoning signature exceeds 1 MiB")
		}
		b.signature += str(d["signature"])
		return nil
	default:
		return unsupported("provider delta " + str(d["type"]))
	}
}

func consumeResponsesEvent(body map[string]any, sink *replySink) error {
	t := str(body["type"])
	if sink.native {
		return consumeNativeResponsesEvent(t, body, sink)
	}
	r := sink.reply
	switch t {
	case "response.created", "response.in_progress":
		response := object(body["response"])
		if r.id == "" {
			r.id = str(response["id"])
		}
		mergeReplyUsage(r, response["usage"])
		return sink.begin()
	case "response.output_item.added":
		return startResponsesItem(object(body["item"]), body, sink)
	case "response.content_part.added", "response.reasoning_summary_part.added":
		return startResponsesPart(body, sink)
	case "response.output_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.reasoning_summary_text.delta":
		kind, _ := responsesTextKind(t)
		b, err := sink.start(responseKey(body, kind), kind, str(body["item_id"]), "", "")
		if err != nil {
			return err
		}
		return sink.append(b, str(body["delta"]))
	case "response.output_text.done", "response.refusal.done", "response.function_call_arguments.done", "response.reasoning_summary_text.done":
		kind, field := responsesTextKind(t)
		return completeResponsesBlock(sink, responseKey(body, kind), str(body[field]), "Provider completed a missing content block")
	case "response.content_part.done", "response.reasoning_summary_part.done":
		kind, text := responsesPartText(object(body["part"]))
		return completeResponsesBlock(sink, responseKey(body, kind), text, "Provider completed a missing content part")
	case "response.output_item.done":
		return consumeResponseItem(object(body["item"]), replyIndex(body["output_index"]), sink, true)
	case "response.completed", "response.incomplete", "response.failed":
		return finishResponsesEvent(t, body, sink)
	case "error":
		return failResponsesEvent(body, sink)
	case "response.output_text.annotation.added":
		return unsupported("provider text annotations")
	default:
		return unsupported("provider event " + t)
	}
}

// responsesTextKind maps a Responses delta or done event to its block kind
// and the done event's text field.
func responsesTextKind(t string) (string, string) {
	switch t {
	case "response.refusal.delta", "response.refusal.done":
		return "refusal", "refusal"
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		return "tool", "arguments"
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		return "reasoning", "text"
	}
	return "text", "text"
}

func responsesPartText(part map[string]any) (string, string) {
	switch str(part["type"]) {
	case "summary_text":
		return "reasoning", str(part["text"])
	case "refusal":
		return "refusal", str(part["refusal"])
	}
	return "text", str(part["text"])
}

// A done event repeats the complete text of a block that already started.
func completeResponsesBlock(sink *replySink, key, text, missing string) error {
	b := sink.reply.find(key)
	if b == nil {
		return errors.New(missing)
	}
	return sink.full(b, text)
}

func startResponsesItem(item, body map[string]any, sink *replySink) error {
	switch str(item["type"]) {
	case "message", "reasoning":
		return nil
	case "function_call":
		b, err := sink.start(responseKey(body, "tool"), "tool", str(item["id"]), str(item["call_id"]), str(item["name"]))
		if err != nil {
			return err
		}
		return sink.append(b, str(item["arguments"]))
	default:
		return unsupported("provider output item " + str(item["type"]))
	}
}

func startResponsesPart(body map[string]any, sink *replySink) error {
	part := object(body["part"])
	if t := str(part["type"]); t != "summary_text" && t != "refusal" && t != "output_text" {
		return unsupported("provider output part " + t)
	}
	if len(list(part["annotations"])) > 0 {
		return unsupported("provider text annotations")
	}
	kind, text := responsesPartText(part)
	b, err := sink.start(responseKey(body, kind), kind, str(body["item_id"]), "", "")
	if err != nil {
		return err
	}
	return sink.append(b, text)
}

func finishResponsesEvent(t string, body map[string]any, sink *replySink) error {
	r := sink.reply
	response := object(body["response"])
	if response == nil {
		return errors.New("Provider terminal event has no response")
	}
	r.raw = response
	mergeReplyUsage(r, response["usage"])
	r.status = str(response["status"])
	switch t {
	case "response.completed":
		r.status = "completed"
	case "response.incomplete":
		r.status = "incomplete"
	case "response.failed":
		r.status = "failed"
		r.errorBody = response["error"]
		if r.errorBody == nil {
			r.errorBody = map[string]any{"type": "provider_error", "message": "Provider response failed"}
		}
	}
	if r.id == "" {
		r.id = str(response["id"])
	}
	for i, item := range list(response["output"]) {
		if err := consumeResponseItem(object(item), strconv.Itoa(i), sink, false); err != nil {
			return err
		}
	}
	return sink.finish()
}

func failResponsesEvent(body map[string]any, sink *replySink) error {
	r := sink.reply
	r.errorBody, r.status = body["error"], "failed"
	if r.errorBody == nil {
		r.errorBody = map[string]any{"type": body["code"], "message": body["message"]}
	}
	return sink.finish()
}

// A native Responses stream keeps the provider's items so a nonstream client
// receives the complete Responses object.
func consumeNativeResponsesEvent(t string, body map[string]any, sink *replySink) error {
	r := sink.reply
	switch t {
	case "response.output_item.added", "response.output_item.done":
		if r.nativeItems == nil {
			r.nativeItems = map[int]map[string]any{}
		}
		index, _ := strconv.Atoi(replyIndex(body["output_index"]))
		r.nativeItems[index] = object(body["item"])
		return nil
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.output_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.reasoning_summary_text.delta":
		return updateNativeResponseItem(body, r)
	case "response.completed", "response.incomplete", "response.failed":
		response := object(body["response"])
		if response == nil {
			return errors.New("Provider terminal event has no response")
		}
		r.raw, r.id, r.status = response, str(response["id"]), str(response["status"])
		if len(list(response["output"])) == 0 && len(r.nativeItems) > 0 {
			items := make([]any, 0, len(r.nativeItems))
			for _, index := range slices.Sorted(maps.Keys(r.nativeItems)) {
				items = append(items, r.nativeItems[index])
			}
			response["output"] = items
		}
		mergeReplyUsage(r, response["usage"])
		if t == "response.failed" {
			r.errorBody = response["error"]
		}
		return sink.finish()
	case "error":
		return failResponsesEvent(body, sink)
	}
	return nil
}

func updateNativeResponseItem(body map[string]any, reply *protocolReply) error {
	index, _ := strconv.Atoi(replyIndex(body["output_index"]))
	item := reply.nativeItems[index]
	if item == nil {
		return errors.New("Provider sent content without an output item")
	}
	kind := str(body["type"])
	if kind == "response.function_call_arguments.delta" {
		item["arguments"] = str(item["arguments"]) + str(body["delta"])
		return nil
	}
	field, partIndex := "content", "content_index"
	if strings.Contains(kind, "reasoning_summary") {
		field, partIndex = "summary", "summary_index"
	}
	parts := list(item[field])
	n, _ := strconv.Atoi(replyIndex(body[partIndex]))
	if n < 0 || n > 4096 {
		return errors.New("Provider content index is out of range")
	}
	for len(parts) <= n {
		parts = append(parts, nil)
	}
	if strings.HasSuffix(kind, ".added") || strings.HasSuffix(kind, ".done") {
		parts[n] = body["part"]
	} else {
		part := object(parts[n])
		if part == nil {
			return errors.New("Provider sent a delta without a content part")
		}
		text := "text"
		if kind == "response.refusal.delta" {
			text = "refusal"
		}
		part[text] = str(part[text]) + str(body["delta"])
	}
	item[field] = parts
	return nil
}

func consumeResponseItem(item map[string]any, index string, sink *replySink, done bool) error {
	base := "r:" + index
	switch str(item["type"]) {
	case "function_call":
		return addResponseItemPart(sink, item, base, "tool", str(item["arguments"]), done)
	case "message":
		for i, v := range list(item["content"]) {
			part := object(v)
			if t := str(part["type"]); t != "output_text" && t != "refusal" {
				return unsupported("provider output part " + t)
			}
			if len(list(part["annotations"])) > 0 {
				return unsupported("provider text annotations")
			}
			kind, text := responsesPartText(part)
			if err := addResponseItemPart(sink, item, base+":c:"+strconv.Itoa(i), kind, text, done); err != nil {
				return err
			}
		}
		return nil
	case "reasoning":
		return consumeResponseReasoning(item, base, sink, done)
	default:
		return unsupported("provider output item " + str(item["type"]))
	}
}

func addResponseItemPart(sink *replySink, item map[string]any, key, kind, text string, done bool) error {
	b, err := sink.start(key, kind, str(item["id"]), str(item["call_id"]), str(item["name"]))
	if err != nil {
		return err
	}
	if err := sink.full(b, text); err != nil {
		return err
	}
	if done {
		return sink.stop(b)
	}
	return nil
}

func consumeResponseReasoning(item map[string]any, base string, sink *replySink, done bool) error {
	summary := list(item["summary"])
	if len(summary) == 0 {
		b, err := sink.start(base+":s:0", "reasoning", str(item["id"]), "", "")
		if err != nil {
			return err
		}
		b.nativeProvider, b.native = "codex", item
		if done {
			return sink.stop(b)
		}
		return nil
	}
	for i, v := range summary {
		part := object(v)
		if str(part["type"]) != "summary_text" {
			return unsupported("provider reasoning summary")
		}
		b, err := sink.start(base+":s:"+strconv.Itoa(i), "reasoning", str(item["id"]), "", "")
		if err != nil {
			return err
		}
		b.nativeProvider, b.native = "codex", item
		if err := sink.full(b, str(part["text"])); err != nil {
			return err
		}
		if !done {
			continue
		}
		if err := sink.stop(b); err != nil {
			return err
		}
	}
	return nil
}

func consumeProtocolJSON(body map[string]any, protocol wireProtocol, sink *replySink) error {
	r := sink.reply
	r.id = str(body["id"])
	mergeReplyUsage(r, body["usage"])
	r.status = "completed"
	var err error
	if protocol == responsesProtocol {
		err = consumeResponsesJSON(body, sink)
	} else {
		err = consumeMessagesJSON(body, sink)
	}
	if err != nil {
		return err
	}
	return sink.finish()
}

func consumeResponsesJSON(body map[string]any, sink *replySink) error {
	r := sink.reply
	r.raw = body
	if status := str(body["status"]); status != "" {
		r.status = status
	}
	r.errorBody = body["error"]
	for i, item := range list(body["output"]) {
		if err := consumeResponseItem(object(item), strconv.Itoa(i), sink, false); err != nil {
			return err
		}
	}
	return nil
}

func consumeMessagesJSON(body map[string]any, sink *replySink) error {
	r := sink.reply
	r.stop = str(body["stop_reason"])
	if r.stop == "max_tokens" {
		r.status = "incomplete"
	}
	if r.stop == "pause_turn" {
		return unsupported("provider pause_turn")
	}
	for i, value := range list(body["content"]) {
		part := object(value)
		kind, err := messagesBlockKind(part)
		if err != nil {
			return err
		}
		text := str(part["text"])
		switch kind {
		case "tool":
			var raw []byte
			if raw, err = json.Marshal(part["input"]); err != nil {
				return err
			}
			text = string(raw)
		case "reasoning":
			text = str(part["thinking"])
		}
		if err := openMessagesBlock(sink, "m:"+strconv.Itoa(i), kind, text, part); err != nil {
			return err
		}
	}
	return nil
}

// Errors inside a provider stream get the same redaction as HTTP errors.
func safeProtocolError(value any, account storedAccount) any {
	body, _ := json.Marshal(map[string]any{"error": value}) //nolint:errchkjson // value was decoded from JSON; an empty body falls back to the status text
	return sanitizedProviderError(bytes.NewReader(body), http.StatusBadGateway, account)
}
