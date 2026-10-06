package gateway

import (
	"io"
	"net/http"
	"testing"
	"testing/fstest"
	"time"
)

// A reservation that survives a restart cannot still be running: the key must
// come back uncertain so a future cap cannot be bypassed, and the owner's
// budget change must recover it.
func TestRestartWithUnfinishedReservationMarksUncertain(t *testing.T) {
	s := oauthServer(t)
	key, _ := createKeyOn(t, s, defaultGatewayID, "interrupted", nil, int64Ptr(100))
	principal := inferenceKey{GatewayID: defaultGatewayID, KeyID: key.ID, KeyName: key.Name}
	if allowed, status, message := s.manager.reserve(principal); !allowed {
		t.Fatalf("initial reserve denied: %d %s", status, message)
	}
	if k, ok := findKeyView(keyList(t, s, defaultGatewayID), key.ID); !ok {
		t.Fatal(k)
	}

	cfg := s.cfg
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := New(cfg, fstest.MapFS{"index.html": {Data: []byte("vrouter")}})
	if err != nil {
		t.Fatal(err)
	}
	restarted := h.(*server)
	defer restarted.Close()

	keys := keyList(t, restarted, defaultGatewayID)
	recovered, ok := findKeyView(keys, key.ID)
	if !ok || !recovered.UsageUncertain {
		t.Fatalf("unfinished reservation did not become uncertain: %+v", keys)
	}
	for _, stored := range restarted.manager.registry.snapshot().Keys {
		if stored.ID == key.ID && stored.InFlight != 0 {
			t.Fatalf("restart retained an in-flight count: %+v", stored)
		}
	}
	if allowed, status, _ := restarted.manager.reserve(principal); allowed || status != http.StatusTooManyRequests {
		t.Fatalf("uncertain key admitted after restart: %v %d", allowed, status)
	}
	if w := gatewayCall(restarted, defaultGatewayID, "PATCH", "/api/keys/"+key.ID, `{"limitTokens":500}`); w.Code != 200 {
		t.Fatalf("budget change: %d %s", w.Code, w.Body)
	}
	if allowed, status, message := restarted.manager.reserve(principal); !allowed {
		t.Fatalf("key did not recover: %d %s", status, message)
	}
}

// Adding a cap while an uncapped request is running must not let a second
// request through: every reservation is counted, not only capped ones.
func TestCapAddedDuringUncappedRequestIsEnforced(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
	}))
	key, secret := createKeyOn(t, s, defaultGatewayID, "late-cap", nil, nil)
	done := make(chan int, 1)
	go func() { done <- nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`).Code }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not start")
	}
	if w := gatewayCall(s, defaultGatewayID, "PATCH", "/api/keys/"+key.ID, `{"limitTokens":1000}`); w.Code != 200 {
		t.Fatalf("cap change: %d %s", w.Code, w.Body)
	}
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 429 {
		t.Fatalf("in-flight uncapped request invisible to the new cap: %d %s", w.Code, w.Body)
	}
	close(release)
	if code := <-done; code != 200 {
		t.Fatalf("first request: %d", code)
	}
	// The settled request releases its own reservation and its measured
	// tokens count toward the new cap.
	if k, ok := findKeyView(keyList(t, s, defaultGatewayID), key.ID); !ok || k.UsedTokens != 2 || k.UsageUncertain {
		t.Fatalf("settled key %+v", k)
	}
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("post-settle request: %d %s", w.Code, w.Body)
	}
}
