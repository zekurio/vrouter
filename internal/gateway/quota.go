package gateway

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// QuotaWindow is one usage limit window on an account.
type QuotaWindow struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Remaining float64    `json:"remaining"`
	ResetAt   *time.Time `json:"resetAt,omitempty"`
	// Window length, so the dashboard can compare usage with elapsed time.
	Seconds int `json:"seconds,omitempty"`
	used    *float64
}

type quotaCache struct {
	ReportedWindows []QuotaWindow
	Windows         []QuotaWindow
	Plan            string
	ObservedAt      time.Time
	Error           string
	RetryAt         time.Time
	Allowed         *bool
	Resets          *resetStatus
}

func parseQuota(provider string, body []byte, now time.Time) ([]QuotaWindow, string, error) {
	if provider == "codex" {
		return parseCodexQuota(body, now)
	}
	return parseClaudeQuota(body)
}

// appendQuotaWindow adds a window with a finite, non-negative used percentage.
func appendQuotaWindow(windows []QuotaWindow, id, label string, used *float64, reset *time.Time, seconds int) []QuotaWindow {
	if used == nil || math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0 {
		return windows
	}
	remaining := math.Round(math.Max(0, 100-*used)*10) / 10
	return append(windows, QuotaWindow{ID: id, Label: label, Remaining: remaining, ResetAt: reset, Seconds: max(seconds, 0), used: used})
}

type codexQuotaWindow struct {
	Used    *float64 `json:"used_percent"`
	Seconds int      `json:"limit_window_seconds"`
	Reset   int64    `json:"reset_at"`
	After   *int64   `json:"reset_after_seconds"`
}

type codexQuotaLimit struct {
	Primary   *codexQuotaWindow `json:"primary_window"`
	Secondary *codexQuotaWindow `json:"secondary_window"`
}

func parseCodexQuota(body []byte, now time.Time) ([]QuotaWindow, string, error) {
	var data struct {
		Plan       string          `json:"plan_type"`
		Limit      codexQuotaLimit `json:"rate_limit"`
		Review     codexQuotaLimit `json:"code_review_rate_limit"`
		Additional []struct {
			Name  string          `json:"limit_name"`
			Limit codexQuotaLimit `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "", err
	}
	windows := []QuotaWindow{}
	windows = data.Limit.appendWindows(windows, "", "", now)
	windows = data.Review.appendWindows(windows, "review-", "Code review · ", now)
	for i, extra := range data.Additional {
		windows = extra.Limit.appendWindows(windows, fmt.Sprintf("extra-%d-", i), extra.Name+" · ", now)
	}
	return windows, data.Plan, nil
}

func (l codexQuotaLimit) appendWindows(windows []QuotaWindow, prefix, name string, now time.Time) []QuotaWindow {
	for i, w := range []*codexQuotaWindow{l.Primary, l.Secondary} {
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
		windows = appendQuotaWindow(windows, prefix+id, name+label, w.Used, reset, w.Seconds)
	}
	return windows
}

func parseClaudeQuota(body []byte) ([]QuotaWindow, string, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "", err
	}
	windows := []QuotaWindow{}
	// Claude does not report window lengths; each key implies one.
	const fiveHours, week = 5 * 60 * 60, 7 * 24 * 60 * 60
	for _, meta := range []struct {
		key, id, label string
		seconds        int
	}{{"five_hour", "five-hour", "5-hour window", fiveHours}, {"seven_day", "weekly", "Weekly window", week}, {"seven_day_opus", "opus", "Opus weekly", week}, {"seven_day_sonnet", "sonnet", "Sonnet weekly", week}, {"seven_day_oauth_apps", "oauth-apps", "OAuth apps weekly", week}, {"seven_day_cowork", "cowork", "Cowork weekly", week}} {
		var w struct {
			Used  *float64   `json:"utilization"`
			Reset *time.Time `json:"resets_at"`
		}
		if raw := data[meta.key]; len(raw) > 0 && json.Unmarshal(raw, &w) == nil {
			windows = appendQuotaWindow(windows, meta.id, meta.label, w.Used, w.Reset, meta.seconds)
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
				windows = appendQuotaWindow(windows, fmt.Sprintf("scoped-%d", i), l.Scope.Model.Name+" weekly", l.Used, l.Reset, week)
			}
		}
	}
	return windows, "", nil
}
