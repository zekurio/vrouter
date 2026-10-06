package gateway

import "time"

func percent(value float64) *float64 { return &value }

func demoState() State {
	return State{Mode: "demo", Connected: false, ObservedAt: time.Now().UTC(), Warnings: []string{},
		Accounts: []Account{
			{ID: "1", Name: "personal", Provider: "Codex", Plan: "Pro", Status: "unavailable", Remaining: percent(0), Window: "Weekly window", Reset: "2d 21h"},
			{ID: "2", Name: "work", Provider: "Codex", Plan: "Pro", Status: "unavailable", Remaining: percent(0), Window: "Weekly window", Reset: "2d 21h"},
			{ID: "3", Name: "studio", Provider: "Codex", Plan: "Pro", Status: "ready", Remaining: percent(67), Window: "Weekly window", Reset: "6d 4h"},
			{ID: "4", Name: "personal", Provider: "Claude", Plan: "Max", Status: "ready", Remaining: percent(99), Window: "Weekly window", Reset: "4d 12h"},
			{ID: "5", Name: "personal", Provider: "Grok", Plan: "Heavy", Status: "ready", Remaining: percent(96), Window: "Weekly window", Reset: "5d 8h"},
		},
		Models: []Model{
			{ID: "gpt-6-astra", Name: "GPT-6 Astra", Provider: "Codex", Context: 0},
			{ID: "gpt-6.1-sol", Name: "GPT-6.1 Sol", Provider: "Codex", Context: 0},
			{ID: "gpt-6-luna", Name: "GPT-6 Luna", Provider: "Codex", Context: 0},
			{ID: "claude-fable-5-1", Name: "Claude Fable 5.1", Provider: "Claude", Context: 0},
			{ID: "claude-opus-5-5", Name: "Claude Opus 5.5", Provider: "Claude", Context: 0},
			{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5", Provider: "Claude", Context: 0},
			{ID: "grok-4.7", Name: "Grok 4.7", Provider: "Grok", Context: 0},
			{ID: "grok-4.7-build-fast", Name: "Grok 4.7 Build Fast", Provider: "Grok", Context: 0},
		},
	}
}
