package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// serveInference is the /v1 entry point. It selects the gateway from the
// presented API key, never from a header, and hands the request to the engine
// that owns the key. VROUTER_API_KEY continues to select the legacy default.
func (s *server) serveInference(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Demo {
		writeJSON(w, 503, map[string]string{"error": "Inference is disabled in demo mode"})
		return
	}
	principal, ok := s.authorizeInference(w, r)
	if !ok {
		return
	}
	if principal.GatewayID != s.gatewayID {
		if s.manager == nil {
			writeJSON(w, 503, map[string]string{"error": "This key's gateway is unavailable"})
			return
		}
		target, err := s.manager.engine(principal.GatewayID)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "This key's gateway is unavailable"})
			return
		}
		target.serveInferenceAuthorized(w, r, principal)
		return
	}
	s.serveInferenceAuthorized(w, r, principal)
}

// authorizeInference validates the client credential. Keys never authorize
// management requests; management routes have their own authentication.
func (s *server) authorizeInference(w http.ResponseWriter, r *http.Request) (inferenceKey, bool) {
	auth := r.Header.Get("Authorization")
	key := r.Header.Get("X-Api-Key")
	if auth != "" {
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSON(w, 401, map[string]string{"error": "Invalid client API key"})
			return inferenceKey{}, false
		}
		bearer := strings.TrimPrefix(auth, "Bearer ")
		if key != "" && !tokenEqual(key, bearer) {
			writeJSON(w, 401, map[string]string{"error": "Conflicting client credentials"})
			return inferenceKey{}, false
		}
		key = bearer
	}
	if r.URL.Query().Has("key") || r.Header.Get("X-Goog-Api-Key") != "" || key == "" {
		writeJSON(w, 401, map[string]string{"error": "A valid vrouter client API key is required"})
		return inferenceKey{}, false
	}
	if s.cfg.APIKey != "" && tokenEqual(key, s.cfg.APIKey) {
		return inferenceKey{GatewayID: s.gatewayID, KeyName: "VROUTER_API_KEY", Legacy: true}, true
	}
	if s.manager != nil {
		if record, ok := s.manager.keyByHash(key); ok {
			return inferenceKey{GatewayID: record.GatewayID, KeyID: record.ID, KeyName: record.Name}, true
		}
	}
	if s.cfg.APIKey == "" && (s.manager == nil || !s.manager.hasAnyKeys()) {
		writeJSON(w, 503, map[string]string{"error": "Set VROUTER_API_KEY or create a gateway API key to enable client requests"})
		return inferenceKey{}, false
	}
	writeJSON(w, 401, map[string]string{"error": "A valid vrouter client API key is required"})
	return inferenceKey{}, false
}

// serveInferenceAuthorized runs an already authenticated request. Supported
// inference POSTs are admitted against the key's lifetime quota before any
// provider work; every attempt, including failures, settles exactly one
// telemetry row.
func (s *server) serveInferenceAuthorized(w http.ResponseWriter, r *http.Request, principal inferenceKey) {
	isAttempt := r.Method == http.MethodPost && (r.URL.Path == "/v1/responses" || r.URL.Path == "/v1/messages")
	if !isAttempt {
		s.inference(w, r, nil)
		return
	}
	recorder := &attemptWriter{ResponseWriter: w}
	attempt := &inferenceAttempt{principal: principal, startedAt: time.Now().UTC()}
	if s.manager != nil {
		allowed, status, message := s.manager.reserve(principal)
		if !allowed {
			attempt.outcome = outcomeError
			writeJSON(recorder, status, map[string]string{"error": message})
			s.finishAttempt(attempt, recorder)
			return
		}
		attempt.reserved = true
	}
	s.inference(recorder, r, attempt)
	s.finishAttempt(attempt, recorder)
}

// finishAttempt persists one telemetry row and, for managed keys, the settled
// quota state. Unknown usage is never reported as known zero.
func (s *server) finishAttempt(attempt *inferenceAttempt, recorder *attemptWriter) {
	attempt.usage.complete()
	status := recorder.status
	if status == 0 {
		status = 200
	}
	attempt.durationMs = time.Since(attempt.startedAt).Milliseconds()
	if attempt.durationMs < 0 {
		attempt.durationMs = 0
	}
	outcome := attempt.outcome
	if outcome == "" {
		if status >= 200 && status < 300 {
			outcome = outcomeSuccess
		} else {
			outcome = outcomeError
		}
	}
	if attempt.usage.terminalFailure && outcome == outcomeSuccess {
		outcome = outcomeError
	}
	if attempt.usage.providerIncomplete && outcome == outcomeSuccess {
		outcome = outcomeIncomplete
	}
	if s.manager == nil {
		return
	}
	totals := attempt.usage.totals()
	// Only a forwarded provider success can have consumed tokens; a local
	// rejection or a provider HTTP error must not poison a measured budget.
	dispatched := attempt.provider != ""
	forwarded := dispatched && status >= 200 && status < 300
	usageAccepted := forwarded && totals.Known && attempt.usageTrusted()
	// An ambiguous dispatch may have consumed tokens we never saw, and a
	// forwarded response whose usage is missing or partial may understate the
	// real spend. Both must block a token-limited key instead of silently
	// reopening its budget.
	poison := attempt.dispatchFailed || (forwarded && !usageAccepted)
	id, _ := secureID()
	record := telemetryRecord{
		ID:           id,
		StartedAt:    attempt.startedAt,
		GatewayID:    s.gatewayID,
		KeyID:        attempt.principal.KeyID,
		KeyName:      boundedField(attempt.principal.KeyName),
		Model:        boundedField(attempt.model),
		NativeModel:  boundedField(attempt.native),
		Provider:     boundedField(attempt.provider),
		AccountID:    boundedField(attempt.accountID),
		Status:       status,
		DurationMs:   attempt.durationMs,
		InputTokens:  totals.Input,
		OutputTokens: totals.Output,
		CachedTokens: totals.Cached,
		TotalTokens:  totals.Total,
		UsageKnown:   totals.Known,
		Stream:       attempt.stream,
		Outcome:      outcome,
	}
	s.manager.settle(attempt.principal, record, totals, usageAccepted, poison, attempt.reserved)
}

func (s *server) inference(w http.ResponseWriter, r *http.Request, attempt *inferenceAttempt) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		models, err := s.models(r.Context())
		if err != nil && len(models) == 0 {
			writeJSON(w, 502, map[string]string{"error": "Provider catalogs unavailable"})
			return
		}
		data := make([]map[string]any, 0, len(models))
		for _, m := range models {
			row := map[string]any{"id": m.ID, "object": "model", "owned_by": strings.ToLower(m.Provider), "created": m.Created}
			if m.Context > 0 {
				row["context_window"] = m.Context
			}
			data = append(data, row)
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
		return
	}
	if r.URL.Path != "/v1/responses" && r.URL.Path != "/v1/messages" {
		writeJSON(w, 404, map[string]string{"error": "Supported endpoints are /v1/models, /v1/responses and /v1/messages"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, 405, map[string]string{"error": "Use POST for inference"})
		return
	}
	var payload map[string]json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	if decoder.Decode(&payload) != nil || payload == nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeJSON(w, 400, map[string]string{"error": "Expected a JSON inference request, at most 16 MiB"})
		return
	}
	var model string
	if json.Unmarshal(payload["model"], &model) != nil || model == "" {
		writeJSON(w, 400, map[string]string{"error": "Choose a model"})
		return
	}
	channel := "codex"
	if r.URL.Path == "/v1/messages" {
		channel = "claude"
	}
	state := s.store.snapshot()
	native := model
	for _, a := range state.Policy.Aliases[channel] {
		if a.Alias == model {
			native = a.Name
			break
		}
	}
	if attempt != nil {
		attempt.model = model
		attempt.native = native
	}
	blocked, _ := excludedModel(state.Policy, channel, native)
	hidden := false
	for _, a := range state.Policy.Aliases[channel] {
		if a.Name == native && a.Alias != model {
			hidden = true
		}
	}
	if blocked || hidden {
		writeJSON(w, 404, map[string]string{"error": "Model is disabled or has a different public alias"})
		return
	}
	candidates := []storedAccount{}
	catalogFailed := false
	for _, a := range state.Accounts {
		if a.Disabled || a.Provider != channel || !routableAuth(a) {
			continue
		}
		models, err := s.accountModels(r.Context(), a)
		if err != nil {
			catalogFailed = true
			continue
		}
		for _, m := range models {
			if m.ID == native {
				candidates = append(candidates, a)
				break
			}
		}
	}
	if len(candidates) == 0 {
		status := 404
		message := "No enabled account advertises this model for this endpoint"
		if catalogFailed {
			status = 502
			message = "Provider catalogs unavailable; cannot select an account"
		}
		writeJSON(w, status, map[string]string{"error": message})
		return
	}
	// Preserve billing/authentication boundaries: never silently fall back from
	// subscription access to a paid API-key account.
	mode := candidates[0].AuthMode
	filtered := candidates[:0]
	for _, a := range candidates {
		if a.AuthMode == mode {
			filtered = append(filtered, a)
		}
	}
	candidates = filtered
	var stream bool
	if raw, ok := payload["stream"]; ok && json.Unmarshal(raw, &stream) != nil {
		writeJSON(w, 400, map[string]string{"error": "stream must be a boolean"})
		return
	}
	if attempt != nil {
		attempt.stream = stream
	}
	if channel == "codex" && mode != "api_key" {
		if !stream {
			writeJSON(w, 400, map[string]string{"error": "Codex requests require stream: true and store: false"})
			return
		}
		if raw, ok := payload["store"]; ok && string(raw) != "false" {
			writeJSON(w, 400, map[string]string{"error": "Codex requests require store: false"})
			return
		}
		payload["store"] = json.RawMessage("false")
	}
	if channel == "codex" {
		var text string
		if json.Unmarshal(payload["input"], &text) == nil {
			payload["input"], _ = json.Marshal([]map[string]string{{"role": "user", "content": text}})
		}
		if mode == "codex" {
			if _, ok := payload["instructions"]; !ok {
				payload["instructions"] = json.RawMessage(`""`)
			}
		}
	}
	payload["model"], _ = json.Marshal(native)
	pool := candidates
	candidates = s.usableAccounts(r.Context(), pool, native)
	resetTried := false
	if len(candidates) == 0 {
		resetTried = true
		candidates = s.resetExhaustedPool(r.Context(), pool, native)
		if len(candidates) == 0 {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, 429, map[string]string{"error": "Account usage limits reached; no usable reset was confirmed. Usage will be checked again automatically."})
			return
		}
	}
retryInference:
	start := int(s.sequence.Add(1)-1) % len(candidates)
	for attemptIndex := 0; attemptIndex < len(candidates); attemptIndex++ {
		a, err := s.accessAccount(r.Context(), candidates[(start+attemptIndex)%len(candidates)].ID)
		if err != nil || !routableAuth(a) {
			continue
		}
		target := "https://api.openai.com/v1/responses"
		if channel == "claude" {
			target = "https://api.anthropic.com/v1/messages"
		} else if a.AuthMode == "codex" {
			target = "https://chatgpt.com/backend-api/codex/responses"
		}
		req, err := requestJSON(r.Context(), target, payload)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "Invalid inference request"})
			return
		}
		providerHeaders(req, a)
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		// Header allowlist prevents downstream credentials, cookies and arbitrary
		// routing headers from reaching provider services.
		if channel == "claude" && r.Header.Get("anthropic-beta") != "" {
			beta := r.Header.Get("anthropic-beta")
			if a.AuthMode != "api_key" {
				beta = "oauth-2025-04-20," + beta
			}
			req.Header.Set("anthropic-beta", beta)
		}
		if attempt != nil {
			attempt.provider = a.Provider
			attempt.accountID = a.ID
		}
		resp, err := s.streamClient.Do(req)
		if err != nil {
			if attempt != nil {
				attempt.dispatchFailed = true
			}
			writeJSON(w, 502, map[string]string{"error": "Provider connection failed"})
			return
		}
		if (resp.StatusCode == 429 || resp.StatusCode == 503) && attemptIndex+1 < len(candidates) {
			resp.Body.Close()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			if resp.StatusCode == 429 && !resetTried && nativeUsage(a) {
				resetTried = true
				candidates = s.resetExhaustedPool(r.Context(), pool, native)
				if len(candidates) > 0 {
					goto retryInference
				}
			}
			if retry := resp.Header.Get("Retry-After"); retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			status := resp.StatusCode
			if status >= 300 && status < 400 {
				status = 502
			}
			writeJSON(w, status, map[string]any{"error": map[string]string{"type": "provider_error", "message": http.StatusText(status)}})
			return
		}
		defer resp.Body.Close()
		for _, header := range []string{"Content-Type", "Retry-After", "X-Request-ID"} {
			if value := resp.Header.Get(header); value != "" {
				w.Header().Set(header, value)
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		if attempt != nil {
			attempt.usage.begin(stream)
		}
		// Once headers/body are delivered the request is never retried. Terminal
		// SSE errors and disconnects reach the caller unchanged.
		buf := make([]byte, 32<<10)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if attempt != nil {
					attempt.usage.observe(buf[:n])
				}
				if _, err = w.Write(buf[:n]); err != nil {
					if attempt != nil {
						attempt.outcome = outcomeIncomplete
					}
					return
				}
				if stream {
					_ = http.NewResponseController(w).Flush()
				}
			}
			if readErr != nil {
				if attempt != nil {
					if readErr == io.EOF {
						attempt.bodyComplete = true
						attempt.usage.complete()
						if stream && !attempt.usage.terminal && !attempt.usage.terminalFailure {
							attempt.outcome = outcomeIncomplete
						}
					} else {
						attempt.outcome = outcomeError
					}
				}
				return
			}
		}
	}
	writeJSON(w, 503, map[string]string{"error": "No usable account remains. Reconnect or enable an account."})
}
