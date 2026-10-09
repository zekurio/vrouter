package gateway

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

type QuotaWindow struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Remaining float64    `json:"remaining"`
	ResetAt   *time.Time `json:"resetAt,omitempty"`
	used      *float64
}

type quotaCache struct {
	ReportedWindows []QuotaWindow
	Windows         []QuotaWindow
	Plan            string
	ObservedAt      time.Time
	Error           string
	Allowed         *bool
	Resets          *resetStatus
}

func parseQuota(provider string, body []byte, now time.Time) ([]QuotaWindow, string, error) {
	windows := []QuotaWindow{}
	add := func(id, label string, used *float64, reset *time.Time) {
		if used == nil || math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0 {
			return
		}
		remaining := math.Round(math.Max(0, 100-*used)*10) / 10
		windows = append(windows, QuotaWindow{ID: id, Label: label, Remaining: remaining, ResetAt: reset, used: used})
	}
	if provider == "codex" {
		type window struct {
			Used    *float64 `json:"used_percent"`
			Seconds int      `json:"limit_window_seconds"`
			Reset   int64    `json:"reset_at"`
			After   *int64   `json:"reset_after_seconds"`
		}
		type limit struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		}
		var data struct {
			Plan       string `json:"plan_type"`
			Limit      limit  `json:"rate_limit"`
			Review     limit  `json:"code_review_rate_limit"`
			Additional []struct {
				Name  string `json:"limit_name"`
				Limit limit  `json:"rate_limit"`
			} `json:"additional_rate_limits"`
		}
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, "", err
		}
		appendLimit := func(l limit, prefix, name string) {
			for i, w := range []*window{l.Primary, l.Secondary} {
				if w == nil {
					continue
				}
				id, label := fmt.Sprintf("window-%d", i+1), fmt.Sprintf("Window %d", i+1)
				switch {
				case w.Seconds == 18000:
					id, label = "five-hour", "5-hour window"
				case w.Seconds == 604800:
					id, label = "weekly", "Weekly window"
				case w.Seconds >= 2419200 && w.Seconds <= 2678400:
					id, label = "monthly", "Monthly window"
				}
				var reset *time.Time
				if w.Reset > 0 {
					t := time.Unix(w.Reset, 0).UTC()
					reset = &t
				} else if w.After != nil && *w.After >= 0 {
					t := now.Add(time.Duration(*w.After) * time.Second)
					reset = &t
				}
				add(prefix+id, name+label, w.Used, reset)
			}
		}
		appendLimit(data.Limit, "", "")
		appendLimit(data.Review, "review-", "Code review · ")
		for i, extra := range data.Additional {
			appendLimit(extra.Limit, fmt.Sprintf("extra-%d-", i), extra.Name+" · ")
		}
		return windows, data.Plan, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "", err
	}
	for _, meta := range []struct{ key, id, label string }{{"five_hour", "five-hour", "5-hour window"}, {"seven_day", "weekly", "Weekly window"}, {"seven_day_opus", "opus", "Opus weekly"}, {"seven_day_sonnet", "sonnet", "Sonnet weekly"}, {"seven_day_oauth_apps", "oauth-apps", "OAuth apps weekly"}, {"seven_day_cowork", "cowork", "Cowork weekly"}} {
		var w struct {
			Used  *float64   `json:"utilization"`
			Reset *time.Time `json:"resets_at"`
		}
		if raw := data[meta.key]; len(raw) > 0 && json.Unmarshal(raw, &w) == nil {
			add(meta.id, meta.label, w.Used, w.Reset)
		}
	}
	// New Claude responses also report explicitly named scoped limits.
	var limits []struct {
		Kind  string     `json:"kind"`
		Used  *float64   `json:"percent"`
		Reset *time.Time `json:"resets_at"`
		Scope struct {
			Model struct {
				Name string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	}
	if json.Unmarshal(data["limits"], &limits) == nil {
		for i, l := range limits {
			if l.Kind == "weekly_scoped" && l.Scope.Model.Name != "" {
				add(fmt.Sprintf("scoped-%d", i), l.Scope.Model.Name+" weekly", l.Used, l.Reset)
			}
		}
	}
	return windows, "", nil
}
