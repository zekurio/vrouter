package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWindowStartSelectsSmallModel(t *testing.T) {
	model, err := windowStartModel([]Model{{ID: "claude-opus-test"}, {ID: "claude-sonnet-test"}, {ID: "claude-haiku-test"}}, "claude")
	if err != nil || model.ID != "claude-haiku-test" {
		t.Fatalf("selection: %+v %v", model, err)
	}
	if _, err := windowStartModel([]Model{{ID: "claude-opus-test"}}, "claude"); err == nil {
		t.Fatal("fell back to expensive model")
	}
	model = Model{ID: "gpt-mini", Reasoning: []string{"high", "low", "medium"}}
	if got := lowestReasoning(model); got != "low" {
		t.Fatalf("reasoning: %s", got)
	}
	payload := windowStartPayload(model, "codex")
	if payload["reasoning"].(map[string]string)["effort"] != "low" || payload["store"] != false {
		t.Fatalf("payload: %+v", payload)
	}
}

func TestWindowPlanSkipped(t *testing.T) {
	for _, c := range []struct {
		list, provider, plan string
		want                 bool
	}{
		{"", "codex", "pro", true},
		{"", "codex", "ProLite", true},
		{"", "codex", "plus", false},
		{"", "codex", "pro-next", true},
		{"claude:max*", "claude", "Max 20x", true},
		{"*", "codex", "plus", false},
		{"", "claude", "Pro", false},
		{"", "codex", "", false},
		{"none", "codex", "pro", false},
		{"openai:Pro 20x", "codex", "pro", true},
		{" team , claude:max 5x", "claude", "Max 5x", true},
		{"team", "codex", "team", true},
		{"claude:pro", "codex", "pro", false},
	} {
		if got := windowPlanSkipped(c.list, c.provider, c.plan); got != c.want {
			t.Errorf("list=%q %s/%s: got %v", c.list, c.provider, c.plan, got)
		}
	}
}

func TestStartWindow(t *testing.T) {
	for _, scenario := range []string{"idle", "active", "unsupported", "skipped-plan", "skipped-plan-missing", "missing", "unconfirmed", "rejected", "no-small-model"} {
		t.Run(scenario, func(t *testing.T) {
			s := testServer(t, Config{ExternalAuth: true, WindowSkipPlans: "claude:max"})
			addClaude(t, s, "a")
			if strings.HasPrefix(scenario, "skipped-plan") {
				if err := s.store.update(func(d *diskState) error { d.Accounts[0].Plan = "Max"; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			sent := 0
			reset := time.Now().Add(5 * time.Hour).Format(time.RFC3339)
			transport := roundTripper(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/oauth/profile":
					return jsonResponse(`{}`, 200), nil
				case "/api/oauth/usage":
					if scenario == "missing" || scenario == "skipped-plan-missing" {
						return jsonResponse(`{}`, 503), nil
					}
					if scenario == "unsupported" {
						return jsonResponse(`{"seven_day":{"utilization":0}}`, 200), nil
					}
					var boundary any
					if scenario == "active" || sent > 0 && scenario != "unconfirmed" {
						boundary = reset
					}
					raw, _ := json.Marshal(map[string]any{"five_hour": map[string]any{"utilization": 0, "resets_at": boundary}, "seven_day": map[string]any{"utilization": 0}})
					return jsonResponse(string(raw), 200), nil
				case "/v1/models":
					if scenario == "no-small-model" {
						return jsonResponse(`{"data":[{"id":"claude-opus-test"}]}`, 200), nil
					}
					return jsonResponse(`{"data":[{"id":"claude-opus-test"},{"id":"claude-haiku-test"}]}`, 200), nil
				case "/v1/messages":
					sent++
					raw, _ := io.ReadAll(r.Body)
					var payload map[string]any
					_ = json.Unmarshal(raw, &payload)
					if payload["model"] != "claude-haiku-test" || payload["max_tokens"] != float64(8) {
						t.Errorf("unexpected trigger payload: %s", raw)
					}
					if strings.Contains(string(raw), "tools") {
						t.Error("trigger enabled tools")
					}
					if scenario == "rejected" {
						return jsonResponse(`{}`, 400), nil
					}
					return jsonResponse(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`, 200), nil
				}
				t.Errorf("unexpected path %s", r.URL.Path)
				return jsonResponse(`{}`, 404), nil
			})
			s.client.Transport = transport
			s.streamClient.Transport = transport
			s.startIdleWindows(context.Background())
			s.windowMu.Lock()
			watch := s.windowWatch["a"]
			s.windowMu.Unlock()
			// Later checks must not send again while the window or backoff lasts.
			s.startIdleWindows(context.Background())
			switch scenario {
			case "idle":
				if sent != 1 || watch.lastErr != "" || time.Until(watch.next) < 4*time.Hour {
					t.Fatalf("sent=%d watch=%+v", sent, watch)
				}
			case "unsupported", "skipped-plan", "skipped-plan-missing":
				if sent != 0 || watch.lastErr != "" || time.Until(watch.next) < 30*time.Minute {
					t.Fatalf("sent=%d watch=%+v", sent, watch)
				}
			case "active":
				if sent != 0 || watch.lastErr != "" || time.Until(watch.next) < 4*time.Hour {
					t.Fatalf("active window triggered: sent=%d watch=%+v", sent, watch)
				}
			case "unconfirmed", "rejected":
				if sent != 1 || watch.lastErr == "" {
					t.Fatalf("sent=%d watch=%+v", sent, watch)
				}
				// The persisted cooldown must hold after a restart forgets the backoff.
				s.windowMu.Lock()
				delete(s.windowWatch, "a")
				s.windowMu.Unlock()
				s.startIdleWindows(context.Background())
				if sent != 1 {
					t.Fatal("trigger repeated within the cooldown")
				}
			default:
				if sent != 0 || watch.lastErr == "" {
					t.Fatalf("unsafe trigger: sent=%d watch=%+v", sent, watch)
				}
			}
		})
	}
}
