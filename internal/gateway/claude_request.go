package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
)

const claudeOAuthIdentity = "You are Claude Code, Anthropic's official CLI for Claude."

// Claude subscription inference requires its native CLI identity. Keep caller
// system blocks after that identity, with their content and order unchanged.
func claudeOAuthPayload(payload map[string]json.RawMessage, account storedAccount) (map[string]json.RawMessage, error) {
	if account.Provider != "claude" || account.AuthMode != "oauth" {
		return payload, nil
	}
	var blocks []json.RawMessage
	if system, ok := payload["system"]; ok && !bytes.Equal(bytes.TrimSpace(system), []byte("null")) {
		var text string
		if json.Unmarshal(system, &text) == nil {
			if text != "" {
				block, _ := json.Marshal(map[string]string{"type": "text", "text": text})
				blocks = []json.RawMessage{block}
			}
		} else if json.Unmarshal(system, &blocks) != nil {
			return nil, errors.New("Claude system must be text or an array of text blocks")
		}
	}
	for _, raw := range blocks {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &block) == nil && block.Type == "text" && block.Text == claudeOAuthIdentity {
			return payload, nil
		}
	}
	identity, _ := json.Marshal(map[string]string{"type": "text", "text": claudeOAuthIdentity})
	system, err := json.Marshal(append([]json.RawMessage{identity}, blocks...))
	if err != nil {
		return nil, errors.New("invalid Claude system blocks")
	}
	prepared := make(map[string]json.RawMessage, len(payload)+1)
	maps.Copy(prepared, payload)
	prepared["system"] = system
	return prepared, nil
}
