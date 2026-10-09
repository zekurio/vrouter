package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagementIsScopedToSelectedGateway(t *testing.T) {
	s := testServer(t, Config{ExternalAuth: true})
	err := s.store.data.update(func(d *diskData) error {
		d.Registry.Gateways = append(d.Registry.Gateways, gatewayRecord{ID: "other", Name: "Other", OwnerID: localAdminID, CreatedAt: time.Now().UTC()})
		d.States["other"] = diskState{Version: storeStateVersion}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, gateway string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set(gatewayHeader, gateway)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	created := request("POST", "/api/keys", `{"name":"scoped"}`, "other")
	var out struct{ Key keyView }
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &out) != nil {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	if strings.Contains(request("GET", "/api/keys", "", "").Body.String(), out.Key.ID) {
		t.Fatal("default gateway lists another gateway's key")
	}
	if !strings.Contains(request("GET", "/api/keys", "", "other").Body.String(), out.Key.ID) {
		t.Fatal("selected gateway does not list its key")
	}
	if got := request("DELETE", "/api/keys/"+out.Key.ID, "", "").Code; got != 404 {
		t.Fatalf("default gateway deleted another gateway's key: %d", got)
	}
	if got := request("GET", "/api/keys", "", "missing").Code; got != 404 {
		t.Fatalf("unknown gateway: %d", got)
	}
	// The list ignores the selection, so a stale one can still recover.
	list := request("GET", "/api/gateways", "", "missing")
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"id":"other"`) || !strings.Contains(list.Body.String(), `"id":"default"`) {
		t.Fatalf("gateway list: %d %s", list.Code, list.Body)
	}
}
