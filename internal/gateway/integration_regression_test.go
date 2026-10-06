package gateway

// Root integration review regressions. Additional review details are in
// /tmp/vrouter-review-notes.txt; keep these public-path assertions intact.

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Admission failures must not settle another request's reservation.
func TestIntegrationRejectedRequestKeepsActiveReservation(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return nativeSSE("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}\n\n"), nil
	}))
	_, secret := createKeyOn(t, s, defaultGatewayID, "metered", nil, int64Ptr(100))
	done := make(chan int, 1)
	go func() { done <- nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`).Code }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("request did not reach provider")
	}
	defer func() {
		close(release)
		if code := <-done; code != 200 {
			t.Errorf("first response %d", code)
		}
	}()
	for i := 0; i < 2; i++ {
		if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 429 {
			t.Errorf("concurrent attempt %d: got %d, want 429", i, w.Code)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("%d requests reached provider with one token budget active", calls.Load())
	}
}

func TestIntegrationPartialClaudeUsageCannotReopenBudget(t *testing.T) {
	s := oauthServer(t)
	a := nativeAccount("claude-a")
	a.Provider = "claude"
	a.AuthMode = "api_key"
	nativeSeed(t, s, a)
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, map[string]any{"data": []map[string]string{{"id": "claude-test"}}}), nil
		}
		return nativeSSE("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"unfinished\"}}\n\n"), nil
	}))
	_, secret := createKeyOn(t, s, defaultGatewayID, "partial", nil, int64Ptr(1000))
	if w := nativeCall(s, "POST", "/v1/messages", secret, `{"model":"claude-test","stream":true,"max_tokens":100,"messages":[]}`); w.Code != 200 {
		t.Fatalf("stream status %d: %s", w.Code, w.Body)
	}
	records := readTelemetry(t, s)
	if records.Requests[0].Outcome == outcomeSuccess {
		t.Error("EOF without message_stop recorded as success")
	}
	if w := nativeCall(s, "POST", "/v1/messages", secret, `{"model":"claude-test","stream":true,"max_tokens":100,"messages":[]}`); w.Code != 429 {
		t.Errorf("partial accounting reopened budget: %d", w.Code)
	}
}

func TestIntegrationAmbiguousDispatchBlocksMeasuredBudget(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		return nil, errors.New("connection lost after dispatch")
	}))
	_, secret := createKeyOn(t, s, defaultGatewayID, "ambiguous", nil, int64Ptr(1000))
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 502 {
		t.Fatalf("first dispatch status %d", w.Code)
	}
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 429 {
		t.Errorf("ambiguous dispatched usage reopened budget: %d", w.Code)
	}
}

func TestIntegrationOversizedModelCannotDisableOtherKeys(t *testing.T) {
	s := oauthServer(t)
	_, secret := createKeyOn(t, s, defaultGatewayID, "oversized", nil, nil)
	_, measured := createKeyOn(t, s, defaultGatewayID, "unrelated", nil, int64Ptr(100))
	w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"`+strings.Repeat("x", 300)+`","stream":true}`)
	if w.Code != 400 && w.Code != 404 {
		t.Fatalf("long model status %d", w.Code)
	}
	if w := nativeCall(s, "POST", "/v1/responses", measured, `{"model":"missing","stream":true}`); w.Code != 404 {
		t.Errorf("oversized telemetry broke unrelated key: %d %s", w.Code, w.Body)
	}
}
