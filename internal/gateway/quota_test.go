package gateway

import (
	"testing"
	"time"
)

func TestQuotaWindows(t *testing.T) {
	now := time.Date(2026, 10, 6, 19, 0, 0, 0, time.UTC)
	tests := []struct {
		name, provider, body string
		count                int
		firstID              string
		remaining            float64
	}{
		{"codex weekly in primary", "codex", `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":73,"limit_window_seconds":604800,"reset_at":1791583206},"secondary_window":null}}`, 1, "weekly", 27},
		{"codex both windows", "codex", `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":30,"limit_window_seconds":604800}}}`, 2, "five-hour", 90},
		{"claude both windows", "claude", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":7,"resets_at":"2026-10-09T06:00:00.395289+00:00"}}`, 2, "five-hour", 100},
		{"missing is not unused", "claude", `{"five_hour":{},"seven_day":null}`, 0, "", 0},
		{"missing codex usage", "codex", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000}}}`, 0, "", 0},
		{"overdrawn", "claude", `{"five_hour":{"utilization":105}}`, 1, "five-hour", 0},
		{"negative is invalid", "claude", `{"five_hour":{"utilization":-1}}`, 0, "", 0},
		{"scoped model", "claude", `{"limits":[{"kind":"weekly_scoped","percent":12,"scope":{"model":{"display_name":"Fable"}}}]}`, 1, "scoped-0", 88},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			windows, _, err := parseQuota(tc.provider, []byte(tc.body), now)
			if err != nil {
				t.Fatal(err)
			}
			if len(windows) != tc.count {
				t.Fatalf("windows: %+v", windows)
			}
			if tc.count > 0 && (windows[0].ID != tc.firstID || windows[0].Remaining != tc.remaining) {
				t.Fatalf("bad window: %+v", windows[0])
			}
			if tc.name == "codex both windows" && windows[0].ResetAt.Sub(now) != time.Hour {
				t.Fatal("relative reset incorrect")
			}
		})
	}
}
