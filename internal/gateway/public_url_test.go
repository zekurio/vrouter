package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicURLConfig(t *testing.T) {
	for _, value := range []string{
		"router.example", "//router.example", "ftp://router.example", "https://",
		"https://user:secret@router.example", "https://router.example/app",
		"https://router.example/%2f", "https://router.example?x=1", "https://router.example?",
		"https://router.example#fragment", "https://router.example#",
		"https://router.example:0", "https://router.example:65536", "https://router.example:",
	} {
		t.Run(value, func(t *testing.T) {
			if h, err := New(Config{PublicURL: value, DataDir: t.TempDir()}, testAssets()); err == nil {
				h.(*server).Close()
				t.Fatal("invalid public URL accepted")
			}
		})
	}
	for _, value := range []string{"", "http://localhost:8080", "https://router.example/", "http://[::1]:8080/"} {
		s := testServer(t, Config{PublicURL: value})
		w := call(t, s, "GET", "/api/auth", "", "")
		var body struct{ Mode, PublicURL string }
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.PublicURL != strings.TrimSuffix(value, "/") || body.Mode != "local" {
			t.Fatalf("unexpected auth status: %s", w.Body.String())
		}
		// Merely setting a public URL must not disable local-only management.
		r := httptest.NewRequest("GET", "https://router.example/api/keys", nil)
		w = httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("public URL bypassed authentication: %d", w.Code)
		}
	}
}

func TestPublicURLManagementOrigin(t *testing.T) {
	s := testServer(t, Config{PublicURL: "https://router.example/", ExternalAuth: true})
	for _, tc := range []struct {
		origin, host string
		want         int
	}{
		{"https://router.example", "router.example", 201},
		{"http://router.example", "router.example", 403},
		{"https://other.example", "other.example", 403},
		{"https://router.example", "127.0.0.1:8080", 403},
		{"", "router.example", 201}, // Non-browser management clients still work.
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/keys", strings.NewReader(`{"name":"origin-test"}`))
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("origin %q host %q: got %d, want %d", tc.origin, tc.host, w.Code, tc.want)
		}
	}
	if w := call(t, s, "GET", "/v1/models", "", ""); w.Code != 401 {
		t.Fatalf("inference without a key: %d", w.Code)
	}
}

func TestAppURLBehindProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/oauth/codex", nil)
	r.Header.Set("Forwarded", "host=attacker.example;proto=https")
	r.Header.Set("X-Forwarded-Host", "attacker.example")
	r.Header.Set("X-Forwarded-Proto", "https")
	s := &server{cfg: Config{PublicURL: "https://router.example"}}
	if got := s.appURL(r); got != "https://router.example" {
		t.Fatalf("proxy return URL: %s", got)
	}
	s.cfg.PublicURL = ""
	if got := s.appURL(r); got != "http://127.0.0.1:8080" {
		t.Fatalf("trusted forwarded headers: %s", got)
	}
	r.Header.Set("Origin", "http://localhost:5173")
	if got := s.appURL(r); got != "http://localhost:5173" {
		t.Fatalf("development return URL: %s", got)
	}
}
