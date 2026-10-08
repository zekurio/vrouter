package gateway

import (
	"net/http"
	"strings"
	"time"
)

// serveInference is the /v1 entry point. It selects the gateway from the
// presented API key, never from a header, and hands the request to the engine
// that owns the key.
func (s *server) serveInference(w http.ResponseWriter, r *http.Request) {
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
	if s.manager != nil {
		if record, ok := s.manager.keyByHash(key); ok {
			if record.expired(time.Now()) {
				writeJSON(w, 401, map[string]string{"error": keyExpiredMessage})
				return inferenceKey{}, false
			}
			return inferenceKey{GatewayID: record.GatewayID, KeyID: record.ID, KeyName: record.Name}, true
		}
	}
	if s.manager == nil || !s.manager.hasAnyKeys() {
		writeJSON(w, 503, map[string]string{"error": "Create a managed gateway API key to enable client requests"})
		return inferenceKey{}, false
	}
	writeJSON(w, 401, map[string]string{"error": "A valid vrouter client API key is required"})
	return inferenceKey{}, false
}

// serveInferenceAuthorized selects the model and checks its provider quota.
// Each supported POST settles one telemetry row, including failed requests.
func (s *server) serveInferenceAuthorized(w http.ResponseWriter, r *http.Request, principal inferenceKey) {
	if r.Method != http.MethodPost || inferenceProtocol(r.URL.Path) == "" {
		s.inference(w, r, nil)
		return
	}
	recorder := &attemptWriter{ResponseWriter: w}
	attempt := &inferenceAttempt{principal: principal, startedAt: time.Now().UTC()}
	prepared, status, message := s.prepareInference(recorder, r, attempt)
	if prepared == nil {
		writeJSON(recorder, status, protocolError(message))
		s.finishAttempt(attempt, recorder)
		return
	}
	principal.Provider = prepared.provider
	attempt.principal = principal
	unlock, available := s.lockPercentUsage(principal.Provider)
	if !available {
		w.Header().Set("Retry-After", "1")
		writeJSON(recorder, 429, map[string]string{"error": "This provider pool is measuring another request. Retry when it finishes."})
		s.finishAttempt(attempt, recorder)
		return
	}
	defer unlock()
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
	s.forwardInference(recorder, r, attempt, prepared)
	s.endPercentMeasurement(attempt)
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
	// Only a forwarded provider success can have consumed tokens.
	dispatched := attempt.provider != ""
	forwarded := dispatched && status >= 200 && status < 300
	trusted := attempt.usageTrusted()
	// usageKnown means trustworthy final counts. Counts observed without a
	// trustworthy final report are marked partial instead of pretending to be
	// complete or silently dropping what was seen.
	usageKnown := totals.Known && trusted
	usagePartial := totals.Known && !trusted
	usageAccepted := forwarded && usageKnown
	id, _ := secureID()
	record := telemetryRecord{
		ID:               id,
		StartedAt:        attempt.startedAt,
		GatewayID:        s.gatewayID,
		KeyID:            attempt.principal.KeyID,
		KeyName:          boundedField(attempt.principal.KeyName),
		Model:            boundedField(attempt.model),
		NativeModel:      boundedField(attempt.native),
		Provider:         boundedField(attempt.provider),
		AccountID:        boundedField(attempt.accountID),
		Status:           status,
		DurationMs:       attempt.durationMs,
		InputTokens:      totals.Input,
		OutputTokens:     totals.Output,
		CachedTokens:     totals.Cached,
		CacheWriteTokens: totals.CacheWrite,
		TotalTokens:      totals.Total,
		UsageKnown:       usageKnown,
		UsagePartial:     usagePartial,
		Stream:           attempt.stream,
		Outcome:          outcome,
	}
	s.manager.settle(attempt.principal, record, totals, usageAccepted, attempt.reserved, attempt.percentCharges, attempt.percentUnknown)
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
	if inferenceProtocol(r.URL.Path) == "" {
		writeJSON(w, 404, map[string]string{"error": "Supported endpoints are /v1/models, /v1/responses, /v1/messages and /v1/chat/completions"})
		return
	}
	w.Header().Set("Allow", "POST")
	writeJSON(w, 405, map[string]string{"error": "Use POST for inference"})
}

func (s *server) forwardInference(w http.ResponseWriter, r *http.Request, attempt *inferenceAttempt, prepared *preparedInference) {
	channel, native, payload, candidates := prepared.provider, prepared.native, prepared.payload, prepared.accounts
	upstreamStream := prepared.upstreamStream
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
		req, err := providerInferenceRequest(r.Context(), target, payload, a)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "Invalid inference request"})
			return
		}
		if upstreamStream {
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
		if err := s.beginPercentMeasurement(r.Context(), attempt, a); err != nil {
			writeJSON(w, 503, map[string]string{"error": err.Error()})
			return
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
			s.endPercentMeasurement(attempt)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			providerError := readProviderError(resp, a)
			resp.Body.Close()
			s.endPercentMeasurement(attempt)
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
			writeJSON(w, status, providerError)
			return
		}
		defer resp.Body.Close()
		if attempt != nil {
			attempt.percentForwarded = true
			attempt.usage.begin(upstreamStream)
		}
		s.deliverInference(w, r, resp, attempt, prepared, a)
		return
	}
	writeJSON(w, 503, map[string]string{"error": "No usable account remains. Reconnect or enable an account."})
}
