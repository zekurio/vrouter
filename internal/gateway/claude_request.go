package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

const (
	claudeOAuthIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	claudeCLIVersion    = "2.1.291"
)

// Adapt only subscription inference. Caller instructions stay intact, but must
// not share the top-level system slot with the native CLI identity.
func claudeOAuthPayload(payload map[string]json.RawMessage, account storedAccount, session string) (map[string]json.RawMessage, map[string]string, error) {
	if account.Provider != "claude" || account.AuthMode != "oauth" {
		return payload, nil, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	body, err := decodeProtocolJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	blocks, err := claudeSystemBlocks(body["system"])
	if err != nil {
		return nil, nil, err
	}
	native := claudeNativeSystem(blocks)
	var aliases map[string]string
	if !native {
		messages := list(body["messages"])
		billing := claudeBillingHeader(messages)
		if err := relocateClaudeSystem(body, messages, blocks); err != nil {
			return nil, nil, err
		}
		body["system"] = []any{
			map[string]any{"type": "text", "text": billing},
			map[string]any{"type": "text", "text": claudeOAuthIdentity},
		}
		aliases = aliasClaudeTools(body, session)
	}
	if err := setClaudeIdentity(body, account, session, native); err != nil {
		return nil, nil, err
	}
	raw, err = encodeClaudeBody(body)
	if err != nil {
		return nil, nil, err
	}
	var prepared map[string]json.RawMessage
	if err := json.Unmarshal(raw, &prepared); err != nil {
		return nil, nil, err
	}
	return prepared, aliases, nil
}

func claudeSystemBlocks(system any) ([]any, error) {
	if system == nil {
		return nil, nil
	}
	if text, ok := system.(string); ok {
		if text == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	blocks, ok := system.([]any)
	if !ok {
		return nil, errors.New("Claude system must be text or an array of text blocks")
	}
	for _, block := range blocks {
		b := object(block)
		if str(b["type"]) != "text" {
			return nil, errors.New("Claude system must contain text blocks")
		}
		if _, ok := b["text"].(string); !ok {
			return nil, errors.New("Claude system blocks must contain text")
		}
	}
	return blocks, nil
}

func claudeNativeSystem(blocks []any) bool {
	identity, billing := false, false
	for _, block := range blocks {
		text := str(object(block)["text"])
		identity = identity || text == claudeOAuthIdentity
		billing = billing || strings.HasPrefix(text, "x-anthropic-billing-header:")
	}
	return identity && billing
}

func claudeMidSystem(model string) bool {
	return !strings.Contains(model, "-3") && !strings.Contains(model, "-4")
}

func relocateClaudeSystem(body map[string]any, messages, blocks []any) error {
	if len(blocks) == 0 {
		return nil
	}
	first := -1
	for i, message := range messages {
		if str(object(message)["role"]) == "user" {
			first = i
			break
		}
	}
	if first < 0 {
		return errors.New("Claude subscription requests with system instructions need a user message")
	}
	if claudeMidSystem(str(body["model"])) {
		insert := first + 1
		for insert < len(messages) && str(object(messages[insert])["role"]) == "user" {
			insert++
		}
		out := append([]any(nil), messages[:insert]...)
		out = append(out, map[string]any{"role": "system", "content": blocks})
		body["messages"] = append(out, messages[insert:]...)
		return nil
	}
	message := object(messages[first])
	var content []any
	for _, block := range blocks {
		b := object(block)
		b["text"] = "<system-reminder>\n" + str(b["text"]) + "\n</system-reminder>"
		content = append(content, b)
	}
	if text, ok := message["content"].(string); ok {
		content = append(content, map[string]any{"type": "text", "text": text})
	} else {
		content = append(content, list(message["content"])...)
	}
	message["content"] = content
	return nil
}

func claudeBillingHeader(messages []any) string {
	text := ""
	for _, message := range messages {
		m := object(message)
		if str(m["role"]) != "user" {
			continue
		}
		text = str(m["content"])
		for _, block := range list(m["content"]) {
			if b := object(block); str(b["type"]) == "text" {
				text = str(b["text"])
				break
			}
		}
		break
	}
	units := utf16.Encode([]rune(text))
	sample := []uint16{'0', '0', '0'}
	for i, index := range [...]int{4, 7, 20} {
		if index < len(units) {
			sample[i] = units[index]
		}
	}
	sum := sha256.Sum256([]byte("59cf53e54c78" + string(utf16.Decode(sample)) + claudeCLIVersion))
	return "x-anthropic-billing-header: cc_version=" + claudeCLIVersion + "." + hex.EncodeToString(sum[:])[:3] + "; cc_entrypoint=cli; cch=00000;"
}

// Session IDs depend on the caller and conversation, never the selected account
// or the current date. Account rotation and restarts must not rewrite history.
func claudeSession(payload map[string]json.RawMessage, scope, explicit string) string {
	var messages []any
	_ = json.Unmarshal(payload["messages"], &messages)
	var metadata map[string]any
	_ = json.Unmarshal(payload["metadata"], &metadata)
	var identity map[string]any
	_ = json.Unmarshal([]byte(str(metadata["user_id"])), &identity)
	if explicit != "" {
		return claudeUUID(explicit)
	}
	if session := str(identity["session_id"]); session != "" {
		return claudeUUID(session)
	}
	if _, session, ok := strings.Cut(str(metadata["user_id"]), "_session_"); ok && session != "" {
		return claudeUUID(session)
	}
	seed := []any{scope, metadata["user_id"], codexSessionAnchor(messages), payload["system"]}
	raw, err := json.Marshal(seed)
	if err != nil {
		return claudeUUID(scope)
	}
	return claudeUUID(string(raw))
}

func claudeUUID(seed string) string {
	if len(seed) == 36 && seed[8] == '-' && seed[13] == '-' && seed[18] == '-' && seed[23] == '-' {
		if _, err := hex.DecodeString(strings.ReplaceAll(seed, "-", "")); err == nil {
			return seed
		}
	}
	sum := sha256.Sum256([]byte(seed))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func setClaudeIdentity(body map[string]any, account storedAccount, session string, native bool) error {
	metadata := object(body["metadata"])
	if metadata == nil {
		metadata = map[string]any{}
	}
	identity := map[string]any{}
	if native {
		_ = json.Unmarshal([]byte(str(metadata["user_id"])), &identity)
	}
	if identity == nil {
		identity = map[string]any{}
	}
	device := sha256.Sum256([]byte("vrouter/claude/device/" + account.ID))
	if str(identity["device_id"]) == "" {
		identity["device_id"] = hex.EncodeToString(device[:])
	}
	identity["account_uuid"], identity["session_id"] = account.AccountID, session
	raw, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	metadata["user_id"] = string(raw)
	body["metadata"] = metadata
	return nil
}

func encodeClaudeBody(body any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}
