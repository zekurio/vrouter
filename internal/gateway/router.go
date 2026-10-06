package gateway

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

// authenticator is the identity surface the tenant facade consumes. The login
// implementation owns OIDC discovery, sessions and role mapping; the facade
// only requires these three operations.
type authenticator interface {
	mode() string
	register(mux *http.ServeMux)
	authenticate(r *http.Request) (loginUser, bool)
}

// router is the multi-tenant facade used when an OIDC identity integration is
// configured. It authenticates management requests once, then dispatches to
// the selected gateway's management routes. Inference selects its gateway from
// the presented API key, never from a header.
type router struct {
	cfg     Config
	auth    authenticator
	root    *server
	manager *manager
	handler http.Handler
}

func newRouter(cfg Config, assets fs.FS, auth authenticator) (*router, error) {
	if cfg.Demo {
		return nil, errors.New("demo mode cannot be combined with OIDC sign-in")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("set VROUTER_DATA_DIR for multi-user mode: home directory is unavailable")
	}
	root, err := newGateway(cfg, assets)
	if err != nil {
		_ = closeIfPossible(auth)
		return nil, err
	}
	manager, err := openManager(root, assets)
	if err != nil {
		_ = root.Close()
		_ = closeIfPossible(auth)
		return nil, err
	}
	root.auth = auth
	r := &router{cfg: cfg, auth: auth, root: root, manager: manager}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	auth.register(mux)
	// Gateway listing and creation stay independent of any selection so a
	// stale or missing header can never hide them.
	mux.HandleFunc("GET /api/gateways", r.gateways)
	mux.HandleFunc("POST /api/gateways", r.gateways)
	mux.Handle("/api/", http.HandlerFunc(r.management))
	// Inference resolves its gateway from the API key; root handles both the
	// legacy VROUTER_API_KEY and every managed gateway key.
	mux.Handle("/v1/", http.HandlerFunc(root.serveInference))
	mux.HandleFunc("/v1beta/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"error": "This protocol is not supported by vrouter"})
	})
	mux.HandleFunc("/", staticHandler(assets))
	r.handler = mux
	return r, nil
}

func (r *router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	r.handler.ServeHTTP(w, req)
}

func (r *router) Close() error {
	// The root engine closes the shared manager and the authenticator.
	return r.root.Close()
}

// closeIfPossible calls an optional Close method. The identity worker keeps
// its Close signature private, so both common shapes are accepted.
func closeIfPossible(value any) error {
	switch closer := value.(type) {
	case interface{ Close() error }:
		return closer.Close()
	case interface{ Close() }:
		closer.Close()
	}
	return nil
}

func (r *router) gateways(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOriginWrite(req) {
		writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
		return
	}
	user, ok := r.auth.authenticate(req)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "Sign in to manage gateways"})
		return
	}
	r.manager.handleGateways(w, req, user)
}

// management authenticates an OIDC (or legacy admin token) management request
// and hands it to the selected gateway. Without an explicit selection the
// request is refused: legacy data is never exposed implicitly.
func (r *router) management(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOriginWrite(req) {
		writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
		return
	}
	user, ok := r.auth.authenticate(req)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "Sign in with your vrouter account"})
		return
	}
	selected := gatewayHeaderValue(req)
	if selected == "" {
		writeJSON(w, 400, map[string]string{"error": "Select a gateway with the X-Vrouter-Gateway header"})
		return
	}
	target, err := r.manager.engineFor(selected, user)
	if err != nil {
		writeJSON(w, gatewayErrorStatus(err), map[string]string{"error": gatewayErrorMessage(err)})
		return
	}
	ctx := withLoginUser(req.Context(), user)
	target.mgmt.ServeHTTP(w, req.WithContext(ctx))
}

// sameOriginWrite mirrors the legacy management origin guard: mutations from a
// cross-site context or a mismatched Origin/Host pair are refused.
func sameOriginWrite(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return parsed.Host == r.Host && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

// staticHandler serves the embedded frontend with the same cache and path
// rules as the legacy server.
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
		if _, err := fs.Stat(assets, path); err != nil {
			http.NotFound(w, r)
			return
		}
		if path == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}
}
