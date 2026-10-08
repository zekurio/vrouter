package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Provider signatures only work with their original provider. This marker
// carries that state through another client protocol. Clients must replay it
// unchanged when they continue a tool call.
const reasoningStatePrefix = "vrouter:reasoning:v1:"
const maxReasoningStateBytes = 1 << 20

type reasoningState struct {
	Provider string         `json:"provider"`
	Item     map[string]any `json:"item"`
	BlockID  string         `json:"block_id,omitempty"`
}

func encodeReasoningState(provider string, item map[string]any) (string, error) {
	data, err := json.Marshal(reasoningState{Provider: provider, Item: item})
	if err != nil || len(data) > maxReasoningStateBytes {
		return "", fmt.Errorf("Provider reasoning state exceeds 1 MiB")
	}
	return reasoningStatePrefix + base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeReasoningState(value string) (protocolBlock, error) {
	if !strings.HasPrefix(value, reasoningStatePrefix) {
		return protocolBlock{}, unsupported("opaque reasoning state from another protocol")
	}
	data := strings.TrimPrefix(value, reasoningStatePrefix)
	if len(data) > base64.RawURLEncoding.EncodedLen(maxReasoningStateBytes) {
		return protocolBlock{}, unsupported("oversized reasoning state")
	}
	raw, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		return protocolBlock{}, unsupported("invalid vrouter reasoning state")
	}
	p, err := decodeProtocolJSON(raw)
	if err != nil {
		return protocolBlock{}, unsupported("invalid vrouter reasoning state")
	}
	provider, item := str(p["provider"]), object(p["item"])
	if err := fields(p, "provider item block_id order"); err != nil {
		return protocolBlock{}, err
	}
	if item == nil {
		return protocolBlock{}, unsupported("invalid reasoning item")
	}
	b := protocolBlock{kind: "reasoning", nativeProvider: provider, native: item, projectedID: str(p["block_id"]), order: list(p["order"])}
	if p["order"] != nil && b.order == nil {
		return b, unsupported("reasoning block order")
	}
	switch provider {
	case "claude":
		if str(item["type"]) != "thinking" {
			return b, unsupported("invalid Claude reasoning state")
		}
		if err := fields(item, "type thinking signature"); err != nil {
			return b, err
		}
		b.text = str(item["thinking"])
	case "codex":
		if str(item["type"]) != "reasoning" {
			return b, unsupported("invalid Responses reasoning state")
		}
		if err := fields(item, "type id status summary encrypted_content"); err != nil {
			return b, err
		}
		b.text, err = reasoningSummary(item["summary"])
		if err != nil {
			return b, err
		}
	case "summary":
		if err := fields(item, "text"); err != nil {
			return b, err
		}
		b.text, b.nativeProvider, b.native = str(item["text"]), "", nil
	default:
		return b, unsupported("reasoning state provider " + provider)
	}
	return b, nil
}

func reasoningSummary(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	items, ok := value.([]any)
	if !ok {
		return "", unsupported("reasoning summary")
	}
	parts := []string{}
	for _, v := range items {
		part := object(v)
		if str(part["type"]) != "summary_text" {
			return "", unsupported("reasoning summary part")
		}
		if err := fields(part, "type text"); err != nil {
			return "", err
		}
		text, ok := part["text"].(string)
		if !ok {
			return "", unsupported("reasoning summary text")
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n"), nil
}

func parseResponseReasoning(item map[string]any) (protocolBlock, error) {
	if err := fields(item, "type id status summary encrypted_content"); err != nil {
		return protocolBlock{}, err
	}
	if value := str(item["encrypted_content"]); value != "" {
		return decodeReasoningState(value)
	}
	text, err := reasoningSummary(item["summary"])
	return protocolBlock{kind: "reasoning", text: text}, err
}

func prepareReplyReasoning(b *replyBlock) error {
	if b.kind != "reasoning" || b.opaque != "" {
		return nil
	}
	provider, item := b.nativeProvider, b.native
	if provider == "claude" {
		item = map[string]any{"type": "thinking", "thinking": b.text, "signature": b.signature}
	}
	if provider == "" {
		provider, item = "summary", map[string]any{"text": b.text}
	}
	data, err := json.Marshal(reasoningState{Provider: provider, Item: item, BlockID: b.id})
	if err != nil || len(data) > maxReasoningStateBytes {
		return fmt.Errorf("Provider reasoning state exceeds 1 MiB")
	}
	b.opaque = reasoningStatePrefix + base64.RawURLEncoding.EncodeToString(data)
	return nil
}

func chatReasoningWithOrder(state string, blocks []*replyBlock) string {
	if !strings.HasPrefix(state, reasoningStatePrefix) {
		return state
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(state, reasoningStatePrefix))
	if err != nil {
		return state
	}
	value, err := decodeProtocolJSON(raw)
	if err != nil {
		return state
	}
	order := []any{}
	for _, b := range blocks {
		entry := map[string]any{"kind": b.kind}
		switch b.kind {
		case "reasoning":
			entry["id"] = b.id
		case "tool":
			entry["id"] = b.callID
		default:
			entry["length"] = len(b.text)
		}
		order = append(order, entry)
	}
	value["order"] = order
	raw, err = json.Marshal(value)
	if err != nil {
		return state
	}
	return reasoningStatePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// Chat groups visible text and tool calls in separate fields. The owned
// wrapper records their original positions so provider state keeps its order.
func orderChatBlocks(message map[string]any, blocks []protocolBlock) ([]protocolBlock, error) {
	var order []any
	reasoning, tools := map[string]protocolBlock{}, map[string]protocolBlock{}
	for _, b := range blocks {
		if b.kind == "reasoning" {
			if b.order != nil {
				order = b.order
			}
			if b.projectedID != "" {
				reasoning[b.projectedID] = b
			}
		}
		if b.kind == "tool" {
			tools[b.id] = b
		}
	}
	if order == nil {
		return blocks, nil
	}
	text, refusal := str(message["content"]), str(message["refusal"])
	if message["content"] != nil {
		if _, ok := message["content"].(string); !ok {
			return nil, unsupported("ordered Chat content that is not text")
		}
	}
	out := []protocolBlock{}
	for _, value := range order {
		entry := object(value)
		if err := fields(entry, "kind id length"); err != nil {
			return nil, err
		}
		switch str(entry["kind"]) {
		case "reasoning":
			id := str(entry["id"])
			b, ok := reasoning[id]
			if !ok {
				return nil, unsupported("missing ordered reasoning block")
			}
			out = append(out, b)
			delete(reasoning, id)
		case "tool":
			id := str(entry["id"])
			b, ok := tools[id]
			if !ok {
				return nil, unsupported("missing ordered tool call")
			}
			out = append(out, b)
			delete(tools, id)
		case "text", "refusal":
			number, ok := entry["length"].(json.Number)
			if !ok {
				return nil, unsupported("reasoning text position")
			}
			n, err := number.Int64()
			source := &text
			if str(entry["kind"]) == "refusal" {
				source = &refusal
			}
			if err != nil || n < 0 || n > int64(len(*source)) {
				return nil, unsupported("reasoning text position")
			}
			out = append(out, protocolBlock{kind: "text", text: (*source)[:int(n)]})
			*source = (*source)[int(n):]
		default:
			return nil, unsupported("ordered Chat block " + str(entry["kind"]))
		}
	}
	if len(reasoning) > 0 || len(tools) > 0 || text != "" || refusal != "" {
		return nil, unsupported("incomplete reasoning block order")
	}
	return out, nil
}

func reasoningTarget(b protocolBlock, target wireProtocol) error {
	provider := "codex"
	if target == messagesProtocol {
		provider = "claude"
	}
	if b.nativeProvider != "" && b.nativeProvider != provider {
		return unsupported("signed reasoning state for " + b.nativeProvider + " in a request to " + provider)
	}
	return nil
}

// Only vrouter-owned wrappers change in a native request. Other provider
// fields keep their original values and go through without translation.
func unwrapNativeReasoning(payload map[string]any, protocol wireProtocol) error {
	if protocol == responsesProtocol {
		for _, value := range list(payload["input"]) {
			item := object(value)
			if str(item["type"]) != "reasoning" || !strings.HasPrefix(str(item["encrypted_content"]), reasoningStatePrefix) {
				continue
			}
			b, err := decodeReasoningState(str(item["encrypted_content"]))
			if err != nil {
				return err
			}
			if err := reasoningTarget(b, protocol); err != nil {
				return err
			}
			for k := range item {
				delete(item, k)
			}
			if b.native != nil {
				for k, v := range b.native {
					item[k] = v
				}
			} else {
				item["type"], item["role"], item["content"] = "message", "assistant", b.text
			}
		}
	} else if protocol == messagesProtocol {
		for _, value := range list(payload["messages"]) {
			for _, content := range list(object(value)["content"]) {
				item := object(content)
				if str(item["type"]) != "thinking" || !strings.HasPrefix(str(item["signature"]), reasoningStatePrefix) {
					continue
				}
				b, err := decodeReasoningState(str(item["signature"]))
				if err != nil {
					return err
				}
				if err := reasoningTarget(b, protocol); err != nil {
					return err
				}
				for k := range item {
					delete(item, k)
				}
				if b.native != nil {
					for k, v := range b.native {
						item[k] = v
					}
				} else {
					item["type"], item["text"] = "text", b.text
				}
			}
		}
	}
	return nil
}
