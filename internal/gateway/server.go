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
	DataDir, AdminToken string
	// PublicURL is the browser-facing HTTP(S) origin, without a path.
	PublicURL string
	// ExternalAuth delegates dashboard authentication to a trusted reverse proxy.
	ExternalAuth bool
	// StartWindows keeps a 5-hour window running on subscription accounts.
	StartWindows bool
	// WindowSkipPlans lists plans that never get a window trigger, as
	// comma-separated "plan" or "provider:plan". Empty skips the Codex Pro plans.
	WindowSkipPlans string
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
	Email           string        `json:"email,omitempty"`
	StatusMessage   string        `json:"statusMessage,omitempty"`
	CreatedAt       *time.Time    `json:"createdAt,omitempty"`
	Reconnectable   bool          `json:"reconnectable"`
	Windows         []QuotaWindow `json:"windows,omitempty"`
	QuotaUpdatedAt  *time.Time    `json:"quotaUpdatedAt,omitempty"`
	QuotaError      string        `json:"quotaError,omitempty"`
	AvailableResets *int          `json:"availableResets,omitempty"`
}

type State struct {
	ObservedAt time.Time `json:"observedAt"`
	Models     []Model   `json:"models"`
	Accounts   []Account `json:"accounts"`
	Warnings   []string  `json:"warnings"`
}

type server struct {
	cfg             Config
	handler         http.Handler
	mux             *http.ServeMux
	mgmt            *http.ServeMux
	manager         *manager
	gatewayID       string
	store           *accountStore
	client          *http.Client
	streamClient    *http.Client
	quotaMu         sync.Mutex
	quotas          map[string]quotaCache
	quotaPending    map[string]chan struct{}
	lastQuotas      map[string]quotaCache
	oauthMu         sync.Mutex
	oauth           map[string]oauthSession
	callbacks       map[string][]*http.Server
	modelMu         sync.Mutex
	catalogMu       sync.Mutex
	catalogs        map[string]catalogCache
	refreshMu       sync.Mutex
	windowMu        sync.Mutex
	windowWatch     map[string]windowWatch
	stopWindows     context.CancelFunc
	windowsDone     chan struct{}
	claudeUsageGate sync.RWMutex
	codexUsageGate  sync.RWMutex
	resetMu         sync.Mutex
	sequence        atomic.Uint64
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
	publicURL, err := parsePublicURL(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	cfg.PublicURL = publicURL
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
		if cfg.DataDir == "" {
			return nil, errors.New("set VROUTER_DATA_DIR: home directory is unavailable")
		}
	}
	store, err := openStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := newGatewayStore(cfg, assets, store)
	s.mux.HandleFunc("GET /api/auth", s.authStatus)
	s.mux.HandleFunc("POST /api/auth/session", s.startSession)
	s.mux.HandleFunc("DELETE /api/auth/session", s.endSession)
	attachManager(s, assets)
	if cfg.StartWindows {
		var ctx context.Context
		ctx, s.stopWindows = context.WithCancel(context.Background())
		s.windowsDone = make(chan struct{})
		go s.watchWindows(ctx)
	}
	return s, nil
}

// newGatewayStore builds one gateway's handlers over its view of the store.
// The root authorizes management once, then dispatches to a gateway's mgmt mux.
func newGatewayStore(cfg Config, assets fs.FS, store *accountStore) *server {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	noRedirect := func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	s := &server{cfg: cfg, gatewayID: defaultGatewayID, client: &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: noRedirect}, streamClient: &http.Client{Transport: transport, CheckRedirect: noRedirect}, quotas: map[string]quotaCache{}, lastQuotas: map[string]quotaCache{}, oauth: map[string]oauthSession{}, catalogs: map[string]catalogCache{}, windowWatch: map[string]windowWatch{}}
	s.store = store
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
	}
	for _, route := range management {
		mux.HandleFunc(route.pattern, s.authorize(s.dispatchManagement))
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
	return s
}
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	s.handler.ServeHTTP(w, r)
}
func (s *server) Close() error {
	if s.stopWindows != nil {
		s.stopWindows()
		<-s.windowsDone
	}
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
	return err
}
func tokenEqual(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
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

func (s *server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.sameOrigin(r) {
			writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
			return
		}
		if s.cfg.AdminToken != "" {
			value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			bearer := strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && tokenEqual(value, s.cfg.AdminToken)
			if !bearer && !s.hasSession(r) {
				writeJSON(w, 401, map[string]string{"error": "Sign in with your vrouter admin token"})
				return
			}
		} else if !s.cfg.ExternalAuth {
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

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	state := State{ObservedAt: time.Now().UTC(), Models: []Model{}, Accounts: []Account{}, Warnings: []string{}}
	var err error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); state.Models, err = s.models(ctx) }()
	go func() { defer wg.Done(); state.Accounts = s.accounts(ctx) }()
	wg.Wait()
	if err != nil {
		state.Warnings = append(state.Warnings, "Some provider model catalogs could not be loaded. Reconnect expired accounts or refresh to retry.")
	}
	writeJSON(w, 200, state)
}

// Providers report plans as internal identifiers. Show the marketed name where
// it differs; unknown identifiers pass through with a capital letter.
func planName(provider, value string) string {
	if value == "" {
		return ""
	}
	if provider == "codex" {
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
		return "codex"
	case "anthropic", "claude":
		return "claude"
	case "xai", "x-ai", "grok":
		return "grok"
	case "google", "gemini":
		return "gemini"
	case "":
		return "other"
	default:
		return value
	}
}

// providerLabel names a provider where no account label or email is known.
func providerLabel(value string) string {
	switch provider(value) {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
