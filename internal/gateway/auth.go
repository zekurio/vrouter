package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// The dashboard signs in once with the admin key and then presents this
// cookie, so the browser never keeps the key. Scripts send the key itself as
// a bearer token.
const (
	sessionCookie = "vrouter_session"
	sessionMaxAge = 30 * 24 * 60 * 60
	// minAdminKey keeps a guessable key from guarding the provider
	// credentials. Sign-in has no attempt limit.
	minAdminKey = 16
)

func validateAdminKey(key string) error {
	if len(key) < minAdminKey || len(key) > 512 || strings.TrimSpace(key) != key {
		return errors.New("the admin key must be 16 to 512 characters without surrounding whitespace")
	}
	return nil
}

func tokenEqual(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

// sessionValue is derived from the admin key. It survives a restart and stops
// working as soon as the key changes.
func (s *server) sessionValue() string {
	mac := hmac.New(sha256.New, []byte(s.cfg.AdminKey))
	mac.Write([]byte("vrouter dashboard session v2"))
	return hex.EncodeToString(mac.Sum(nil))
}

// hasSession accepts the cookie on a write only when the browser named its
// origin, which sameOrigin has already matched against this host.
func (s *server) hasSession(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || !tokenEqual(cookie.Value, s.sessionValue()) {
		return false
	}
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Header.Get("Origin") != ""
}

// hasAdminKey reports whether the request carries the admin key as a bearer
// token.
func (s *server) hasAdminKey(r *http.Request) bool {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && tokenEqual(value, s.cfg.AdminKey)
}

// sameOrigin rejects writes a browser sent from another origin.
func (s *server) sameOrigin(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	header := r.Header.Get("Origin")
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if header == "" {
		return true
	}
	origin, err := url.Parse(header)
	return err == nil && origin.Host == r.Host && (origin.Scheme == "http" || origin.Scheme == "https") && (s.cfg.PublicURL == "" || header == s.cfg.PublicURL)
}

// authorize admits requests that carry the admin key or a dashboard session.
func (s *server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.sameOrigin(r) {
			writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
			return
		}
		if !s.hasAdminKey(r) && !s.hasSession(r) {
			writeJSON(w, 401, map[string]string{"error": "Sign in with the vrouter admin key"})
			return
		}
		next(w, r)
	}
}

// setSession writes or clears the session cookie. Secure follows the public
// URL, or the browser's own scheme when none is set, so plain-HTTP loopback
// sessions still work.
func (s *server) setSession(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	//nolint:gosec // Secure is set whenever the dashboard is served over https
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/api",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.appURL(r), "https://"),
		SameSite: http.SameSiteStrictMode,
	})
}

// authStatus tells the dashboard whether it is signed in and which origin
// clients should use.
func (s *server) authStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"publicUrl": s.cfg.PublicURL, "signedIn": s.hasSession(r) || s.hasAdminKey(r)})
}

func (s *server) startSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.sameOrigin(r) {
		writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || !tokenEqual(body.Key, s.cfg.AdminKey) {
		writeJSON(w, 401, map[string]string{"error": "That admin key was not accepted."})
		return
	}
	s.setSession(w, r, s.sessionValue(), sessionMaxAge)
	writeJSON(w, 200, map[string]string{"status": "signed in"})
}

// endSession signs the browser out. It needs no live session, so a stale
// cookie can always be cleared.
func (s *server) endSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.sameOrigin(r) {
		writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
		return
	}
	s.setSession(w, r, "", -1)
	writeJSON(w, 200, map[string]string{"status": "signed out"})
}
