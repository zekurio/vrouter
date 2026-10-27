package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
)

// The dashboard keeps a random session token in an HTTP-only cookie. SQLite
// stores only its hash, so signing out, disabling a user, or a lost database
// copy ends the session without exposing a usable token.
const (
	sessionCookie   = "vrouter_session"
	sessionLifetime = 7 * 24 * time.Hour
	maxCookieBytes  = 128
)

func sessionHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// startSession stores a new session and drops expired ones.
func (s *usageStore) startSession(ctx context.Context, hash, userID string, expires time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires<=?`, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions VALUES (?,?,?)`, hash, userID, expires.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// sessionUser returns the user of a live session, or "" when there is none.
func (s *usageStore) sessionUser(ctx context.Context, hash string, now time.Time) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM sessions WHERE hash=? AND expires>?`, hash, now.Unix()).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return userID, err
}

func (s *usageStore) endSession(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE hash=?`, hash)
	return err
}

func (s *usageStore) endUserSessions(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
	return err
}

// adoptUnowned moves the history of keys no user owned to a user. Totals
// that land in a bucket the user already has are added to it.
func (s *usageStore) adoptUnowned(ctx context.Context, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	statements := []string{
		`UPDATE usage_pending SET user_id=? WHERE user_id=''`,
		`INSERT INTO usage_buckets SELECT ?,bucket,provider,model,key_id,speed,scope,key_name,` + strings.Join(usageColumns(), ",") + ` FROM usage_buckets WHERE user_id='' ` + usageMerge(),
		`UPDATE usage_requests SET user_id=? WHERE user_id=''`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, userID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_buckets WHERE user_id=''`); err != nil {
		return err
	}
	return tx.Commit()
}

// setCookie writes a dashboard cookie. Secure follows the public URL, the
// only origin the dashboard is served from.
func (s *server) setCookie(w http.ResponseWriter, name, value, path string, maxAge int, sameSite http.SameSite) {
	//nolint:gosec // Secure is set for every https public URL; a plain-http one cannot carry it
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.PublicURL, "https://"),
		SameSite: sameSite,
	})
}

// beginSession signs the browser in as the user.
func (s *server) beginSession(ctx context.Context, w http.ResponseWriter, user userRecord) error {
	token := randomToken()
	if err := s.store.data.usage.startSession(ctx, sessionHash(token), user.ID, time.Now().Add(sessionLifetime)); err != nil {
		return err
	}
	s.setCookie(w, sessionCookie, token, "/api", int(sessionLifetime/time.Second), http.SameSiteStrictMode)
	return nil
}

// sessionUser returns the signed-in user of a request. A write must name its
// origin, which authorize has already matched against the public URL.
func (s *server) sessionUser(r *http.Request) (userRecord, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" || len(cookie.Value) > maxCookieBytes {
		return userRecord{}, false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") == "" {
		return userRecord{}, false
	}
	id, err := s.store.data.usage.sessionUser(r.Context(), sessionHash(cookie.Value), time.Now())
	if err != nil || id == "" {
		return userRecord{}, false
	}
	return s.activeUser(id)
}

func (s *server) sessionStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, sessionView(currentUser(r)))
}

// endSession signs the browser out. It needs no live session, so a stale
// cookie can always be cleared.
func (s *server) endSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.sameOrigin(r) {
		writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
		return
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" && len(cookie.Value) <= maxCookieBytes {
		if err := s.store.data.usage.endSession(r.Context(), sessionHash(cookie.Value)); err != nil {
			writeJSON(w, 500, map[string]string{"error": "Could not sign out. Try again."})
			return
		}
	}
	s.setCookie(w, sessionCookie, "", "/api", -1, http.SameSiteStrictMode)
	writeJSON(w, 200, map[string]string{"status": "signed out"})
}
