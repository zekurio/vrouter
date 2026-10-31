package gateway

import (
	"crypto/sha256"
	"encoding/hex"
)

// Alias all custom tools, including historical tools no longer declared on this
// turn. Only protocol name fields change; schemas, arguments and text do not.
func aliasClaudeTools(body map[string]any, session string) map[string]string {
	forward, reverse := map[string]string{}, map[string]string{}
	protected := map[string]bool{}
	for _, tool := range list(body["tools"]) {
		if t := object(tool); !customClaudeTool(t) {
			protected[str(t["name"])] = true
		}
	}
	alias := func(o map[string]any, key string) bool {
		name := str(o[key])
		if name == "" || protected[name] {
			return false
		}
		renamed := forward[name]
		if renamed == "" {
			sum := sha256.Sum256([]byte(session + "\x00" + name))
			renamed = "mcp__vrouter__" + hex.EncodeToString(sum[:20])
			forward[name], reverse[renamed] = renamed, name
		}
		o[key] = renamed
		return true
	}
	walkClaudeToolNames(body, alias)
	return reverse
}

func customClaudeTool(tool map[string]any) bool {
	kind := str(tool["type"])
	return kind == "" || kind == "custom"
}

func walkClaudeToolNames(body map[string]any, rename func(map[string]any, string) bool) bool {
	changed := false
	for _, tool := range list(body["tools"]) {
		if t := object(tool); customClaudeTool(t) && rename(t, "name") {
			changed = true
		}
	}
	if choice := object(body["tool_choice"]); str(choice["type"]) == "tool" && rename(choice, "name") {
		changed = true
	}
	for _, message := range list(body["messages"]) {
		if walkClaudeContent(list(object(message)["content"]), rename) {
			changed = true
		}
	}
	if walkClaudeContent(list(object(body["message"])["content"]), rename) {
		changed = true
	}
	if walkClaudeContent(list(body["content"]), rename) {
		changed = true
	}
	if block := object(body["content_block"]); block != nil && walkClaudeContent([]any{block}, rename) {
		changed = true
	}
	return changed
}

func walkClaudeContent(blocks []any, rename func(map[string]any, string) bool) bool {
	changed := false
	for _, block := range blocks {
		b := object(block)
		switch str(b["type"]) {
		case "tool_use":
			changed = rename(b, "name") || changed
		case "tool_reference":
			changed = rename(b, "tool_name") || changed
		case "tool_result":
			changed = walkClaudeContent(list(b["content"]), rename) || changed
		case "tool_search_tool_result":
			changed = walkClaudeContent(list(object(b["content"])["tool_references"]), rename) || changed
		case "tool_addition", "tool_removal":
			changed = renameClaudeToolChange(b, rename) || changed
		}
	}
	return changed
}

func restoreClaudeToolNames(body map[string]any, aliases map[string]string) bool {
	return walkClaudeToolNames(body, func(o map[string]any, key string) bool {
		if name, ok := aliases[str(o[key])]; ok {
			o[key] = name
			return true
		}
		return false
	})
}

func renameClaudeToolChange(block map[string]any, rename func(map[string]any, string) bool) bool {
	tool := object(block["tool"])
	switch str(tool["type"]) {
	case "tool_reference":
		return rename(tool, "name")
	case "tool_definition":
		definition := object(tool["definition"])
		return customClaudeTool(definition) && rename(definition, "name")
	}
	return false
}
