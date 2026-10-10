package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"slices"
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
	// CacheWriteTokens are Claude cache-creation tokens. Like CachedTokens they
	// are a subset of InputTokens, kept apart because the API bills them at a
	// different rate.
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	TotalTokens      int64 `json:"totalTokens"`
	UsageKnown       bool  `json:"usageKnown"`
	// UsagePartial is set when counts were observed but the provider never
	// delivered a trustworthy final usage report (a stream cut short, a
	// terminal event without usage). The token fields are then partial.
	UsagePartial bool   `json:"usagePartial,omitempty"`
	Stream       bool   `json:"stream"`
	Outcome      string `json:"outcome"`
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
	for _, value := range []int64{record.InputTokens, record.OutputTokens, record.CachedTokens, record.CacheWriteTokens, record.TotalTokens} {
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
	for _, record := range slices.Backward(records) {
		response.Requests = append(response.Requests, record)
		if !record.UsageKnown {
			continue
		}
		response.Totals.InputTokens = clampTokenCount(saturatingAdd(response.Totals.InputTokens, record.InputTokens))
		response.Totals.OutputTokens = clampTokenCount(saturatingAdd(response.Totals.OutputTokens, record.OutputTokens))
		response.Totals.TotalTokens = clampTokenCount(saturatingAdd(response.Totals.TotalTokens, record.TotalTokens))
	}
	return response
}

func (s *server) telemetryHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.manager.telemetry(s.gatewayID))
}

// ---------------------------------------------------------------------------
// Quota admission and settlement

const keyExpiredMessage = "This vrouter client API key has expired"

// inferenceKey is the authenticated request identity carried from key
// validation to the forwarding engine.
type inferenceKey struct {
	Provider  string
	GatewayID string
	KeyID     string
	KeyName   string
}

// inferenceAttempt accumulates everything the settlement needs for one
// supported inference POST, including usage parsed from the provider response.
type inferenceAttempt struct {
	principal    inferenceKey
	startedAt    time.Time
	model        string
	native       string
	provider     string
	accountID    string
	stream       bool
	outcome      string
	bodyComplete bool
	durationMs   int64
	usage        usageParser
}

// usageTrusted reports whether the provider response was delivered to a
// terminal state carrying a usable final usage report. A stream that ends
// early, a terminal event without usage, or a truncated body may carry stale
// or partial usage, so its numbers are not counted as known usage.
func (a *inferenceAttempt) usageTrusted() bool {
	if a.usage.stream {
		return a.usage.terminal && !a.usage.terminalFailure && (a.usage.terminalUsage || a.usage.finalUsage)
	}
	return a.bodyComplete
}

// reserve admits one inference attempt. It checks that the key is still
// active and unexpired, and counts the request.
func (m *manager) reserve(principal inferenceKey) (bool, int, string) {
	if principal.KeyID == "" {
		return true, 0, ""
	}
	allowed, status, message := true, 0, ""
	err := m.registry.update(func(registry *diskRegistry) error {
		key := findKey(registry, principal.KeyID)
		if key == nil || key.RevokedAt != nil {
			allowed, status, message = false, http.StatusUnauthorized, "A valid vrouter client API key is required"
			return errRegistryNoChange
		}
		if key.expired(time.Now()) {
			allowed, status, message = false, http.StatusUnauthorized, keyExpiredMessage
			return errRegistryNoChange
		}
		key.UsedRequests = clampTokenCount(saturatingAdd(key.UsedRequests, 1))
		return nil
	})
	if errors.Is(err, errRegistryNoChange) {
		return false, status, message
	}
	if err != nil {
		return false, http.StatusServiceUnavailable, "Usage accounting is unavailable. Try again shortly."
	}
	return allowed, status, message
}

// settle adds measured tokens to the key and appends the telemetry row in one
// atomic registry write.
func (m *manager) settle(principal inferenceKey, record telemetryRecord, totals usageTotals, usageAccepted bool) {
	err := m.registry.update(func(registry *diskRegistry) error {
		if principal.KeyID != "" && usageAccepted {
			if key := findKey(registry, principal.KeyID); key != nil {
				key.UsedTokens = clampTokenCount(saturatingAdd(key.UsedTokens, totals.Total))
			}
		}
		registry.Telemetry[record.GatewayID] = append(registry.Telemetry[record.GatewayID], record)
		return nil
	})
	if err != nil {
		slog.Warn("request telemetry not saved", "gateway", record.GatewayID, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Bounded usage parsing

const (
	maxUsageLineBytes    = 1 << 20 // one SSE line; usage events are far smaller
	maxUsageEventBytes   = 2 << 20 // joined data of one SSE event
	maxUsageCaptureBytes = 2 << 20 // non-stream JSON body captured for usage
	// maxTokenCount bounds every token counter so registry validation can
	// never fail from an absurd provider value.
	maxTokenCount = int64(1) << 62
)

type usageTotals struct {
	Input      int64
	Output     int64
	Cached     int64
	CacheWrite int64
	Total      int64
	Known      bool
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

type rawUsageEnvelope struct {
	Type       string          `json:"type"`
	Usage      json.RawMessage `json:"usage"`
	Error      json.RawMessage `json:"error"`
	StopReason string          `json:"stop_reason"`
	Delta      *struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Message *struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	Response *struct {
		Usage  json.RawMessage `json:"usage"`
		Error  json.RawMessage `json:"error"`
		Status string          `json:"status"`
	} `json:"response"`
}

// usageParser accumulates provider-reported usage with bounded memory. It
// understands both Codex Responses SSE/JSON and Claude Messages SSE/JSON,
// tolerates events split across reads, treats cumulative events as maxima
// (never sums), and keeps cached tokens as a subset of input tokens.
type usageParser struct {
	stream        bool
	started       bool
	line          []byte
	dropping      bool
	eventData     []byte
	eventOverflow bool
	capture       *bytes.Buffer
	captured      int64
	overflow      bool
	input         int64
	output        int64
	cached        int64
	cacheWrite    int64
	total         int64
	known         bool
	hasInput      bool
	// terminal records that the provider reported a final event
	// (message_stop, response.completed/incomplete/failed, [DONE]). A stream
	// that ends without one delivered only a partial response.
	terminal        bool
	terminalFailure bool
	// terminalUsage records that the terminal event itself carried a usable
	// usage report (Codex response.completed/incomplete). finalUsage records a
	// Claude message_delta, which carries the final cumulative counts before
	// message_stop. Only these make stream usage trustworthy.
	terminalUsage bool
	finalUsage    bool
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

// complete flushes a trailing partial line and event, and parses a captured
// JSON body. It is idempotent and safe to call after the provider body ended.
func (u *usageParser) complete() {
	if !u.started {
		return
	}
	if u.stream {
		if !u.dropping && len(u.line) > 0 {
			u.processSSELine(u.line)
			u.line = nil
		}
		u.flushEvent()
		return
	}
	if u.capture != nil && !u.overflow {
		u.parseJSON(u.capture.Bytes())
	}
	u.capture = nil
}

func (u *usageParser) totals() usageTotals {
	totals := usageTotals{Input: u.input, Output: u.output, Cached: u.cached, CacheWrite: u.cacheWrite, Total: u.total, Known: u.known}
	if !totals.Known {
		return usageTotals{}
	}
	totals.Input = clampTokenCount(totals.Input)
	totals.Output = clampTokenCount(totals.Output)
	totals.Cached = clampTokenCount(totals.Cached)
	totals.CacheWrite = clampTokenCount(totals.CacheWrite)
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
			u.eventOverflow = true
			u.invalidateUsage()
			u.dropping = true
			continue
		}
		u.line = append(u.line, b)
	}
}

func (u *usageParser) processSSELine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) == 0 {
		u.flushEvent()
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
		if u.eventOverflow {
			return
		}
		if len(u.eventData)+len(value)+1 > maxUsageEventBytes {
			u.eventData = nil
			u.eventOverflow = true
			u.invalidateUsage()
			return
		}
		if len(u.eventData) > 0 {
			u.eventData = append(u.eventData, '\n')
		}
		u.eventData = append(u.eventData, value...)
	}
}

// flushEvent joins the data lines of one SSE event, as the protocol requires,
// and parses the result. A stream that never reaches a terminal event leaves
// the response incomplete.
func (u *usageParser) flushEvent() {
	event := u.lastEvent
	if len(u.eventData) > 0 && !u.eventOverflow {
		if bytes.Equal(bytes.TrimSpace(u.eventData), []byte("[DONE]")) {
			u.terminal = true
		} else {
			u.parseJSON(u.eventData)
		}
	}
	if event == "message_stop" {
		u.terminal = true
	}
	u.eventData = u.eventData[:0]
	u.eventOverflow = false
	u.lastEvent = ""
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
	var envelope rawUsageEnvelope
	if json.Unmarshal(data, &envelope) != nil {
		// Unreadable final usage must not leave earlier counts trusted.
		u.invalidateUsage()
		return
	}
	usable := u.applyRawUsage(envelope.Usage)
	if envelope.StopReason == "max_tokens" || envelope.Delta != nil && envelope.Delta.StopReason == "max_tokens" {
		u.providerIncomplete = true
	}
	if envelope.Message != nil {
		usable = u.applyRawUsage(envelope.Message.Usage) || usable
	}
	if envelope.Response != nil {
		usable = u.applyRawUsage(envelope.Response.Usage) || usable
	}
	switch envelope.Type {
	case "message_stop":
		u.terminal = true
	case "response.completed":
		u.terminal = true
		u.terminalUsage = usable && envelope.Response != nil && finalUsageReport(envelope.Response.Usage, true)
	case "response.incomplete":
		u.terminal = true
		u.providerIncomplete = true
		u.terminalUsage = usable && envelope.Response != nil && finalUsageReport(envelope.Response.Usage, true)
	case "message_delta":
		// Claude reports its final cumulative output count here, just before
		// message_stop.
		u.finalUsage = usable && u.hasInput && finalUsageReport(envelope.Usage, false)
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

func finalUsageReport(raw json.RawMessage, requireInput bool) bool {
	var sample usagePayload
	return json.Unmarshal(raw, &sample) == nil && sample.OutputTokens != nil && (!requireInput || sample.InputTokens != nil)
}

// applyRawUsage decodes one usage object leniently and reports whether it
// contributed usable counts. An object that cannot be decoded, or that carries
// no recognizable token field after earlier counts were seen, invalidates all
// accumulated usage: a final cumulative report must never leave stale partial
// numbers looking trustworthy.
func (u *usageParser) applyRawUsage(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var sample usagePayload
	if err := json.Unmarshal(raw, &sample); err != nil {
		u.invalidateUsage()
		return false
	}
	return u.applyUsage(&sample)
}

// invalidateUsage discards all accumulated counts and marks the usage
// unknown. It is used when a later cumulative sample is unusable.
func (u *usageParser) invalidateUsage() {
	u.input, u.output, u.cached, u.cacheWrite, u.total = 0, 0, 0, 0, 0
	u.known = false
	u.hasInput = false
	u.terminalUsage = false
	u.finalUsage = false
}

// applyUsage folds one cumulative usage sample into the maxima and reports
// whether it contributed usable counts. Claude counts cache reads and cache
// writes apart from input_tokens, so they are added to input while cached
// reads stay a subset of it. A sample with any negative or absurd count is
// discarded whole, and a sample with no usable fields after earlier counts
// were seen invalidates those earlier counts: invalid numbers must never look
// like a trustworthy zero.
func (u *usageParser) applyUsage(sample *usagePayload) bool {
	if sample == nil {
		return false
	}
	counts := []*int64{sample.InputTokens, sample.OutputTokens, sample.TotalTokens, sample.CacheReadInputTokens, sample.CacheCreationInputTokens}
	if sample.InputTokensDetails != nil {
		counts = append(counts, sample.InputTokensDetails.CachedTokens)
	}
	for _, count := range counts {
		if count != nil && (*count < 0 || *count > maxTokenCount) {
			u.invalidateUsage()
			return false
		}
	}
	seen := false
	if sample.InputTokens != nil {
		u.hasInput = true
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
	if sample.CacheCreationInputTokens != nil {
		if written := *sample.CacheCreationInputTokens; written > u.cacheWrite {
			u.cacheWrite = written
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
	if !seen {
		if u.known {
			u.invalidateUsage()
		}
		return false
	}
	u.known = true
	return true
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
