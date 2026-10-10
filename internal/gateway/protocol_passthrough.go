package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

// passthroughHoldMax bounds how much a native relay holds back while it waits
// for the end of an SSE event or a JSON body. Past it, bytes pass unchanged.
const passthroughHoldMax = 32 << 20

// passthroughScrubber relays a provider response that is already in the
// client's format. Bytes pass unchanged, except error payloads that would
// echo a credential or a stack trace, which get the same redaction as
// translated errors. SSE is released one whole event at a time and a JSON
// body once it ends, so an error is never split around the check.
type passthroughScrubber struct {
	account storedAccount
	stream  bool
	pending []byte
	raw     bool
}

// next takes provider bytes and returns the bytes ready for the client.
func (s *passthroughScrubber) next(data []byte) []byte {
	if s.raw {
		return data
	}
	s.pending = append(s.pending, data...)
	if len(s.pending) > passthroughHoldMax {
		s.raw = true
		return s.take()
	}
	if !s.stream {
		return nil
	}
	end := sseEventsEnd(s.pending)
	if end == 0 {
		return nil
	}
	out := scrubSSEEvents(s.pending[:end], s.account)
	s.pending = append([]byte(nil), s.pending[end:]...)
	return out
}

// flush returns whatever is still held back once the provider stops sending.
func (s *passthroughScrubber) flush() []byte {
	if s.raw || len(s.pending) == 0 {
		return s.take()
	}
	if s.stream {
		out := scrubSSEEvents(s.pending, s.account)
		s.pending = nil
		return out
	}
	out := scrubProviderJSON(s.pending, s.account)
	s.pending = nil
	return out
}

func (s *passthroughScrubber) take() []byte {
	out := s.pending
	s.pending = nil
	return out
}

// nextSSELine returns the line starting at start, the index after its
// terminator, and false while the line is incomplete. A trailing CR waits
// for the next byte, which may complete a CRLF.
func nextSSELine(b []byte, start int) ([]byte, int, bool) {
	for i := start; i < len(b); i++ {
		switch b[i] {
		case '\n':
			return b[start:i], i + 1, true
		case '\r':
			if i+1 == len(b) {
				return nil, start, false
			}
			if b[i+1] == '\n' {
				return b[start:i], i + 2, true
			}
			return b[start:i], i + 1, true
		}
	}
	return nil, start, false
}

// sseEventsEnd returns the index after the last complete event in b.
func sseEventsEnd(b []byte) int {
	end, pos := 0, 0
	for {
		line, next, ok := nextSSELine(b, pos)
		if !ok {
			return end
		}
		if len(line) == 0 {
			end = next
		}
		pos = next
	}
}

// scrubSSEEvents checks each event in b. A trailing event without its blank
// line, which only reaches here on flush, is checked too.
func scrubSSEEvents(b []byte, a storedAccount) []byte {
	var out bytes.Buffer
	start, pos := 0, 0
	var lines [][]byte
	for {
		line, next, ok := nextSSELine(b, pos)
		if !ok {
			break
		}
		pos = next
		if len(line) > 0 {
			lines = append(lines, line)
			continue
		}
		out.Write(scrubSSEEvent(b[start:next], lines, a))
		start, lines = next, nil
	}
	if pos < len(b) {
		lines = append(lines, bytes.TrimRight(b[pos:], "\r"))
	}
	if start < len(b) {
		out.Write(scrubSSEEvent(b[start:], lines, a))
	}
	return out.Bytes()
}

// scrubSSEEvent returns the event unchanged unless its data is a JSON error
// payload that redaction changes. Then the data lines become one line.
func scrubSSEEvent(event []byte, lines [][]byte, a storedAccount) []byte {
	prefix := []byte("data:")
	first := -1
	var data [][]byte
	for i, line := range lines {
		if value, ok := bytes.CutPrefix(line, prefix); ok {
			if first < 0 {
				first = i
			}
			data = append(data, bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	if first < 0 {
		return event
	}
	body, changed := scrubProviderPayload(bytes.Join(data, []byte("\n")), a)
	if !changed {
		return event
	}
	var out bytes.Buffer
	for i, line := range lines {
		switch {
		case i == first:
			out.WriteString("data: ")
			out.Write(body)
		case bytes.HasPrefix(line, prefix):
			continue
		default:
			out.Write(line)
		}
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	return out.Bytes()
}

// scrubProviderJSON returns a non-streaming body, redacted when it carries an
// error payload that needs it.
func scrubProviderJSON(body []byte, a storedAccount) []byte {
	if scrubbed, changed := scrubProviderPayload(body, a); changed {
		return scrubbed
	}
	return body
}

// scrubProviderPayload redacts the error objects a provider can send in a
// successful response: a top-level error, a Responses error event, and the
// error of a failed Responses object. It reports whether anything changed.
func scrubProviderPayload(data []byte, a storedAccount) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil || payload == nil {
		return nil, false
	}
	changed := false
	switch e := payload["error"].(type) {
	case map[string]any:
		changed = scrubProviderErrorObject(e, a)
	case string:
		if message := scrubProviderMessage(e, a); message != e {
			payload["error"], changed = message, true
		}
	}
	if payload["type"] == "error" && scrubProviderErrorObject(payload, a) {
		changed = true
	}
	if response, ok := payload["response"].(map[string]any); ok {
		if e, ok := response["error"].(map[string]any); ok && scrubProviderErrorObject(e, a) {
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, false
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), true
}

// scrubProviderErrorObject redacts the message and drops any other string
// field that would echo a credential, as translated errors do.
func scrubProviderErrorObject(o map[string]any, a storedAccount) bool {
	changed := false
	for key, value := range o {
		text, ok := value.(string)
		if !ok {
			continue
		}
		if key == "message" {
			if message := scrubProviderMessage(text, a); message != text {
				o[key], changed = message, true
			}
			continue
		}
		if redactProviderSecrets(text, a) != text {
			delete(o, key)
			changed = true
		}
	}
	return changed
}

// scrubProviderMessage redacts credentials and replaces HTML or stack traces.
// Other text, including its whitespace, is kept as the provider sent it.
func scrubProviderMessage(text string, a storedAccount) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	if providerMessageUnsafe(text) {
		return providerGenericMessage
	}
	return redactProviderSecrets(text, a)
}
