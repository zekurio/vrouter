package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
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

func (s *server) deliverInference(w http.ResponseWriter, r *http.Request, resp *http.Response, attempt *inferenceAttempt, p *preparedInference, account storedAccount) {
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
		buf := make([]byte, 32<<10)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					attempt.outcome = outcomeIncomplete
					return
				}
				if p.stream {
					if err := http.NewResponseController(w).Flush(); err != nil {
						attempt.outcome = outcomeIncomplete
						return
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					attempt.outcome = outcomeError
				} else if p.upstreamStream && !attempt.usage.terminal {
					attempt.outcome = outcomeIncomplete
				}
				return
			}
		}
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
		var data []byte
		data, err = io.ReadAll(io.LimitReader(reader, (32<<20)+1))
		if err == nil && len(data) > 32<<20 {
			err = fmt.Errorf("Provider response exceeds 32 MiB")
		}
		if err == nil {
			var body map[string]any
			body, err = decodeProtocolJSON(data)
			if err == nil {
				err = consumeProtocolJSON(body, p.upstream, sink)
			}
		}
	}
	if err != nil || !reply.terminal {
		if err == nil {
			err = fmt.Errorf("Provider stream ended before its terminal event")
			attempt.outcome = outcomeIncomplete
		} else {
			attempt.outcome = outcomeError
		}
		var writeErr protocolWriteError
		if r.Context().Err() != nil || errors.As(err, &writeErr) {
			attempt.outcome = outcomeIncomplete
			return
		}
		reply.status = "failed"
		reply.errorBody = safeProtocolError(map[string]any{"type": "provider_stream_error", "message": err.Error()}, account)
		if p.stream {
			_ = sink.emitter.finish()
		} else {
			writeJSON(w, 502, map[string]any{"error": reply.errorBody})
		}
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
		if p.client == messagesProtocol {
			for _, b := range reply.blocks {
				if b.kind == "tool" && objectFromJSON(b.text) == nil {
					attempt.outcome = outcomeIncomplete
					writeJSON(w, 502, map[string]any{"error": map[string]any{"type": "provider_incomplete_error", "message": "Provider ended with partial tool arguments. Use streaming to receive these fragments."}})
					return
				}
			}
		}
		// Native Codex nonstream replies keep the complete Responses object.
		if p.client == p.upstream && reply.raw != nil {
			writeJSON(w, resp.StatusCode, reply.raw)
		} else {
			writeJSON(w, resp.StatusCode, reply.encode(p.client))
		}
	}
}

// The SSE parser accepts split reads, CRLF, comments, and multiline data. It
// bounds each event without buffering the complete stream.
func readProtocolSSE(reader io.Reader, protocol wireProtocol, sink *replySink) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	name := ""
	var data bytes.Buffer
	dispatch := func() error {
		if data.Len() == 0 {
			name = ""
			return nil
		}
		payload := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		if bytes.Equal(payload, []byte("[DONE]")) {
			data.Reset()
			name = ""
			return nil
		}
		body, err := decodeProtocolJSON(payload)
		data.Reset()
		if err != nil {
			return fmt.Errorf("Provider sent invalid SSE JSON")
		}
		if str(body["type"]) == "" {
			body["type"] = name
		}
		name = ""
		if sink.reply.terminal {
			return nil
		}
		if protocol == messagesProtocol {
			return consumeMessagesEvent(body, sink)
		}
		return consumeResponsesEvent(body, sink)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			if sink.reply.terminal {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = value
		case "data":
			if data.Len()+len(value)+1 > 2<<20 {
				return fmt.Errorf("Provider SSE event exceeds 2 MiB")
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("Provider stream read failed")
	}
	return dispatch()
}

func mergeReplyUsage(reply *protocolReply, usage any) {
	for k, v := range object(usage) {
		reply.usage[k] = v
	}
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
		b := object(body["content_block"])
		kind, text := "text", str(b["text"])
		switch str(b["type"]) {
		case "text":
			if len(list(b["citations"])) > 0 {
				return unsupported("provider text citations")
			}
		case "tool_use":
			kind = "tool"
			text = ""
			if len(object(b["input"])) > 0 {
				raw, _ := json.Marshal(b["input"])
				text = string(raw)
			}
		case "thinking":
			kind, text = "reasoning", str(b["thinking"])
		default:
			return unsupported("provider content block " + str(b["type"]))
		}
		block, err := sink.start(messageKey(body["index"]), kind, "", str(b["id"]), str(b["name"]))
		if err != nil {
			return err
		}
		if kind == "reasoning" {
			block.nativeProvider, block.signature = "claude", str(b["signature"])
		}
		return sink.append(block, text)
	case "content_block_delta":
		b := r.find(messageKey(body["index"]))
		d := object(body["delta"])
		switch str(d["type"]) {
		case "text_delta":
			return sink.append(b, str(d["text"]))
		case "input_json_delta":
			return sink.append(b, str(d["partial_json"]))
		case "thinking_delta":
			return sink.append(b, str(d["thinking"]))
		case "signature_delta":
			if b == nil {
				return fmt.Errorf("Provider sent a signature without a thinking block")
			}
			if len(b.signature)+len(str(d["signature"])) > maxReasoningStateBytes {
				return fmt.Errorf("Provider reasoning signature exceeds 1 MiB")
			}
			b.signature += str(d["signature"])
			return nil
		default:
			return unsupported("provider delta " + str(d["type"]))
		}
	case "content_block_stop":
		b := r.find(messageKey(body["index"]))
		if b != nil && b.kind == "tool" && b.text == "" {
			if err := sink.append(b, "{}"); err != nil {
				return err
			}
		}
		return sink.stop(b)
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

func consumeResponsesEvent(body map[string]any, sink *replySink) error {
	r := sink.reply
	t := str(body["type"])
	if sink.native {
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
				return fmt.Errorf("Provider terminal event has no response")
			}
			r.raw, r.id, r.status = response, str(response["id"]), str(response["status"])
			if len(list(response["output"])) == 0 && len(r.nativeItems) > 0 {
				indices := make([]int, 0, len(r.nativeItems))
				for index := range r.nativeItems {
					indices = append(indices, index)
				}
				sort.Ints(indices)
				items := []any{}
				for _, index := range indices {
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
			r.status, r.errorBody = "failed", body["error"]
			if r.errorBody == nil {
				r.errorBody = map[string]any{"type": body["code"], "message": body["message"]}
			}
			return sink.finish()
		}
		return nil
	}
	switch t {
	case "response.created", "response.in_progress":
		response := object(body["response"])
		if r.id == "" {
			r.id = str(response["id"])
		}
		mergeReplyUsage(r, response["usage"])
		return sink.begin()
	case "response.output_item.added":
		item := object(body["item"])
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
	case "response.content_part.added", "response.reasoning_summary_part.added":
		part := object(body["part"])
		kind, text := "text", str(part["text"])
		if str(part["type"]) == "summary_text" {
			kind = "reasoning"
		} else if str(part["type"]) == "refusal" {
			kind, text = "refusal", str(part["refusal"])
		} else if str(part["type"]) != "output_text" {
			return unsupported("provider output part " + str(part["type"]))
		}
		if len(list(part["annotations"])) > 0 {
			return unsupported("provider text annotations")
		}
		b, err := sink.start(responseKey(body, kind), kind, str(body["item_id"]), "", "")
		if err != nil {
			return err
		}
		return sink.append(b, text)
	case "response.output_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.reasoning_summary_text.delta":
		kind := "text"
		if t == "response.refusal.delta" {
			kind = "refusal"
		}
		if t == "response.function_call_arguments.delta" {
			kind = "tool"
		}
		if t == "response.reasoning_summary_text.delta" {
			kind = "reasoning"
		}
		b := r.find(responseKey(body, kind))
		if b == nil {
			var err error
			b, err = sink.start(responseKey(body, kind), kind, str(body["item_id"]), "", "")
			if err != nil {
				return err
			}
		}
		return sink.append(b, str(body["delta"]))
	case "response.output_text.done", "response.refusal.done", "response.function_call_arguments.done", "response.reasoning_summary_text.done":
		kind, field := "text", "text"
		if t == "response.refusal.done" {
			kind, field = "refusal", "refusal"
		}
		if t == "response.function_call_arguments.done" {
			kind, field = "tool", "arguments"
		}
		if t == "response.reasoning_summary_text.done" {
			kind = "reasoning"
		}
		b := r.find(responseKey(body, kind))
		if b == nil {
			return fmt.Errorf("Provider completed a missing content block")
		}
		return sink.full(b, str(body[field]))
	case "response.content_part.done", "response.reasoning_summary_part.done":
		part := object(body["part"])
		kind, text := "text", str(part["text"])
		if str(part["type"]) == "summary_text" {
			kind = "reasoning"
		}
		if str(part["type"]) == "refusal" {
			kind, text = "refusal", str(part["refusal"])
		}
		b := r.find(responseKey(body, kind))
		if b == nil {
			return fmt.Errorf("Provider completed a missing content part")
		}
		return sink.full(b, text)
	case "response.output_item.done":
		return consumeResponseItem(object(body["item"]), replyIndex(body["output_index"]), sink, true)
	case "response.completed", "response.incomplete", "response.failed":
		response := object(body["response"])
		if response == nil {
			return fmt.Errorf("Provider terminal event has no response")
		}
		r.raw = response
		mergeReplyUsage(r, response["usage"])
		r.status = str(response["status"])
		if t == "response.completed" {
			r.status = "completed"
		}
		if t == "response.incomplete" {
			r.status = "incomplete"
		}
		if t == "response.failed" {
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
	case "error":
		r.errorBody, r.status = body["error"], "failed"
		if r.errorBody == nil {
			r.errorBody = map[string]any{"type": body["code"], "message": body["message"]}
		}
		return sink.finish()
	case "response.output_text.annotation.added":
		return unsupported("provider text annotations")
	default:
		return unsupported("provider event " + t)
	}
}

func updateNativeResponseItem(body map[string]any, reply *protocolReply) error {
	index, _ := strconv.Atoi(replyIndex(body["output_index"]))
	item := reply.nativeItems[index]
	if item == nil {
		return fmt.Errorf("Provider sent content without an output item")
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
		return fmt.Errorf("Provider content index is out of range")
	}
	for len(parts) <= n {
		parts = append(parts, nil)
	}
	if strings.HasSuffix(kind, ".added") || strings.HasSuffix(kind, ".done") {
		parts[n] = body["part"]
	} else {
		part := object(parts[n])
		if part == nil {
			return fmt.Errorf("Provider sent a delta without a content part")
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
	add := func(key, kind, text string) error {
		b, err := sink.start(key, kind, str(item["id"]), str(item["call_id"]), str(item["name"]))
		if err != nil {
			return err
		}
		if err = sink.full(b, text); err != nil {
			return err
		}
		if done {
			return sink.stop(b)
		}
		return nil
	}
	switch str(item["type"]) {
	case "function_call":
		return add(base, "tool", str(item["arguments"]))
	case "message":
		for i, v := range list(item["content"]) {
			part := object(v)
			kind, text := "text", str(part["text"])
			if str(part["type"]) == "refusal" {
				kind, text = "refusal", str(part["refusal"])
			} else if str(part["type"]) != "output_text" {
				return unsupported("provider output part " + str(part["type"]))
			}
			if len(list(part["annotations"])) > 0 {
				return unsupported("provider text annotations")
			}
			if err := add(base+":c:"+strconv.Itoa(i), kind, text); err != nil {
				return err
			}
		}
	case "reasoning":
		if len(list(item["summary"])) == 0 {
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
		for i, v := range list(item["summary"]) {
			part := object(v)
			if str(part["type"]) != "summary_text" {
				return unsupported("provider reasoning summary")
			}
			key := base + ":s:" + strconv.Itoa(i)
			b, err := sink.start(key, "reasoning", str(item["id"]), "", "")
			if err != nil {
				return err
			}
			b.nativeProvider, b.native = "codex", item
			if err := sink.full(b, str(part["text"])); err != nil {
				return err
			}
			if done {
				if err := sink.stop(b); err != nil {
					return err
				}
			}
		}
	default:
		return unsupported("provider output item " + str(item["type"]))
	}
	return nil
}

func consumeProtocolJSON(body map[string]any, protocol wireProtocol, sink *replySink) error {
	r := sink.reply
	r.id = str(body["id"])
	mergeReplyUsage(r, body["usage"])
	r.status = "completed"
	if protocol == responsesProtocol {
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
	} else {
		r.stop = str(body["stop_reason"])
		if r.stop == "max_tokens" {
			r.status = "incomplete"
		}
		if r.stop == "pause_turn" {
			return unsupported("provider pause_turn")
		}
		for i, value := range list(body["content"]) {
			part := object(value)
			kind, text := "text", str(part["text"])
			switch str(part["type"]) {
			case "text":
				if len(list(part["citations"])) > 0 {
					return unsupported("provider text citations")
				}
			case "tool_use":
				kind = "tool"
				raw, err := json.Marshal(part["input"])
				if err != nil {
					return err
				}
				text = string(raw)
			case "thinking":
				kind, text = "reasoning", str(part["thinking"])
			default:
				return unsupported("provider content block " + str(part["type"]))
			}
			b, err := sink.start("m:"+strconv.Itoa(i), kind, "", str(part["id"]), str(part["name"]))
			if err != nil {
				return err
			}
			if kind == "reasoning" {
				b.nativeProvider, b.signature = "claude", str(part["signature"])
			}
			if err := sink.append(b, text); err != nil {
				return err
			}
		}
	}
	return sink.finish()
}

// Errors inside a provider stream get the same redaction as HTTP errors.
func safeProtocolError(value any, account storedAccount) any {
	body, _ := json.Marshal(map[string]any{"error": value})
	return sanitizedProviderError(bytes.NewReader(body), http.StatusBadGateway, account)
}
