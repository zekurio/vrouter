package gateway

import (
	"net/http"
	"os"
	"sync/atomic"
	"testing"
)

func TestAdmissionWriteFailureFailsClosedAndRecovers(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	var providerCalls atomic.Int32
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return oauthReply(200, nativeModels("model")), nil
		}
		providerCalls.Add(1)
		return nativeSSE("data: done\n\n"), nil
	}))
	key, secret := createKeyOn(t, s, defaultGatewayID, "durable", int64Ptr(5), nil)

	if err := os.RemoveAll(s.cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 503 {
		t.Fatalf("unpersisted admission was allowed: %d %s", w.Code, w.Body)
	}
	if providerCalls.Load() != 0 {
		t.Fatal("admission failure still reached the provider")
	}
	if k, ok := findKeyView(keyList(t, s, defaultGatewayID), key.ID); !ok || k.UsedRequests != 0 {
		t.Fatalf("failed admission consumed a request: %+v", k)
	}

	// Once the directory is writable again the key works and the counter is
	// persisted before forwarding.
	if err := os.MkdirAll(s.cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if w := nativeCall(s, "POST", "/v1/responses", secret, `{"model":"model","stream":true}`); w.Code != 200 {
		t.Fatalf("recovered admission: %d %s", w.Code, w.Body)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("provider calls %d", providerCalls.Load())
	}
	if k, ok := findKeyView(keyList(t, s, defaultGatewayID), key.ID); !ok || k.UsedRequests != 1 {
		t.Fatalf("recovered counter: %+v", k)
	}
}

func TestSettlementWriteFailureBlocksTokenBudgetUntilOwnerAction(t *testing.T) {
	s := oauthServer(t)
	key, _ := createKeyOn(t, s, defaultGatewayID, "metered", nil, int64Ptr(100))
	principal := inferenceKey{GatewayID: defaultGatewayID, KeyID: key.ID, KeyName: key.Name}
	if allowed, status, message := s.manager.reserve(principal); !allowed {
		t.Fatalf("initial reserve denied: %d %s", status, message)
	}
	if k, ok := findKeyView(keyList(t, s, defaultGatewayID), key.ID); !ok || k.UsedRequests != 1 {
		t.Fatalf("reservation not persisted: %+v", k)
	}

	// Break persistence, then settle as a forwarded 2xx response with known
	// usage. The write must fail closed: the key becomes uncertain and the
	// process stops trusting its measured budget.
	if err := os.RemoveAll(s.cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	totals := usageTotals{Input: 3, Output: 4, Total: 7, Known: true}
	s.manager.settle(principal, telemetryRecord{ID: "x", GatewayID: defaultGatewayID, Outcome: outcomeSuccess, Status: 200}, totals, false, true, true)
	if !s.manager.degraded.Load() {
		t.Fatal("settlement failure did not degrade the manager")
	}
	if k, ok := findKeyView(keyList(t, s, defaultGatewayID), key.ID); !ok || !k.UsageUncertain {
		t.Fatalf("settlement failure did not mark uncertain: %+v", k)
	}
	if allowed, status, _ := s.manager.reserve(principal); allowed || status != http.StatusTooManyRequests {
		t.Fatalf("uncertain key was admitted: %v %d", allowed, status)
	}

	// The owner's token-budget change is the documented recovery action.
	if err := os.MkdirAll(s.cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if w := gatewayCall(s, defaultGatewayID, "PATCH", "/api/keys/"+key.ID, `{"limitTokens":200}`); w.Code != 200 {
		t.Fatalf("budget change: %d %s", w.Code, w.Body)
	}
	if s.manager.degraded.Load() {
		t.Fatal("degraded flag survived a successful write")
	}
	if allowed, status, message := s.manager.reserve(principal); !allowed {
		t.Fatalf("key did not recover: %d %s", status, message)
	}
}
