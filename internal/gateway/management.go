package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

// The dashboard signs in once and then presents this cookie, so the browser
// never keeps the admin token. Scripts keep using the bearer token.
const (
	sessionCookie = "vrouter_session"
	sessionMaxAge = 30 * 24 * 60 * 60
)

// sessionValue is derived from the admin token. It survives a restart and
// stops working as soon as the token changes.
func (s *server) sessionValue() string {
	mac := hmac.New(sha256.New, []byte(s.cfg.AdminToken))
	mac.Write([]byte("vrouter dashboard session v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// hasSession accepts the cookie on a write only when the browser named its
// origin, which authorize has already matched against this host.
func (s *server) hasSession(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || !tokenEqual(cookie.Value, s.sessionValue()) {
		return false
	}
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Header.Get("Origin") != ""
}

func (s *server) setSession(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	origin, _ := url.Parse(r.Header.Get("Origin"))
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/api",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil || (origin != nil && origin.Scheme == "https"),
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *server) startSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.sameOrigin(r) {
		writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if s.cfg.AdminToken == "" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || !tokenEqual(body.Token, s.cfg.AdminToken) {
		writeJSON(w, 401, map[string]string{"error": "That admin token was not accepted."})
		return
	}
	s.setSession(w, r, s.sessionValue(), sessionMaxAge)
	writeJSON(w, 200, map[string]string{"status": "signed in"})
}

func (s *server) endSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.sameOrigin(r) {
		writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
		return
	}
	s.setSession(w, r, "", -1)
	writeJSON(w, 200, map[string]string{"status": "signed out"})
}

// staticHandler serves the embedded frontend. Only files are served, and
// index.html is revalidated on every load so a new build is picked up.
func staticHandler(assets fs.FS) http.HandlerFunc {
	files := http.FileServer(http.FS(assets))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(405)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if info, err := fs.Stat(assets, path); err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		if path == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}
}

func (s *server) authStatus(w http.ResponseWriter, r *http.Request) {
	mode := "local"
	if s.cfg.ExternalAuth {
		mode = "external"
	}
	if s.cfg.AdminToken != "" {
		mode = "token"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"mode": mode, "publicUrl": s.cfg.PublicURL})
}
