package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
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
	// seenAt is when an inference response last reported the window. Zero
	// for a window the usage endpoint reported.
	seenAt time.Time
}

type quotaCache struct {
	ReportedWindows []QuotaWindow
	Windows         []QuotaWindow
	Plan            string
	// ObservedAt is when the provider's usage endpoint was read.
	ObservedAt time.Time
	// HeadersAt is when an inference response last updated a window. Zero
	// when none did since the usage read.
	HeadersAt time.Time
	Error     string
	RetryAt   time.Time
	Allowed   *bool
	Resets    *resetStatus
	// recordedAt is when header windows last went into the quota history.
	recordedAt time.Time
}

// updatedAt is when the windows were last confirmed, by a usage read or by
// the rate-limit headers of an inference response.
func (q quotaCache) updatedAt() time.Time {
	if q.HeadersAt.After(q.ObservedAt) {
		return q.HeadersAt
	}
	return q.ObservedAt
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

// claudeUsageKeys are the fields of a Claude usage report. A body with none
// of them is an error the endpoint sent with a 200.
var claudeUsageKeys = []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet", "cinder_cove", "wattle_ember", "extra_usage", "limits"} //nolint:gochecknoglobals // fixed list

// Claude does not report window lengths; each key implies one.
const (
	fiveHourSeconds = 5 * 60 * 60
	weekSeconds     = 7 * 24 * 60 * 60
	// fableWindow is the weekly allowance of the Fable models. Claude calls
	// its limit seven_day_overage_included.
	fableWindow = "fable"
)

func parseClaudeQuota(body []byte) ([]QuotaWindow, string, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, "", err
	}
	if !slices.ContainsFunc(claudeUsageKeys, func(key string) bool { _, ok := data[key]; return ok }) {
		return nil, "", errors.New("provider response contained no usage fields")
	}
	windows := []QuotaWindow{}
	for _, meta := range []struct {
		key, id, label string
		seconds        int
	}{{"five_hour", "five-hour", "5-hour window", fiveHourSeconds}, {"seven_day", "weekly", "Weekly window", weekSeconds}, {"seven_day_opus", "opus", "Opus weekly", weekSeconds}, {"seven_day_sonnet", "sonnet", "Sonnet weekly", weekSeconds}, {"seven_day_oauth_apps", "oauth-apps", "OAuth apps weekly", weekSeconds}, {"seven_day_cowork", "cowork", "Cowork weekly", weekSeconds}} {
		var w struct {
			Used  *float64   `json:"utilization"`
			Reset *time.Time `json:"resets_at"`
		}
		if raw := data[meta.key]; len(raw) > 0 && json.Unmarshal(raw, &w) == nil {
			windows = appendQuotaWindow(windows, meta.id, meta.label, w.Used, w.Reset, meta.seconds)
		}
	}
	// Limits of one model family are reported by name. Fable's is one of them.
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
		for _, l := range limits {
			name := strings.TrimSpace(l.Scope.Model.Name)
			if l.Kind != "weekly_scoped" || name == "" {
				continue
			}
			id, label := scopedWindow(name)
			if slices.ContainsFunc(windows, func(w QuotaWindow) bool { return w.ID == id }) {
				continue
			}
			windows = appendQuotaWindow(windows, id, label, l.Used, l.Reset, weekSeconds)
		}
	}
	return windows, "", nil
}

// scopedWindow names the window of a model family's weekly limit. The ID
// comes from the name, so it stays the same when the list changes order.
func scopedWindow(name string) (string, string) {
	if strings.HasPrefix(strings.ToLower(name), "fable") {
		return fableWindow, "Fable weekly"
	}
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, name)
	return "scoped-" + boundedText(strings.Trim(slug, "-"), 48), name + " weekly"
}

// Claude reports the windows a request counted against on the response
// itself. Reading them keeps the cache current without asking the usage
// endpoint, which rate limits. Contract read from Claude Code 2.1.322.
const claudeLimitHeader = "Anthropic-Ratelimit-Unified-"

// claudeHeaderWindows reads the usage windows from the headers of an
// inference response. A 429 also names the limit that refused the request.
func claudeHeaderWindows(h http.Header, status int, now time.Time) []QuotaWindow {
	windows := []QuotaWindow{}
	for _, meta := range []struct {
		name, id, label string
		seconds         int
	}{{"5h", "five-hour", "5-hour window", fiveHourSeconds}, {"7d", "weekly", "Weekly window", weekSeconds}, {"7d_oi", fableWindow, "Fable weekly", weekSeconds}} {
		used, err := strconv.ParseFloat(h.Get(claudeLimitHeader+meta.name+"-Utilization"), 64)
		reset := headerTime(h.Get(claudeLimitHeader+meta.name+"-Reset"), now)
		if err != nil || reset == nil {
			continue
		}
		// The header holds a fraction of the window.
		used *= 100
		windows = appendQuotaWindow(windows, meta.id, meta.label, &used, reset, meta.seconds)
	}
	if status != http.StatusTooManyRequests || h.Get(claudeLimitHeader+"Status") != "rejected" {
		return windows
	}
	reset := headerTime(h.Get(claudeLimitHeader+"Reset"), now)
	for _, meta := range []struct {
		claim, id, label string
		seconds          int
	}{{"five_hour", "five-hour", "5-hour window", fiveHourSeconds}, {"seven_day", "weekly", "Weekly window", weekSeconds}, {"seven_day_opus", "opus", "Opus weekly", weekSeconds}, {"seven_day_sonnet", "sonnet", "Sonnet weekly", weekSeconds}, {"seven_day_overage_included", fableWindow, "Fable weekly", weekSeconds}} {
		if h.Get(claudeLimitHeader+"Representative-Claim") != meta.claim || reset == nil {
			continue
		}
		used := 100.0
		windows = slices.DeleteFunc(windows, func(w QuotaWindow) bool { return w.ID == meta.id })
		windows = appendQuotaWindow(windows, meta.id, meta.label, &used, reset, meta.seconds)
	}
	return windows
}

// headerTime reads a reset time in Unix seconds. Times that have passed or
// lie more than a year ahead are not window resets.
func headerTime(value string, now time.Time) *time.Time {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || seconds < 0 || seconds > 1e11 {
		return nil
	}
	t := time.Unix(int64(seconds), 0).UTC()
	if !t.After(now) || t.After(now.AddDate(1, 0, 0)) {
		return nil
	}
	return &t
}

// merged returns q with the windows the headers report replaced, and the
// ones it lacks added. Each keeps the time it was seen, so a window no
// response mentions again grows old on its own.
func (q quotaCache) merged(windows []QuotaWindow, at time.Time) quotaCache {
	merge := func(current []QuotaWindow) []QuotaWindow {
		merged := slices.Clone(current)
		for _, w := range windows {
			if w.seenAt.IsZero() {
				w.seenAt = at
			}
			if i := slices.IndexFunc(merged, func(old QuotaWindow) bool { return old.ID == w.ID }); i >= 0 {
				merged[i] = w
			} else {
				merged = append(merged, w)
			}
		}
		return merged
	}
	q.Windows, q.ReportedWindows = merge(q.Windows), merge(q.ReportedWindows)
	if at.After(q.HeadersAt) {
		q.HeadersAt = at
	}
	return q
}

// keepingHeaderWindows returns q with the windows only the headers report,
// such as a limit the usage endpoint leaves out. A window stays for
// quotaStaleAfter after the last response that reported it. Nothing else
// would notice a reset used outside vrouter.
func (q quotaCache) keepingHeaderWindows(last quotaCache, now time.Time) quotaCache {
	var kept []QuotaWindow
	for _, w := range last.ReportedWindows {
		known := slices.ContainsFunc(q.ReportedWindows, func(read QuotaWindow) bool { return read.ID == w.ID })
		if !w.seenAt.IsZero() && now.Sub(w.seenAt) < quotaStaleAfter && !known && w.ResetAt != nil && now.Before(*w.ResetAt) {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		return q
	}
	return q.merged(kept, last.HeadersAt)
}

// withoutHeaderWindows returns q with no window marked as read from headers,
// so the next usage read replaces every one of them. A reset makes what the
// headers said before it wrong.
func (q quotaCache) withoutHeaderWindows() quotaCache {
	q.HeadersAt = time.Time{}
	q.Windows, q.ReportedWindows = slices.Clone(q.Windows), slices.Clone(q.ReportedWindows)
	for _, windows := range [][]QuotaWindow{q.Windows, q.ReportedWindows} {
		for i := range windows {
			windows[i].seenAt = time.Time{}
		}
	}
	return q
}

// recent returns q with only what was confirmed within quotaStaleAfter. A
// window a response reported is as new as that response, whatever the usage
// read around it says. The others, the provider's permission and the resets
// are as new as the usage read.
func (q quotaCache) recent(now time.Time) quotaCache {
	read := now.Sub(q.ObservedAt) < quotaStaleAfter
	keep := func(windows []QuotaWindow) []QuotaWindow {
		kept := make([]QuotaWindow, 0, len(windows))
		for _, w := range windows {
			if (w.seenAt.IsZero() && read) || (!w.seenAt.IsZero() && now.Sub(w.seenAt) < quotaStaleAfter) {
				kept = append(kept, w)
			}
		}
		return kept
	}
	q.Windows, q.ReportedWindows = keep(q.Windows), keep(q.ReportedWindows)
	if !read {
		q.Allowed, q.Resets = nil, nil
	}
	return q
}
