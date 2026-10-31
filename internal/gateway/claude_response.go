package gateway

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// Keep native Messages events intact, including fields unknown to our protocol
// translators. Accounting reads the upstream bytes before this name restoration.
// Each SSE event is bounded and restored before any of its bytes reach a client.
type claudeToolReader struct {
	source  io.Reader
	scanner *bufio.Scanner
	aliases map[string]string
	pending []byte
	stream  bool
	done    bool
}

func (r *claudeToolReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		var err error
		if r.stream {
			r.pending, err = r.nextEvent()
		} else {
			r.pending, err = r.readBody()
			r.done = true
		}
		if err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	if n == 0 && r.done {
		return 0, io.EOF
	}
	return n, nil
}

func (r *claudeToolReader) readBody() ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.source, (32<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 32<<20 {
		return nil, errors.New("Provider response exceeds 32 MiB")
	}
	body, err := decodeProtocolJSON(raw)
	if err != nil {
		return nil, err
	}
	if !restoreClaudeToolNames(body, r.aliases) {
		return raw, nil
	}
	return encodeClaudeBody(body)
}

func (r *claudeToolReader) nextEvent() ([]byte, error) {
	if r.scanner == nil {
		r.scanner = bufio.NewScanner(r.source)
		r.scanner.Buffer(make([]byte, 4096), 2<<20)
	}
	var event bytes.Buffer
	var data [][]byte
	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		event.Write(line)
		event.WriteByte('\n')
		if event.Len() > 2<<20 {
			return nil, errors.New("Provider SSE event exceeds 2 MiB")
		}
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.Clone(bytes.TrimPrefix(value, []byte(" "))))
		}
		if len(line) == 0 {
			return r.restoreEvent(event.Bytes(), data)
		}
	}
	if err := r.scanner.Err(); err != nil {
		return nil, errors.New("Provider stream read failed")
	}
	r.done = true
	return r.restoreEvent(event.Bytes(), data)
}

func (r *claudeToolReader) restoreEvent(event []byte, data [][]byte) ([]byte, error) {
	if len(data) == 0 {
		return event, nil
	}
	body, err := decodeProtocolJSON(bytes.Join(data, []byte("\n")))
	if err != nil {
		return nil, err
	}
	if str(body["type"]) == "message_stop" {
		r.done = true
	}
	if !restoreClaudeToolNames(body, r.aliases) {
		return event, nil
	}
	raw, err := encodeClaudeBody(body)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	replaced := false
	for line := range bytes.SplitSeq(bytes.TrimSuffix(event, []byte("\n")), []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			if replaced {
				continue
			}
			out.WriteString("data: ")
			out.Write(raw)
			replaced = true
		} else {
			out.Write(line)
		}
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}
