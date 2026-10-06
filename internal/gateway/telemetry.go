package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"
	"unicode/utf8"
)

// Telemetry is deliberately narrow: counts, routing identifiers, timing and
// outcome. Prompts, completions, headers, provider tokens and provider error
// text are never persisted.
type telemetryRecord struct {
	ID           string    `json:"id"`
	StartedAt    time.Time `json:"startedAt"`
	GatewayID    string    `json:"gatewayId"`
	KeyID        string    `json:"keyId"`
	KeyName      string    `json:"keyName"`
	Model        string    `json:"model"`
	NativeModel  string    `json:"nativeModel"`
	Provider     string    `json:"provider"`
	AccountID    string    `json:"accountId"`
	Status       int       `json:"status"`
	DurationMs   int64     `json:"durationMs"`
	InputTokens  int64     `json:"inputTokens"`
	OutputTokens int64     `json:"outputTokens"`
	CachedTokens int64     `json:"cachedTokens"`
	TotalTokens  int64     `json:"totalTokens"`
	UsageKnown   bool      `json:"usageKnown"`
	Stream       bool      `json:"stream"`
	Outcome      string    `json:"outcome"`
}

const (
	outcomeSuccess    = "success"
	outcomeError      = "error"
	outcomeIncomplete = "incomplete"

	maxTelemetryField = 256
)

func validateTelemetryRecord(record telemetryRecord) error {
	if len(record.ID) > 64 || len(record.GatewayID) > 64 || len(record.KeyID) > 64 {
		return errors.New("gateway: telemetry record has an oversized identifier")
	}
	for _, value := range []string{record.KeyName, record.Model, record.NativeModel, record.Provider, record.AccountID} {
		if len(value) > maxTelemetryField {
			return errors.New("gateway: telemetry record has an oversized field")
		}
	}
	switch record.Outcome {
	case outcomeSuccess, outcomeError, outcomeIncomplete:
	default:
		return errors.New("gateway: telemetry record has an unknown outcome")
	}
	if record.Status < 0 || record.Status > 999 || record.DurationMs < 0 {
		return errors.New("gateway: telemetry record has invalid counters")
	}
	for _, value := range []int64{record.InputTokens, record.OutputTokens, record.CachedTokens, record.TotalTokens} {
		if value < 0 || value > 1<<62 {
			return errors.New("gateway: telemetry record has invalid token counts")
		}
	}
	return nil
}

type telemetryTotals struct {
	Requests     int   `json:"requests"`
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	TotalTokens  int64 `json:"totalTokens"`
}

type telemetryResponse struct {
	Requests       []telemetryRecord `json:"requests"`
	Totals         telemetryTotals   `json:"totals"`
	RetentionLimit int               `json:"retentionLimit"`
}

// telemetry returns the retained records for one gateway, newest first.
func (m *manager) telemetry(gatewayID string) telemetryResponse {
	response := telemetryResponse{Requests: []telemetryRecord{}, RetentionLimit: telemetryRetention}
	records := m.registry.snapshot().Telemetry[gatewayID]
	response.Totals.Requests = len(records)
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		response.Requests = append(response.Requests, record)
		response.Totals.InputTokens += record.InputTokens
		response.Totals.OutputTokens += record.OutputTokens
		response.Totals.TotalTokens += record.TotalTokens
	}
	return response
}

func (s *server) telemetryHandler(w http.ResponseWriter, r *http.Request) {
	if s.manager == nil {
		writeJSON(w, 200, telemetryResponse{Requests: []telemetryRecord{}, RetentionLimit: telemetryRetention})
		return
	}
	writeJSON(w, 200, s.manager.telemetry(s.gatewayID))
}

// ---------------------------------------------------------------------------
// Quota admission and settlement

const usageUncertainMessage = "This key's token usage could not be measured or recorded, so the key is blocked until the gateway owner changes its limits or replaces it."

// inferenceKey is the authenticated request identity carried from key
// validation to the forwarding engine.
type inferenceKey struct {
	GatewayID string
	KeyID     string
	KeyName   string
	Legacy    bool
}

// inferenceAttempt accumulates everything the settlement needs for one
// supported inference POST, including usage parsed from the provider response.
type inferenceAttempt struct {
	principal      inferenceKey
	startedAt      time.Time
	model          string
	native         string
	provider       string
	accountID      string
	stream         bool
	outcome        string
	reserved       bool
	bodyComplete   bool
	dispatchFailed bool
	durationMs     int64
	usage          usageParser
}

// usageTrusted reports whether the provider response was delivered to a
// terminal state. A stream that ends early or a truncated body may carry
// partial usage, so its numbers must not reopen a measured token budget.
func (a *inferenceAttempt) usageTrusted() bool {
	if a.stream {
		return a.usage.terminal && !a.usage.terminalFailure
	}
	return a.bodyComplete
}

// update applies a registry mutation and records that persistence is healthy
// again after a previous failure, so token-limited keys recover once the disk
// is writable.
func (m *manager) update(mutate func(*diskRegistry) error) error {
	err := m.registry.update(mutate)
	if err == nil {
		m.degraded.Store(false)
	}
	return err
}

// reserve admits one inference attempt. It enforces the lifetime request
// limit and the measured token cap atomically, and permits at most one
// in-flight request for a token-limited key. The reservation is persisted
// before the request may be forwarded.
func (m *manager) reserve(principal inferenceKey) (bool, int, string) {
	if principal.KeyID == "" {
		return true, 0, ""
	}
	allowed, status, message := true, 0, ""
	err := m.update(func(registry *diskRegistry) error {
		key := findKey(registry, principal.KeyID)
		if key == nil || key.RevokedAt != nil {
			allowed, status, message = false, http.StatusUnauthorized, "A valid vrouter client API key is required"
			return errRegistryNoChange
		}
		if key.UsageUncertain && key.LimitTokens > 0 {
			allowed, status, message = false, http.StatusTooManyRequests, usageUncertainMessage
			return errRegistryNoChange
		}
		if key.LimitRequests > 0 && key.UsedRequests >= key.LimitRequests {
			allowed, status, message = false, http.StatusTooManyRequests, "This key's lifetime request limit is reached."
			return errRegistryNoChange
		}
		if key.LimitTokens > 0 {
			if m.degraded.Load() {
				allowed, status, message = false, http.StatusServiceUnavailable, "Usage accounting is unavailable; token-limited requests are paused."
				return errRegistryNoChange
			}
			if key.UsedTokens >= key.LimitTokens {
				allowed, status, message = false, http.StatusTooManyRequests, "This key's lifetime token limit is reached."
				return errRegistryNoChange
			}
			// A cap added while uncapped requests are still running must see
			// them, so every reservation is counted, not only capped ones.
			if key.InFlight > 0 {
				allowed, status, message = false, http.StatusTooManyRequests, "This key already has a request in flight using its measured token budget. Retry when it finishes."
				return errRegistryNoChange
			}
		}
		key.InFlight++
		key.UsedRequests++
		return nil
	})
	if errors.Is(err, errRegistryNoChange) {
		return false, status, message
	}
	if err != nil {
		m.degraded.Store(true)
		return false, http.StatusServiceUnavailable, "Usage accounting is unavailable. Try again shortly."
	}
	return allowed, status, message
}

// settle persists the completed attempt: it releases this attempt's in-flight
// reservation, adds measured tokens, records uncertain usage when a forwarded
// response cannot be trusted, and appends the telemetry row. All of it happens
// in one atomic registry write. A write failure fails closed by keeping
// token-limited keys blocked until a later successful write.
func (m *manager) settle(principal inferenceKey, record telemetryRecord, totals usageTotals, usageAccepted, poison, reserved bool) {
	err := m.update(func(registry *diskRegistry) error {
		if principal.KeyID != "" {
			if key := findKey(registry, principal.KeyID); key != nil {
				// Only the attempt that took the reservation may release it;
				// a rejected request must never free another's budget.
				if reserved && key.InFlight > 0 {
					key.InFlight--
				}
				if usageAccepted {
					key.UsedTokens = clampTokenCount(saturatingAdd(key.UsedTokens, totals.Total))
				}
				if poison {
					key.UsageUncertain = true
				}
			}
		}
		registry.Telemetry[record.GatewayID] = append(registry.Telemetry[record.GatewayID], record)
		return nil
	})
	if err != nil {
		m.degraded.Store(true)
		// Only a poisoned forwarded attempt justifies distrusting the key; a
		// denied or never-forwarded attempt must not.
		if poison {
			m.registry.markUncertain(principal.KeyID)
		}
	}
}

// ---------------------------------------------------------------------------
// Bounded usage parsing

const (
	maxUsageLineBytes    = 1 << 20 // one SSE line; usage events are far smaller
	maxUsageCaptureBytes = 2 << 20 // non-stream JSON body captured for usage
	// maxTokenCount bounds every token counter so registry validation can
	// never fail from an absurd provider value.
	maxTokenCount = int64(1) << 62
)

type usageTotals struct {
	Input  int64
	Output int64
	Cached int64
	Total  int64
	Known  bool
}

type usagePayload struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	TotalTokens              *int64 `json:"total_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	InputTokensDetails       *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type usageEnvelope struct {
	Type    string          `json:"type"`
	Usage   *usagePayload   `json:"usage"`
	Error   json.RawMessage `json:"error"`
	Message *struct {
		Usage *usagePayload `json:"usage"`
	} `json:"message"`
	Response *struct {
		Usage  *usagePayload   `json:"usage"`
		Error  json.RawMessage `json:"error"`
		Status string          `json:"status"`
	} `json:"response"`
}

// usageParser accumulates provider-reported usage with bounded memory. It
// understands both Codex Responses SSE/JSON and Claude Messages SSE/JSON,
// tolerates events split across reads, treats cumulative events as maxima
// (never sums), and keeps cached tokens as a subset of input tokens.
type usageParser struct {
	stream   bool
	started  bool
	line     []byte
	dropping bool
	capture  *bytes.Buffer
	captured int64
	overflow bool
	input    int64
	output   int64
	cached   int64
	total    int64
	known    bool
	// terminal records that the provider reported a final event
	// (message_stop, response.completed/incomplete/failed, [DONE]). A stream
	// that ends without one delivered only a partial response.
	terminal        bool
	terminalFailure bool
	// providerIncomplete marks a terminal response.incomplete event: the
	// provider stopped the response early, so it is not a success.
	providerIncomplete bool
	lastEvent          string
}

func (u *usageParser) begin(stream bool) {
	u.stream = stream
	u.started = true
	if !stream {
		u.capture = &bytes.Buffer{}
	}
}

func (u *usageParser) observe(data []byte) {
	if !u.started || len(data) == 0 {
		return
	}
	if u.stream {
		u.observeSSE(data)
		return
	}
	u.captureJSON(data)
}

// complete flushes a trailing partial line and parses a captured JSON body.
// It is idempotent and safe to call after the provider body has ended.
func (u *usageParser) complete() {
	if !u.started {
		return
	}
	if u.stream {
		if !u.dropping && len(u.line) > 0 {
			u.processSSELine(u.line)
			u.line = nil
		}
		return
	}
	if u.capture != nil && !u.overflow {
		u.parseJSON(u.capture.Bytes())
	}
	u.capture = nil
}

func (u *usageParser) totals() usageTotals {
	totals := usageTotals{Input: u.input, Output: u.output, Cached: u.cached, Total: u.total, Known: u.known}
	if !totals.Known {
		return usageTotals{}
	}
	totals.Input = clampTokenCount(totals.Input)
	totals.Output = clampTokenCount(totals.Output)
	totals.Cached = clampTokenCount(totals.Cached)
	if computed := saturatingAdd(totals.Input, totals.Output); totals.Total < computed {
		totals.Total = computed
	}
	totals.Total = clampTokenCount(totals.Total)
	return totals
}

func (u *usageParser) observeSSE(data []byte) {
	for _, b := range data {
		if b == '\n' {
			if !u.dropping {
				u.processSSELine(u.line)
			}
			u.line = u.line[:0]
			u.dropping = false
			continue
		}
		if u.dropping {
			continue
		}
		if len(u.line) >= maxUsageLineBytes {
			// Bound memory even for hostile or oversized frames: the rest of
			// this line is discarded, later lines are still parsed.
			u.line = nil
			u.dropping = true
			continue
		}
		u.line = append(u.line, b)
	}
}

func (u *usageParser) processSSELine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) == 0 {
		u.lastEvent = ""
		return
	}
	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return
	}
	field := string(bytes.TrimSpace(line[:colon]))
	value := bytes.TrimSpace(line[colon+1:])
	switch field {
	case "event":
		if len(value) <= 64 {
			u.lastEvent = string(value)
		}
	case "data":
		if string(value) == "[DONE]" {
			u.terminal = true
			return
		}
		u.parseJSON(value)
	}
}

func (u *usageParser) captureJSON(data []byte) {
	if u.overflow {
		return
	}
	if u.captured+int64(len(data)) > maxUsageCaptureBytes {
		u.overflow = true
		u.capture = nil
		return
	}
	if u.capture == nil {
		u.capture = &bytes.Buffer{}
	}
	_, _ = u.capture.Write(data)
	u.captured += int64(len(data))
}

func (u *usageParser) parseJSON(data []byte) {
	var envelope usageEnvelope
	if json.Unmarshal(data, &envelope) != nil {
		return
	}
	u.applyUsage(envelope.Usage)
	if envelope.Message != nil {
		u.applyUsage(envelope.Message.Usage)
	}
	if envelope.Response != nil {
		u.applyUsage(envelope.Response.Usage)
	}
	switch envelope.Type {
	case "message_stop", "response.completed":
		u.terminal = true
	case "response.incomplete":
		u.terminal = true
		u.providerIncomplete = true
	case "error", "response.failed":
		u.terminal = true
		u.terminalFailure = true
	}
	if u.lastEvent == "error" {
		u.terminal = true
		u.terminalFailure = true
	}
	if len(envelope.Error) > 0 && !bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null")) {
		u.terminalFailure = true
	}
	if envelope.Response != nil {
		if envelope.Response.Status == "failed" {
			u.terminal = true
			u.terminalFailure = true
		}
		if len(envelope.Response.Error) > 0 && !bytes.Equal(bytes.TrimSpace(envelope.Response.Error), []byte("null")) {
			u.terminalFailure = true
		}
	}
}

// applyUsage folds one cumulative usage sample into the maxima. Claude counts
// cache reads and cache writes apart from input_tokens, so they are added to
// input while cached reads stay a subset of it. A sample with any negative or
// absurd count is discarded whole: invalid numbers must never look like a
// trustworthy zero.
func (u *usageParser) applyUsage(sample *usagePayload) {
	if sample == nil {
		return
	}
	counts := []*int64{sample.InputTokens, sample.OutputTokens, sample.TotalTokens, sample.CacheReadInputTokens, sample.CacheCreationInputTokens}
	if sample.InputTokensDetails != nil {
		counts = append(counts, sample.InputTokensDetails.CachedTokens)
	}
	for _, count := range counts {
		if count != nil && (*count < 0 || *count > maxTokenCount) {
			return
		}
	}
	seen := false
	if sample.InputTokens != nil {
		seen = true
		input := *sample.InputTokens
		if sample.CacheReadInputTokens != nil {
			input = saturatingAdd(input, *sample.CacheReadInputTokens)
		}
		if sample.CacheCreationInputTokens != nil {
			input = saturatingAdd(input, *sample.CacheCreationInputTokens)
		}
		if input > u.input {
			u.input = input
		}
	}
	if sample.CacheReadInputTokens != nil {
		seen = true
		if cached := *sample.CacheReadInputTokens; cached > u.cached {
			u.cached = cached
		}
	}
	if sample.InputTokensDetails != nil && sample.InputTokensDetails.CachedTokens != nil {
		seen = true
		if cached := *sample.InputTokensDetails.CachedTokens; cached > u.cached {
			u.cached = cached
		}
	}
	if sample.OutputTokens != nil {
		seen = true
		if output := *sample.OutputTokens; output > u.output {
			u.output = output
		}
	}
	if sample.TotalTokens != nil {
		seen = true
		if total := *sample.TotalTokens; total > u.total {
			u.total = total
		}
	}
	if seen {
		u.known = true
	}
}

func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func clampTokenCount(value int64) int64 {
	if value < 0 {
		return 0
	}
	if value > maxTokenCount {
		return maxTokenCount
	}
	return value
}

// boundedField truncates a telemetry string to the persisted bound on a rune
// boundary, so an oversized client value can never make the registry
// unwritable or contain invalid UTF-8.
func boundedField(value string) string {
	if len(value) <= maxTelemetryField {
		return value
	}
	cut := maxTelemetryField
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// ---------------------------------------------------------------------------
// Attempt writer

// attemptWriter records the status inferred from what the handler wrote. It
// unwraps for http.ResponseController so streaming flushes are preserved.
type attemptWriter struct {
	http.ResponseWriter
	status int
}

func (w *attemptWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *attemptWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(data)
}

func (w *attemptWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
