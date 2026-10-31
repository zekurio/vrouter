package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pierrec/xxHash/xxHash64"
)

// CCH hashes the final canonical wire body with its own five digits zeroed.
// Model selectors and dispatch limits are excluded by the native CLI protocol.
func signClaudeBody(payload map[string]json.RawMessage) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	body, err := decodeProtocolJSON(raw)
	if err != nil {
		return nil, err
	}
	billing := claudeBillingBlock(body)
	if billing == nil {
		return nil, errors.New("Claude subscription request has no billing block")
	}
	text := str(billing["text"])
	start := strings.Index(text, " cch=")
	if start < 0 {
		text += " cch=00000;"
		start = strings.Index(text, " cch=")
	}
	start += len(" cch=")
	end := strings.IndexByte(text[start:], ';')
	if end < 0 {
		return nil, errors.New("invalid Claude billing checksum")
	}
	text = text[:start] + "00000" + text[start+end:]
	billing["text"] = text
	normalized, err := encodeClaudeBody(claudeHashView(body))
	if err != nil {
		return nil, err
	}
	hasher := xxHash64.New(0x4D659218E32A3268)
	if _, err := hasher.Write(normalized); err != nil {
		return nil, err
	}
	billing["text"] = text[:start] + fmt.Sprintf("%05x", hasher.Sum64()&0xfffff) + text[start+5:]
	return encodeClaudeBody(body)
}

func claudeBillingBlock(body map[string]any) map[string]any {
	for _, block := range list(body["system"]) {
		if b := object(block); strings.HasPrefix(str(b["text"]), "x-anthropic-billing-header:") {
			return b
		}
	}
	return nil
}

func claudeHashView(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			switch key {
			case "max_tokens", "fallbacks", "fallback_credit_token":
				continue
			case "model":
				if _, ok := item.(string); ok {
					out[key] = ""
					continue
				}
			}
			out[key] = claudeHashView(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = claudeHashView(item)
		}
		return out
	default:
		return value
	}
}
