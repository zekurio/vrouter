package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

func claudeInferenceHeaders(req *http.Request, payload map[string]json.RawMessage, session string) {
	req.Header.Set("Anthropic-Beta", "claude-code-20250219")
	req.Header.Set("User-Agent", "claude-cli/"+claudeCLIVersion+" (external, cli)")
	req.Header.Set("X-App", "cli")
	req.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-Package-Version", "0.112.1")
	req.Header.Set("X-Stainless-Runtime", "node")
	req.Header.Set("X-Stainless-Runtime-Version", "v22.20.0")
	req.Header.Set("X-Stainless-Os", "Linux")
	req.Header.Set("X-Stainless-Arch", "x64")
	req.Header.Set("X-Stainless-Retry-Count", "0")
	req.Header.Set("X-Stainless-Timeout", "600")
	req.Header.Set("X-Claude-Code-Session-Id", session)
	for _, beta := range []string{
		"claude-code-20250219", "oauth-2025-04-20",
		"interleaved-thinking-2025-05-14", "thinking-token-count-2026-05-13",
		"context-management-2025-06-27", "prompt-caching-scope-2026-01-05",
		"extended-cache-ttl-2025-04-11",
	} {
		addAnthropicBeta(req, beta)
	}
	var model string
	_ = json.Unmarshal(payload["model"], &model)
	if claudeMidSystem(model) {
		addAnthropicBeta(req, "mid-conversation-system-2026-04-07")
		addAnthropicBeta(req, "mid-conversation-tool-changes-2026-07-01")
	}
	var thinking map[string]any
	_ = json.Unmarshal(payload["thinking"], &thinking)
	if thinking["display"] == nil {
		addAnthropicBeta(req, "redact-thinking-2026-02-12")
	}
}

// Preserve the runtime headers of a native CLI request instead of pairing its
// billing version with another client's software profile.
func forwardClaudeNativeHeaders(upstream, incoming *http.Request, payload map[string]json.RawMessage) {
	var blocks []any
	if json.Unmarshal(payload["system"], &blocks) != nil || !claudeNativeSystem(blocks) ||
		!strings.HasPrefix(incoming.Header.Get("User-Agent"), "claude-cli/") || incoming.Header.Get("X-App") != "cli" {
		return
	}
	for _, name := range []string{
		"User-Agent", "X-Stainless-Package-Version", "X-Stainless-Runtime-Version",
		"X-Stainless-Os", "X-Stainless-Arch", "X-Stainless-Timeout", "X-Stainless-Async",
		"X-Claude-Code-Agent-Id", "X-Claude-Code-Parent-Session-Id",
	} {
		if value := incoming.Header.Get(name); value != "" {
			upstream.Header.Set(name, value)
		}
	}
}
