package gateway

import (
	"context"
	"errors"
	"html/template"
	"net"
	"net/http"
	"time"
)

// Callback listeners use the provider's registered loopback address.
func (s *server) openCallback(id, provider, authMode string) (string, error) {
	port := codexNativeCallbackPort
	path := "/auth/callback"
	host := "127.0.0.1"
	switch {
	case provider == "claude" && authMode == "oauth":
		port = "54545"
		path = "/callback"
		host = "localhost"
	case provider == "codex" && authMode == "codex":
	default:
		return "", errors.New("unsupported sign-in method")
	}
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:"+port)
	if err != nil {
		return "", errors.New("callback listener unavailable")
	}
	if s.callbacks == nil {
		s.callbacks = map[string][]*http.Server{}
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.receiveCallback(w, r, id, path) }), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	s.callbacks[id] = []*http.Server{srv}
	go func() { _ = srv.Serve(l) }()
	return "http://" + net.JoinHostPort(host, fmtPort(l.Addr())) + path, nil
}
func fmtPort(addr net.Addr) string { _, port, _ := net.SplitHostPort(addr.String()); return port }
func (s *server) sweepOAuth() {
	for id, session := range s.oauth {
		if time.Now().After(session.Expires) {
			delete(s.oauth, id)
		}
	}
	for id, servers := range s.callbacks {
		session, ok := s.oauth[id]
		if ok && !session.Completed && session.Error == "" {
			continue
		}
		for _, srv := range servers {
			go func(srv *http.Server) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = srv.Shutdown(ctx)
			}(srv)
		}
		delete(s.callbacks, id)
	}
}

func (s *server) receiveCallback(w http.ResponseWriter, r *http.Request, id, path string) {
	page := callbackPage{Title: "Sign-in link not recognized", Body: "Start again from Add account in vrouter."}
	status := 400
	defer func() {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		w.WriteHeader(status)
		_ = callbackTemplate.Execute(w, page)
	}()
	if r.Method != http.MethodGet || r.URL.Path != path || len(r.URL.RawQuery) > 16<<10 {
		return
	}
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	session, ok := s.oauth[id]
	if !ok {
		return
	}
	// State is checked before showing a saved return address or exchanging codes.
	if !tokenEqual(r.URL.Query().Get("state"), session.State) {
		return
	}
	page.Return = session.Return
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.completeOAuth(ctx, id, r.URL.Query()); err != nil {
		page.Title = "Could not finish sign-in"
		page.Body = err.Error()
		return
	}
	page.Title = "Account connected"
	page.Body = "You can close this tab."
	page.OK = true
	status = 200
}

type callbackPage struct {
	Title, Body, Return string
	OK                  bool
}

//nolint:gochecknoglobals // parsed once at startup
var callbackTemplate = template.Must(template.New("callback").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · vrouter</title>
<style>
:root { color-scheme: dark light; --bg: #000; --muted: #8c8c8c; --text: #f2f2f2; --solid: #f2f2f2; --solid-text: #0a0a0a; --accent: #79bce9; --state: #d4b57a; }
@media (prefers-color-scheme: light) {
  :root { --bg: #f1f1f1; --muted: #6b6b6b; --text: #0d0d0d; --solid: #0d0d0d; --solid-text: #fff; --accent: #2b7cb3; --state: #a57a22; }
}
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; display: flex; flex-direction: column; background: var(--bg); color: var(--text); font: 13px/1.6 "Inter", system-ui, -apple-system, "Segoe UI", sans-serif; -webkit-font-smoothing: antialiased; }
header { display: flex; align-items: center; gap: 10px; padding: 21px 30px; font-size: 16px; font-weight: 650; letter-spacing: -0.3px; }
main { flex: 1; display: flex; flex-direction: column; justify-content: center; width: 100%; max-width: 420px; margin: 0 auto; padding: 0 24px 140px; }
.state { display: flex; align-items: center; gap: 9px; margin-bottom: 14px; color: var(--muted); font-size: 12px; }
.state::before { content: ""; width: 5px; height: 5px; border-radius: 50%; background: var(--state); }
h1 { margin: 0; font-size: 26px; line-height: 1.2; font-weight: 600; letter-spacing: -0.6px; }
p { margin: 12px 0 0; color: var(--muted); font-size: 14px; }
a { display: inline-flex; align-items: center; height: 36px; margin-top: 26px; padding: 0 14px; border-radius: 8px; background: var(--solid); color: var(--solid-text); font-size: 13px; font-weight: 550; text-decoration: none; align-self: flex-start; }
a:focus-visible { outline: 2px solid var(--accent); outline-offset: 3px; }
</style>
</head>
<body>
<header>
<svg width="30" height="30" viewBox="0 0 30 30" fill="none" aria-hidden="true"><path d="M3 4v7h10v9h14v7M7 4v3h10v9h10" stroke="currentColor" stroke-width="3" stroke-linejoin="round"/><path d="M10 12v5h5" stroke="var(--accent)" stroke-width="3"/></svg>
vrouter
</header>
<main>
{{if not .OK}}<div class="state">Not connected</div>{{end}}
<h1>{{.Title}}</h1>
<p>{{.Body}}</p>
{{if .Return}}<a href="{{.Return}}">Back to vrouter</a>{{end}}
</main>
</body>
</html>
`))
