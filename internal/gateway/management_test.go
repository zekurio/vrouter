package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func TestDashboardSessionCookie(t *testing.T) {
	s := testServer(t, Config{AdminToken: "admin-secret"})
	request := func(method, path, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "vrouter.test"
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	const origin = "http://vrouter.test"
	if got := request("GET", "/api/gateways", "", "", nil).Code; got != 401 {
		t.Fatalf("signed out: %d", got)
	}
	if got := request("POST", "/api/auth/session", `{"token":"wrong"}`, origin, nil); got.Code != 401 || len(got.Result().Cookies()) != 0 {
		t.Fatalf("wrong token: %d", got.Code)
	}
	if got := request("POST", "/api/auth/session", `{"token":"admin-secret"}`, "http://other.test", nil).Code; got != 403 {
		t.Fatalf("cross-origin sign-in: %d", got)
	}
	signedIn := request("POST", "/api/auth/session", `{"token":"admin-secret"}`, origin, nil)
	cookies := signedIn.Result().Cookies()
	if signedIn.Code != 200 || len(cookies) != 1 {
		t.Fatalf("sign-in: %d %s", signedIn.Code, signedIn.Body)
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || strings.Contains(cookie.Value, "admin-secret") {
		t.Fatalf("session cookie is not private: %+v", cookie)
	}
	if got := request("GET", "/api/gateways", "", "", cookie).Code; got != 200 {
		t.Fatalf("read with session: %d", got)
	}
	if got := request("POST", "/api/keys", `{"name":"k"}`, origin, cookie).Code; got != 201 {
		t.Fatalf("write with session: %d", got)
	}
	// A cookie write must name its origin, and that origin must be this host.
	if got := request("POST", "/api/keys", `{"name":"k"}`, "", cookie).Code; got != 401 {
		t.Fatalf("write without origin: %d", got)
	}
	if got := request("POST", "/api/keys", `{"name":"k"}`, "http://other.test", cookie).Code; got != 403 {
		t.Fatalf("cross-origin write: %d", got)
	}
	signedOut := request("DELETE", "/api/auth/session", "", origin, cookie)
	if cleared := signedOut.Result().Cookies(); signedOut.Code != 200 || len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Fatalf("sign-out: %d", signedOut.Code)
	}
	// Changing the admin token ends every session.
	s.cfg.AdminToken = "rotated"
	if got := request("GET", "/api/gateways", "", "", cookie).Code; got != 401 {
		t.Fatalf("session after token change: %d", got)
	}
}

func TestDashboardKeepsLastQuotaWhenProbeFails(t *testing.T) {
	s := testServer(t, Config{})
	account := storedAccount{ID: "a", Provider: "codex", AuthMode: "codex", AccessToken: "secret", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.store.update(func(d *diskState) error {
		d.Accounts = append(d.Accounts, account)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	status := 200
	reset := time.Now().Add(time.Hour).Unix()
	s.client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		body := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":18000,"reset_at":%d}}}`, reset)
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if q := s.nativeQuota(cancelled, account); q.Error == "" {
		t.Fatal("cancelled probe succeeded")
	}
	if q := s.nativeQuota(context.Background(), account); q.Error != "" || len(q.Windows) != 1 {
		t.Fatalf("quota = %+v; a cancelled caller poisoned the cache", q)
	}
	status = 429
	s.quotaMu.Lock()
	delete(s.quotas, account.ID)
	s.quotaMu.Unlock()
	if q := s.nativeQuota(context.Background(), account); q.Error == "" || len(q.Windows) != 0 {
		t.Fatalf("routing quota kept stale windows: %+v", q)
	}
	shown := s.dashboardQuota(context.Background(), account)
	if shown.Error == "" || len(shown.Windows) != 1 || shown.Windows[0].Remaining != 60 {
		t.Fatalf("dashboard quota = %+v", shown)
	}
}
