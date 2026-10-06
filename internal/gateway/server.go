package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	DataDir, APIKey, AdminToken string
	Demo                        bool
	// Identity carries the OIDC sign-in configuration. When at least one
	// provider is configured the public New builds the multi-tenant facade
	// instead of the single legacy gateway.
	Identity IdentityConfig
}
type Model struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Provider           string   `json:"provider"`
	Context            int      `json:"context"`
	MaxOutput          int      `json:"maxOutput,omitempty"`
	Created            int64    `json:"created,omitempty"`
	Inputs             []string `json:"inputs,omitempty"`
	Reasoning          []string `json:"reasoning,omitempty"`
	ReasoningSupported bool     `json:"reasoningSupported,omitempty"`
}

type Account struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	Provider        string        `json:"provider"`
	AuthMode        string        `json:"authMode,omitempty"`
	Status          string        `json:"status"`
	Plan            string        `json:"plan"`
	Remaining       *float64      `json:"remaining"`
	Window          string        `json:"window"`
	Reset           string        `json:"reset"`
	Email           string        `json:"email,omitempty"`
	StatusMessage   string        `json:"statusMessage,omitempty"`
	CreatedAt       *time.Time    `json:"createdAt,omitempty"`
	Manageable      bool          `json:"manageable"`
	Reconnectable   bool          `json:"reconnectable"`
	Windows         []QuotaWindow `json:"windows,omitempty"`
	QuotaUpdatedAt  *time.Time    `json:"quotaUpdatedAt,omitempty"`
	QuotaError      string        `json:"quotaError,omitempty"`
	AvailableResets *int          `json:"availableResets,omitempty"`
}

type State struct {
	Mode       string    `json:"mode"`
	Connected  bool      `json:"connected"`
	ObservedAt time.Time `json:"observedAt"`
	Models     []Model   `json:"models"`
	Accounts   []Account `json:"accounts"`
	Warnings   []string  `json:"warnings"`
	Engine     Engine    `json:"engine"`
}

// Engine reports which parts of the server configuration are working, without
// returning any configured secret. Check values are "ok", "missing", or "error".
type Engine struct {
	Storage    string `json:"storage"`
	ClientKey  bool   `json:"clientKey"`
	Version    string `json:"version,omitempty"`
	Catalog    string `json:"catalog"`
	Management string `json:"management"`
	AdminToken bool   `json:"adminToken"`
}

type server struct {
	cfg          Config
	handler      http.Handler
	mux          *http.ServeMux
	mgmt         *http.ServeMux
	manager      *manager
	auth         authenticator
	gatewayID    string
	store        *accountStore
	client       *http.Client
	streamClient *http.Client
	quotaMu      sync.Mutex
	quotas       map[string]quotaCache
	quotaPending map[string]chan struct{}
	oauthMu      sync.Mutex
	oauth        map[string]oauthSession
	callbacks    map[string][]*http.Server
	modelMu      sync.Mutex
	catalogMu    sync.Mutex
	catalogs     map[string]catalogCache
	refreshMu    sync.Mutex
	resetMu      sync.Mutex
	sequence     atomic.Uint64
}

func DefaultDataDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "vrouter")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "vrouter")
}
func New(cfg Config, assets fs.FS) (http.Handler, error) {
	if cfg.APIKey != "" && cfg.AdminToken != "" && tokenEqual(cfg.APIKey, cfg.AdminToken) {
		return nil, errors.New("VROUTER_API_KEY and VROUTER_ADMIN_TOKEN must differ")
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
		if cfg.DataDir == "" && !cfg.Demo {
			return nil, errors.New("set VROUTER_DATA_DIR: home directory is unavailable")
		}
	}
	auth, err := newLoginAuth(cfg.Identity, cfg.AdminToken)
	if err != nil {
		return nil, err
	}
	if auth.mode() == "oidc" {
		return newRouter(cfg, assets, auth)
	}
	s, err := newGateway(cfg, assets)
	if err != nil {
		_ = auth.Close()
		return nil, err
	}
	s.auth = auth
	auth.register(s.mux)
	if !cfg.Demo {
		if _, err := openManager(s, assets); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	return s, nil
}

// newGateway builds one isolated gateway engine. The legacy deployment uses
// it directly as the public handler; tenant mode builds one per gateway with
// its own data directory. The returned muxes are intentionally split: mux is
// the standalone surface (management authorization included) while mgmt holds
// the same handlers unwrapped for a facade that has already authorized.
func newGateway(cfg Config, assets fs.FS) (*server, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	noRedirect := func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	s := &server{cfg: cfg, gatewayID: defaultGatewayID, client: &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: noRedirect}, streamClient: &http.Client{Transport: transport, CheckRedirect: noRedirect}, quotas: map[string]quotaCache{}, oauth: map[string]oauthSession{}, catalogs: map[string]catalogCache{}}
	if !cfg.Demo {
		var err error
		s.store, err = openStore(cfg.DataDir)
		if err != nil {
			return nil, err
		}
	}
	mux := http.NewServeMux()
	s.mux = mux
	s.mgmt = http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	management := []struct {
		pattern string
		handler http.HandlerFunc
	}{
		{"GET /api/state", s.state},
		{"GET /api/model-settings", s.getModelSettings},
		{"PUT /api/model-settings", s.putModelSettings},
		{"PATCH /api/accounts", s.updateAccount},
		{"DELETE /api/accounts", s.removeAccount},
		{"POST /api/oauth/{provider}", s.startOAuth},
		{"GET /api/oauth/sessions/{id}", s.oauthStatus},
		{"POST /api/oauth/sessions/{id}/callback", s.oauthCallback},
		{"DELETE /api/oauth/sessions/{id}", s.cancelOAuth},
		{"GET /api/keys", s.listKeys},
		{"POST /api/keys", s.createKey},
		{"PATCH /api/keys/{id}", s.patchKey},
		{"DELETE /api/keys/{id}", s.deleteKey},
		{"GET /api/telemetry", s.telemetryHandler},
		{"GET /api/gateways", s.gateways},
		{"POST /api/gateways", s.gateways},
	}
	for _, route := range management {
		mux.HandleFunc(route.pattern, s.authorize(s.legacyDispatch))
		s.mgmt.HandleFunc(route.pattern, route.handler)
	}
	unknown := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"error": "Unknown management endpoint"})
	}
	mux.HandleFunc("/api/", unknown)
	s.mgmt.HandleFunc("/api/", unknown)
	mux.Handle("/v1/", http.HandlerFunc(s.serveInference))
	mux.HandleFunc("/v1beta/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"error": "This protocol is not supported by vrouter"})
	})
	mux.HandleFunc("/", staticHandler(assets))
	s.handler = mux
	return s, nil
}
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	s.handler.ServeHTTP(w, r)
}
func (s *server) Close() error {
	s.oauthMu.Lock()
	for _, servers := range s.callbacks {
		for _, srv := range servers {
			_ = srv.Close()
		}
	}
	s.callbacks = nil
	s.oauth = map[string]oauthSession{}
	s.oauthMu.Unlock()
	if s.client != nil {
		s.client.CloseIdleConnections()
	}
	if s.streamClient != nil {
		s.streamClient.CloseIdleConnections()
	}
	var err error
	if s.store != nil {
		err = errors.Join(err, s.store.close())
	}
	// Child engines share the root's manager; only the root closes it.
	if s.manager != nil && s.manager.root == s {
		err = errors.Join(err, s.manager.close())
	}
	return errors.Join(err, closeIfPossible(s.auth))
}
func tokenEqual(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

func (s *server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin, err := url.Parse(r.Header.Get("Origin"))
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" || (r.Header.Get("Origin") != "" && (err != nil || origin.Host != r.Host || (origin.Scheme != "http" && origin.Scheme != "https"))) {
				writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
				return
			}
		}
		if s.cfg.AdminToken != "" {
			value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !tokenEqual(value, s.cfg.AdminToken) {
				writeJSON(w, 401, map[string]string{"error": "Sign in with your vrouter admin token"})
				return
			}
		} else {
			// Both the socket peer and Host must be local to prevent DNS rebinding.
			peer, _, _ := net.SplitHostPort(r.RemoteAddr)
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			hostIP, peerIP := net.ParseIP(host), net.ParseIP(peer)
			if peerIP == nil || !peerIP.IsLoopback() || (host != "localhost" && (hostIP == nil || !hostIP.IsLoopback())) {
				writeJSON(w, 403, map[string]string{"error": "Local management access only"})
				return
			}
		}
		next(w, r)
	}
}

func (s *server) hasClientKey() bool {
	if s.cfg.APIKey != "" {
		return true
	}
	return s.manager != nil && s.manager.hasKeys(s.gatewayID)
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Demo {
		state := demoState()
		state.Engine.AdminToken = s.cfg.AdminToken != ""
		writeJSON(w, 200, state)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	state := State{Mode: "live", Connected: true, ObservedAt: time.Now().UTC(), Models: []Model{}, Accounts: []Account{}, Warnings: []string{}, Engine: Engine{Version: "native", Storage: "local", Catalog: "ok", Management: "ok", AdminToken: s.cfg.AdminToken != "", ClientKey: s.hasClientKey()}}
	var err error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); state.Models, err = s.models(ctx) }()
	go func() { defer wg.Done(); state.Accounts = s.accounts(ctx) }()
	wg.Wait()
	if err != nil {
		state.Engine.Catalog = "error"
		state.Warnings = append(state.Warnings, "Some provider model catalogs could not be loaded. Reconnect expired accounts or refresh to retry.")
	}
	if !s.hasClientKey() {
		state.Warnings = append(state.Warnings, "Set VROUTER_API_KEY or create a gateway API key to enable inference requests.")
	}
	writeJSON(w, 200, state)
}

// Providers report plans as internal identifiers. Show the marketed name where
// it differs; unknown identifiers pass through with a capital letter.
func planName(provider, value string) string {
	if value == "" {
		return ""
	}
	if provider == "Codex" {
		switch strings.ToLower(value) {
		case "pro":
			return "Pro 20x"
		case "prolite":
			return "Pro 5x"
		}
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func provider(value string) string {
	switch strings.ToLower(value) {
	case "openai", "codex":
		return "Codex"
	case "anthropic", "claude":
		return "Claude"
	case "xai", "x-ai", "grok":
		return "Grok"
	case "google", "gemini":
		return "Gemini"
	case "":
		return "Other"
	default:
		return value
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
