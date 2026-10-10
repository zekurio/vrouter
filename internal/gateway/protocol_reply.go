package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type replyBlock struct {
	key, kind, id, callID, name, text string
	index                             int
	done                              bool
	nativeProvider, signature, opaque string
	native                            map[string]any
}
type protocolReply struct {
	id, model, status, stop string
	blocks                  []*replyBlock
	usage                   map[string]any
	errorBody               any
	raw                     map[string]any
	nativeItems             map[int]map[string]any
	started, terminal       bool
	created                 int64
}

// Every event of one reply reports the same creation time.
func (r *protocolReply) createdAt() int64 {
	if r.created == 0 {
		r.created = time.Now().Unix()
	}
	return r.created
}

func (r *protocolReply) find(key string) *replyBlock {
	for _, b := range r.blocks {
		if b.key == key {
			return b
		}
	}
	return nil
}

func (r *protocolReply) responseItem(b *replyBlock, done bool) map[string]any {
	status := "in_progress"
	if done {
		status = "completed"
	}
	if r.status == "incomplete" && done {
		status = "incomplete"
	}
	switch b.kind {
	case "tool":
		return map[string]any{"type": "function_call", "id": b.id, "call_id": b.callID, "name": b.name, "arguments": b.text, "status": status}
	case "reasoning":
		item := map[string]any{"type": "reasoning", "id": b.id, "summary": []any{map[string]any{"type": "summary_text", "text": b.text}}}
		if b.opaque != "" {
			item["encrypted_content"] = b.opaque
		}
		return item
	default:
		content := map[string]any{"type": "output_text", "text": b.text, "annotations": []any{}}
		if b.kind == "refusal" {
			content = map[string]any{"type": "refusal", "refusal": b.text}
		}
		return map[string]any{"type": "message", "id": b.id, "role": "assistant", "status": status, "content": []any{content}}
	}
}

// counts returns input, output, cache read, cache write, and reasoning tokens.
func (r *protocolReply) counts() (int64, int64, int64, int64, int64) {
	number := func(v any) int64 {
		switch v := v.(type) {
		case json.Number:
			n, _ := v.Int64()
			return n
		case int64:
			return v
		case int:
			return int64(v)
		case float64:
			return int64(v)
		}
		return 0
	}
	input, output := number(r.usage["input_tokens"]), number(r.usage["output_tokens"])
	var cached, write, reasoning int64
	if detail := object(r.usage["input_tokens_details"]); detail != nil {
		cached = number(detail["cached_tokens"])
	}
	if detail := object(r.usage["output_tokens_details"]); detail != nil {
		reasoning = number(detail["reasoning_tokens"])
	}
	if r.usage["cache_read_input_tokens"] != nil || r.usage["cache_creation_input_tokens"] != nil {
		cached = number(r.usage["cache_read_input_tokens"])
		write = number(r.usage["cache_creation_input_tokens"])
		input += cached + write
	}
	return input, output, cached, write, reasoning
}

func (r *protocolReply) wireUsage(protocol wireProtocol) map[string]any {
	in, out, cache, write, reasoning := r.counts()
	switch protocol {
	case responsesProtocol:
		return map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": in + out, "input_tokens_details": map[string]any{"cached_tokens": cache}, "output_tokens_details": map[string]any{"reasoning_tokens": reasoning}}
	case messagesProtocol:
		return map[string]any{"input_tokens": in - cache - write, "output_tokens": out, "cache_read_input_tokens": cache, "cache_creation_input_tokens": write}
	case chatProtocol:
		fallthrough
	default:
		return map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out, "prompt_tokens_details": map[string]any{"cached_tokens": cache}, "completion_tokens_details": map[string]any{"reasoning_tokens": reasoning}}
	}
}

func (r *protocolReply) finishReason(protocol wireProtocol) string {
	if protocol == messagesProtocol {
		if r.status == "incomplete" {
			return "max_tokens"
		}
		if r.stop == "refusal" {
			return "refusal"
		}
		for _, b := range r.blocks {
			if b.kind == "tool" {
				return "tool_use"
			}
		}
		return "end_turn"
	}
	if r.status == "incomplete" {
		return "length"
	}
	for _, b := range r.blocks {
		if b.kind == "tool" {
			return "tool_calls"
		}
	}
	return "stop"
}

func (r *protocolReply) encode(protocol wireProtocol) map[string]any {
	switch protocol {
	case responsesProtocol:
		items := make([]any, 0, len(r.blocks))
		for _, b := range r.blocks {
			items = append(items, r.responseItem(b, r.terminal))
		}
		response := map[string]any{"id": r.id, "object": "response", "created_at": r.createdAt(), "model": r.model, "status": r.status, "output": items, "usage": r.wireUsage(protocol), "error": r.errorBody, "incomplete_details": nil}
		if r.status == "incomplete" {
			response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		}
		return response
	case messagesProtocol:
		content := []any{}
		for _, b := range r.blocks {
			switch b.kind {
			case "tool":
				content = append(content, map[string]any{"type": "tool_use", "id": b.callID, "name": b.name, "input": objectFromJSON(b.text)})
			case "reasoning":
				content = append(content, map[string]any{"type": "thinking", "thinking": b.text, "signature": b.opaque})
			default:
				content = append(content, map[string]any{"type": "text", "text": b.text})
			}
		}
		return map[string]any{"id": r.id, "type": "message", "role": "assistant", "model": r.model, "content": content, "stop_reason": r.finishReason(protocol), "stop_sequence": nil, "usage": r.wireUsage(protocol)}
	case chatProtocol:
		fallthrough
	default:
		message := map[string]any{"role": "assistant", "content": nil}
		text, thought, refusal := "", "", ""
		calls := []any{}
		states := []any{}
		for _, b := range r.blocks {
			switch b.kind {
			case "tool":
				calls = append(calls, map[string]any{"id": b.callID, "type": "function", "function": map[string]any{"name": b.name, "arguments": b.text}})
			case "reasoning":
				thought += b.text
				if b.opaque != "" {
					states = append(states, b.opaque)
				}
			case "refusal":
				refusal += b.text
			default:
				text += b.text
			}
		}
		if text != "" {
			message["content"] = text
		}
		if thought != "" {
			message["reasoning_content"] = thought
		}
		if len(states) > 0 {
			states[0] = chatReasoningWithOrder(str(states[0]), r.blocks)
			message["vrouter_reasoning"] = states
		}
		if refusal != "" {
			message["refusal"] = refusal
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
		return map[string]any{"id": r.id, "object": "chat.completion", "created": r.createdAt(), "model": r.model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": r.finishReason(protocol)}}, "usage": r.wireUsage(protocol)}
	}
}

type protocolEmitter struct {
	w            http.ResponseWriter
	protocol     wireProtocol
	reply        *protocolReply
	sequence     int
	includeUsage bool
}

type protocolWriteError struct{ error }

func (e *protocolEmitter) event(name string, body any) error {
	if m := object(body); m != nil && e.protocol == responsesProtocol {
		m["sequence_number"] = e.sequence
		e.sequence++
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	frame := "data: " + string(data) + "\n\n"
	if e.protocol != chatProtocol {
		frame = "event: " + name + "\n" + frame
	}
	if _, err := fmt.Fprint(e.w, frame); err != nil {
		return protocolWriteError{err}
	}
	if err := http.NewResponseController(e.w).Flush(); err != nil {
		return protocolWriteError{err}
	}
	return nil
}

func (e *protocolEmitter) chat(delta map[string]any, finish any, usage bool) error {
	choices := []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
	if usage {
		choices = []any{}
	}
	body := map[string]any{"id": e.reply.id, "object": "chat.completion.chunk", "created": e.reply.createdAt(), "model": e.reply.model, "choices": choices}
	if usage {
		body["usage"] = e.reply.wireUsage(chatProtocol)
	}
	return e.event("", body)
}

func (e *protocolEmitter) begin() error {
	if e.reply.started {
		return nil
	}
	e.reply.started = true
	if e.reply.id == "" {
		id, _ := secureID()
		e.reply.id = "resp_" + id
	}
	if e.reply.status == "" {
		e.reply.status = "in_progress"
	}
	switch e.protocol {
	case responsesProtocol:
		if err := e.event("response.created", map[string]any{"type": "response.created", "response": e.reply.encode(responsesProtocol)}); err != nil {
			return err
		}
		return e.event("response.in_progress", map[string]any{"type": "response.in_progress", "response": e.reply.encode(responsesProtocol)})
	case messagesProtocol:
		message := e.reply.encode(messagesProtocol)
		message["stop_reason"] = nil
		return e.event("message_start", map[string]any{"type": "message_start", "message": message})
	case chatProtocol:
		fallthrough
	default:
		return e.chat(map[string]any{"role": "assistant", "content": ""}, nil, false)
	}
}

func (e *protocolEmitter) start(b *replyBlock) error {
	if err := e.begin(); err != nil {
		return err
	}
	switch e.protocol {
	case responsesProtocol:
		item := e.reply.responseItem(b, false)
		if b.kind == "reasoning" {
			item["summary"] = []any{}
		} else if b.kind != "tool" {
			item["content"] = []any{}
		}
		if err := e.event("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": b.index, "item": item}); err != nil {
			return err
		}
		if b.kind == "tool" {
			return nil
		}
		if b.kind == "reasoning" {
			return e.event("response.reasoning_summary_part.added", map[string]any{"type": "response.reasoning_summary_part.added", "item_id": b.id, "output_index": b.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
		}
		part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
		if b.kind == "refusal" {
			part = map[string]any{"type": "refusal", "refusal": ""}
		}
		return e.event("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": b.id, "output_index": b.index, "content_index": 0, "part": part})
	case messagesProtocol:
		block := map[string]any{"type": "text", "text": ""}
		if b.kind == "tool" {
			block = map[string]any{"type": "tool_use", "id": b.callID, "name": b.name, "input": map[string]any{}}
		}
		if b.kind == "reasoning" {
			block = map[string]any{"type": "thinking", "thinking": ""}
		}
		return e.event("content_block_start", map[string]any{"type": "content_block_start", "index": b.index, "content_block": block})
	case chatProtocol:
		fallthrough
	default:
		if b.kind == "tool" {
			return e.chat(map[string]any{"tool_calls": []any{map[string]any{"index": e.toolIndex(b), "id": b.callID, "type": "function", "function": map[string]any{"name": b.name, "arguments": ""}}}}, nil, false)
		}
		return nil
	}
}

func (e *protocolEmitter) toolIndex(b *replyBlock) int {
	n := 0
	for _, item := range e.reply.blocks {
		if item == b {
			return n
		}
		if item.kind == "tool" {
			n++
		}
	}
	return n
}

func (e *protocolEmitter) delta(b *replyBlock, delta string) error {
	if delta == "" {
		return nil
	}
	switch e.protocol {
	case responsesProtocol:
		name := "response.output_text.delta"
		field := "content_index"
		if b.kind == "tool" {
			name = "response.function_call_arguments.delta"
			field = ""
		}
		if b.kind == "reasoning" {
			name = "response.reasoning_summary_text.delta"
			field = "summary_index"
		}
		if b.kind == "refusal" {
			name = "response.refusal.delta"
		}
		body := map[string]any{"type": name, "item_id": b.id, "output_index": b.index, "delta": delta}
		if field != "" {
			body[field] = 0
		}
		return e.event(name, body)
	case messagesProtocol:
		d := map[string]any{"type": "text_delta", "text": delta}
		if b.kind == "tool" {
			d = map[string]any{"type": "input_json_delta", "partial_json": delta}
		}
		if b.kind == "reasoning" {
			d = map[string]any{"type": "thinking_delta", "thinking": delta}
		}
		return e.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": b.index, "delta": d})
	case chatProtocol:
		fallthrough
	default:
		d := map[string]any{"content": delta}
		if b.kind == "tool" {
			d = map[string]any{"tool_calls": []any{map[string]any{"index": e.toolIndex(b), "function": map[string]any{"arguments": delta}}}}
		}
		if b.kind == "reasoning" {
			d = map[string]any{"reasoning_content": delta}
		}
		if b.kind == "refusal" {
			d = map[string]any{"refusal": delta}
		}
		return e.chat(d, nil, false)
	}
}

func (e *protocolEmitter) stop(b *replyBlock) error {
	if b.done {
		return nil
	}
	if err := prepareReplyReasoning(b); err != nil {
		return err
	}
	b.done = true
	switch e.protocol {
	case responsesProtocol:
		name, value, field := "response.output_text.done", "text", "content_index"
		if b.kind == "tool" {
			name, value, field = "response.function_call_arguments.done", "arguments", ""
		}
		if b.kind == "reasoning" {
			name, field = "response.reasoning_summary_text.done", "summary_index"
		}
		if b.kind == "refusal" {
			name, value = "response.refusal.done", "refusal"
		}
		body := map[string]any{"type": name, "item_id": b.id, "output_index": b.index, value: b.text}
		if field != "" {
			body[field] = 0
		}
		if err := e.event(name, body); err != nil {
			return err
		}
		if b.kind == "reasoning" {
			if err := e.event("response.reasoning_summary_part.done", map[string]any{"type": "response.reasoning_summary_part.done", "item_id": b.id, "output_index": b.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": b.text}}); err != nil {
				return err
			}
		} else if b.kind != "tool" {
			if err := e.event("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": b.id, "output_index": b.index, "content_index": 0, "part": list(e.reply.responseItem(b, true)["content"])[0]}); err != nil {
				return err
			}
		}
		return e.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": b.index, "item": e.reply.responseItem(b, true)})
	case messagesProtocol:
		if b.kind == "reasoning" {
			if err := e.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": b.index, "delta": map[string]any{"type": "signature_delta", "signature": b.opaque}}); err != nil {
				return err
			}
		}
		return e.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.index})
	case chatProtocol:
		if b.kind == "reasoning" && b.opaque != "" {
			return e.chat(map[string]any{"vrouter_reasoning": []any{b.opaque}}, nil, false)
		}
	}
	return nil
}

func (e *protocolEmitter) finish() error {
	if err := e.begin(); err != nil {
		return err
	}
	for _, b := range e.reply.blocks {
		if err := e.stop(b); err != nil {
			return err
		}
	}
	switch e.protocol {
	case responsesProtocol:
		name := "response.completed"
		if e.reply.status == "incomplete" {
			name = "response.incomplete"
		}
		if e.reply.errorBody != nil {
			name = "response.failed"
		}
		return e.event(name, map[string]any{"type": name, "response": e.reply.encode(responsesProtocol)})
	case messagesProtocol:
		if e.reply.errorBody != nil {
			return e.event("error", map[string]any{"type": "error", "error": e.reply.errorBody})
		}
		if err := e.event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": e.reply.finishReason(messagesProtocol), "stop_sequence": nil}, "usage": e.reply.wireUsage(messagesProtocol)}); err != nil {
			return err
		}
		return e.event("message_stop", map[string]any{"type": "message_stop"})
	case chatProtocol:
		fallthrough
	default:
		if e.reply.errorBody != nil {
			return e.event("", map[string]any{"error": e.reply.errorBody})
		}
		for _, b := range e.reply.blocks {
			if b.kind == "reasoning" && b.opaque != "" {
				if err := e.chat(map[string]any{"vrouter_reasoning": []any{chatReasoningWithOrder(b.opaque, e.reply.blocks)}}, nil, false); err != nil {
					return err
				}
				break
			}
		}
		if err := e.chat(map[string]any{}, e.reply.finishReason(chatProtocol), false); err != nil {
			return err
		}
		if e.includeUsage {
			if err := e.chat(map[string]any{}, nil, true); err != nil {
				return err
			}
		}
		_, err := fmt.Fprint(e.w, "data: [DONE]\n\n")
		if err != nil {
			return protocolWriteError{err}
		}
		if err := http.NewResponseController(e.w).Flush(); err != nil {
			return protocolWriteError{err}
		}
		return nil
	}
}

// A sink keeps the assembled reply and sends each delta as it arrives.
type replySink struct {
	reply   *protocolReply
	emitter *protocolEmitter
	native  bool
	account storedAccount
}

func (s *replySink) begin() error {
	if s.emitter != nil {
		return s.emitter.begin()
	}
	if s.reply.id == "" {
		id, _ := secureID()
		s.reply.id = "resp_" + id
	}
	return nil
}

func (s *replySink) start(key, kind, id, callID, name string) (*replyBlock, error) {
	if b := s.reply.find(key); b != nil {
		return b, nil
	}
	if id == "" {
		id, _ = secureID()
		id = "item_" + id
	}
	for _, b := range s.reply.blocks {
		if b.id == id {
			id += "_" + strconv.Itoa(len(s.reply.blocks))
			break
		}
	}
	b := &replyBlock{key: key, kind: kind, id: id, callID: callID, name: name, index: len(s.reply.blocks)}
	if err := s.begin(); err != nil {
		return nil, err
	}
	s.reply.blocks = append(s.reply.blocks, b)
	if s.emitter != nil {
		if err := s.emitter.start(b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (s *replySink) append(b *replyBlock, text string) error {
	if b == nil {
		return errors.New("Provider sent a delta without a content block")
	}
	if b.done {
		return errors.New("Provider sent a delta after its content block ended")
	}
	if len(b.text)+len(text) > 16<<20 {
		return errors.New("Provider output exceeds 16 MiB per content block")
	}
	b.text += text
	if s.emitter != nil {
		return s.emitter.delta(b, text)
	}
	return nil
}

func (s *replySink) full(b *replyBlock, text string) error {
	if text == b.text {
		return nil
	}
	if !strings.HasPrefix(text, b.text) {
		return errors.New("Provider final content does not match its stream")
	}
	return s.append(b, strings.TrimPrefix(text, b.text))
}

func (s *replySink) stop(b *replyBlock) error {
	if b == nil {
		return nil
	}
	if err := prepareReplyReasoning(b); err != nil {
		return err
	}
	if s.emitter != nil {
		return s.emitter.stop(b)
	}
	b.done = true
	return nil
}

func (s *replySink) finish() error {
	s.reply.terminal = true
	if s.reply.errorBody != nil {
		s.reply.errorBody = safeProtocolError(s.reply.errorBody, s.account)
	}
	for _, b := range s.reply.blocks {
		if err := prepareReplyReasoning(b); err != nil {
			return err
		}
		if b.kind == "tool" && objectFromJSON(b.text) == nil && s.reply.status == "completed" {
			return errors.New("Provider returned invalid tool arguments")
		}
	}
	if s.emitter != nil {
		return s.emitter.finish()
	}
	return nil
}
