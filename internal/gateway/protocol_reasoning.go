package gateway

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"strings"
)

// Provider signatures only work with their original provider. This marker
// carries that state through another client protocol. Clients must replay it
// unchanged when they continue a tool call.
const (
	reasoningStatePrefix   = "vrouter:reasoning:v1:"
	maxReasoningStateBytes = 1 << 20
)

type reasoningState struct {
	Provider string         `json:"provider"`
	Item     map[string]any `json:"item"`
	BlockID  string         `json:"block_id,omitempty"`
}

func encodeReasoningState(provider string, item map[string]any, blockID string) (string, error) {
	data, err := json.Marshal(reasoningState{Provider: provider, Item: item, BlockID: blockID})
	if err != nil || len(data) > maxReasoningStateBytes {
		return "", errors.New("Provider reasoning state exceeds 1 MiB")
	}
	return reasoningStatePrefix + base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeReasoningState(value string) (protocolBlock, error) {
	p, err := decodeReasoningPayload(value)
	if err != nil {
		return protocolBlock{}, err
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
	err = decodeReasoningItem(&b, provider, item)
	return b, err
}

func decodeReasoningPayload(value string) (map[string]any, error) {
	if !strings.HasPrefix(value, reasoningStatePrefix) {
		return nil, unsupported("opaque reasoning state from another protocol")
	}
	data := strings.TrimPrefix(value, reasoningStatePrefix)
	if len(data) > base64.RawURLEncoding.EncodedLen(maxReasoningStateBytes) {
		return nil, unsupported("oversized reasoning state")
	}
	raw, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		return nil, unsupported("invalid vrouter reasoning state")
	}
	p, err := decodeProtocolJSON(raw)
	if err != nil {
		return nil, unsupported("invalid vrouter reasoning state")
	}
	return p, nil
}

// decodeReasoningItem validates the wrapped provider item and sets the block
// text. A summary has no provider state.
func decodeReasoningItem(b *protocolBlock, provider string, item map[string]any) error {
	switch provider {
	case "claude":
		if str(item["type"]) != "thinking" {
			return unsupported("invalid Claude reasoning state")
		}
		if err := fields(item, "type thinking signature"); err != nil {
			return err
		}
		b.text = str(item["thinking"])
	case "codex":
		if str(item["type"]) != "reasoning" {
			return unsupported("invalid Responses reasoning state")
		}
		if err := validateResponseReasoningContent(item); err != nil {
			return err
		}
		var err error
		b.text, err = reasoningSummary(item["summary"])
		return err
	case "summary":
		if err := fields(item, "text"); err != nil {
			return err
		}
		b.text, b.nativeProvider, b.native = str(item["text"]), "", nil
	default:
		return unsupported("reasoning state provider " + provider)
	}
	return nil
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
	if err := validateResponseReasoningContent(item); err != nil {
		return protocolBlock{}, err
	}
	if value := str(item["encrypted_content"]); value != "" {
		return decodeReasoningState(value)
	}
	text, err := reasoningSummary(item["summary"])
	if err == nil {
		for _, value := range list(item["content"]) {
			if text != "" {
				text += "\n\n"
			}
			text += str(object(value)["text"])
		}
	}
	return protocolBlock{kind: "reasoning", text: text}, err
}

func validateResponseReasoningContent(item map[string]any) error {
	if err := fields(item, "type id status summary encrypted_content content"); err != nil {
		return err
	}
	if item["content"] == nil {
		return nil
	}
	content, ok := item["content"].([]any)
	if !ok {
		return unsupported("reasoning content")
	}
	for _, value := range content {
		part := object(value)
		if part == nil {
			return unsupported("reasoning content part")
		}
		if err := fields(part, "type text"); err != nil {
			return err
		}
		if part["type"] != "reasoning_text" {
			return unsupported("reasoning content part")
		}
		if _, ok := part["text"].(string); !ok {
			return unsupported("reasoning content text")
		}
	}
	return nil
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
	var err error
	b.opaque, err = encodeReasoningState(provider, item, b.id)
	return err
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
	o := chatBlockOrder{reasoning: map[string]protocolBlock{}, tools: map[string]protocolBlock{}}
	for _, b := range blocks {
		if b.kind == "reasoning" {
			if b.order != nil {
				order = b.order
			}
			if b.projectedID != "" {
				o.reasoning[b.projectedID] = b
			}
		}
		if b.kind == "tool" {
			o.tools[b.id] = b
		}
	}
	if order == nil {
		return blocks, nil
	}
	o.text, o.refusal = str(message["content"]), str(message["refusal"])
	if message["content"] != nil {
		if _, ok := message["content"].(string); !ok {
			return nil, unsupported("ordered Chat content that is not text")
		}
	}
	out := []protocolBlock{}
	for _, value := range order {
		b, err := o.next(object(value))
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if len(o.reasoning) > 0 || len(o.tools) > 0 || o.text != "" || o.refusal != "" {
		return nil, unsupported("incomplete reasoning block order")
	}
	return out, nil
}

// chatBlockOrder holds the Chat blocks and text not yet placed in order.
type chatBlockOrder struct {
	reasoning, tools map[string]protocolBlock
	text, refusal    string
}

func (o *chatBlockOrder) next(entry map[string]any) (protocolBlock, error) {
	if err := fields(entry, "kind id length"); err != nil {
		return protocolBlock{}, err
	}
	switch str(entry["kind"]) {
	case "reasoning":
		return takeChatBlock(o.reasoning, str(entry["id"]), "missing ordered reasoning block")
	case "tool":
		return takeChatBlock(o.tools, str(entry["id"]), "missing ordered tool call")
	case "text":
		return cutChatText(&o.text, entry["length"])
	case "refusal":
		return cutChatText(&o.refusal, entry["length"])
	default:
		return protocolBlock{}, unsupported("ordered Chat block " + str(entry["kind"]))
	}
}

func takeChatBlock(blocks map[string]protocolBlock, id, missing string) (protocolBlock, error) {
	b, ok := blocks[id]
	if !ok {
		return protocolBlock{}, unsupported(missing)
	}
	delete(blocks, id)
	return b, nil
}

func cutChatText(source *string, length any) (protocolBlock, error) {
	number, ok := length.(json.Number)
	if !ok {
		return protocolBlock{}, unsupported("reasoning text position")
	}
	n, err := number.Int64()
	if err != nil || n < 0 || n > int64(len(*source)) {
		return protocolBlock{}, unsupported("reasoning text position")
	}
	b := protocolBlock{kind: "text", text: (*source)[:int(n)]}
	*source = (*source)[int(n):]
	return b, nil
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
			if err := unwrapReasoningState(object(value), "reasoning", "encrypted_content", protocol); err != nil {
				return err
			}
		}
	} else if protocol == messagesProtocol {
		for _, value := range list(payload["messages"]) {
			for _, content := range list(object(value)["content"]) {
				if err := unwrapReasoningState(object(content), "thinking", "signature", protocol); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// unwrapReasoningState replaces an item of the given type whose field holds
// a vrouter wrapper with the provider item, or with plain text for a summary.
func unwrapReasoningState(item map[string]any, kind, field string, protocol wireProtocol) error {
	if str(item["type"]) != kind || !strings.HasPrefix(str(item[field]), reasoningStatePrefix) {
		return nil
	}
	b, err := decodeReasoningState(str(item[field]))
	if err != nil {
		return err
	}
	if err := reasoningTarget(b, protocol); err != nil {
		return err
	}
	clear(item)
	switch {
	case b.native != nil:
		maps.Copy(item, b.native)
	case protocol == responsesProtocol:
		item["type"], item["role"], item["content"] = "message", "assistant", b.text
	default:
		item["type"], item["text"] = "text", b.text
	}
	return nil
}
