// Package gateway routes model API requests across provider accounts and serves
// the management API behind the web UI.
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config configures a gateway.
type Config struct {
	DataDir string
	// PublicURL is the browser-facing HTTP(S) origin, without a path. Sign-in
	// returns to it, so it is required.
	PublicURL string
	// OIDC is the identity provider people sign in with.
	OIDC OIDCConfig
	// StartWindows keeps a 5-hour window running on subscription accounts.
	StartWindows bool
	// WindowSkipPlans lists plans that never get a window trigger, as
	// comma-separated "plan" or "provider:plan". Empty skips the Codex Pro plans.
	WindowSkipPlans string
}

// Model is a model offered through the gateway.
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

// Account is a provider account as shown in the UI.
type Account struct {
	ID string `json:"id"`
	// Scope is global for a shared account and personal for the user's own.
	Scope string `json:"scope"`
	// Manageable is set when the user may reconnect, disable or remove it.
	Manageable      bool          `json:"manageable"`
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

// State is the dashboard snapshot of models and accounts.
type State struct {
	ObservedAt time.Time `json:"observedAt"`
	Models     []Model   `json:"models"`
	Accounts   []Account `json:"accounts"`
	Warnings   []string  `json:"warnings"`
}

type server struct {
	cfg          Config
	handler      http.Handler
	store        *accountStore
	registry     *registryStore
	oidc         *oidcClient
	client       *http.Client
	streamClient *http.Client
	quotaMu      sync.Mutex
	quotas       map[string]quotaCache
	quotaPending map[string]chan struct{}
	lastQuotas   map[string]quotaCache
	oauthMu      sync.Mutex
	oauth        map[string]oauthSession
	callbacks    map[string][]*http.Server
	modelMu      sync.Mutex
	catalogMu    sync.Mutex
	catalogs     map[string]catalogCache
	refreshMu    sync.Mutex
	windowMu     sync.Mutex
	windowWatch  map[string]windowWatch
	stopWindows  context.CancelFunc
	windowsDone  chan struct{}
	resetMu      sync.Mutex
	sequence     atomic.Uint64
}

// DefaultDataDir returns the data directory used when none is configured.
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

// New opens the data directory and returns the gateway handler.
func New(cfg Config, assets fs.FS) (http.Handler, error) {
	publicURL, err := parsePublicURL(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	cfg.PublicURL = publicURL
	base, _ := http.DefaultTransport.(*http.Transport)
	transport := base.Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	signIn, err := newOIDCClient(cfg.OIDC, cfg.PublicURL, &http.Client{Transport: transport, Timeout: 15 * time.Second})
	if err != nil {
		return nil, err
	}
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
	noRedirect := func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	s := &server{cfg: cfg, store: store, registry: &registryStore{data: store.data}, oidc: signIn, client: &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: noRedirect}, streamClient: &http.Client{Transport: transport, CheckRedirect: noRedirect}, quotas: map[string]quotaCache{}, lastQuotas: map[string]quotaCache{}, oauth: map[string]oauthSession{}, catalogs: map[string]catalogCache{}, windowWatch: map[string]windowWatch{}}
	s.handler = s.routes(assets)
	if cfg.StartWindows {
		var ctx context.Context
		ctx, s.stopWindows = context.WithCancel(context.Background())
		s.windowsDone = make(chan struct{})
		go s.watchWindows(ctx)
	}
	return s, nil
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
	return s.store.close()
}

func (s *server) routes(assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/auth", s.authStatus)
	mux.HandleFunc("GET /api/auth/login", s.startLogin)
	mux.HandleFunc("GET /api/auth/callback", s.finishLogin)
	mux.HandleFunc("DELETE /api/auth/session", s.endSession)
	management := []struct {
		pattern string
		handler http.HandlerFunc
		admin   bool
	}{
		{"GET /api/auth/session", s.sessionStatus, false},
		{"GET /api/state", s.state, false},
		{"GET /api/settings", s.getSettings, false},
		{"PUT /api/settings", s.putSettings, true},
		{"GET /api/model-settings", s.getModelSettings, false},
		{"PUT /api/model-settings", s.putModelSettings, true},
		{"PATCH /api/accounts", s.updateAccount, false},
		{"DELETE /api/accounts", s.removeAccount, false},
		{"POST /api/oauth/{provider}", s.startOAuth, false},
		{"GET /api/oauth/sessions/{id}", s.oauthStatus, false},
		{"POST /api/oauth/sessions/{id}/callback", s.oauthCallback, false},
		{"DELETE /api/oauth/sessions/{id}", s.cancelOAuth, false},
		{"GET /api/keys", s.listKeys, false},
		{"POST /api/keys", s.createKey, false},
		{"PATCH /api/keys/{id}", s.patchKey, false},
		{"DELETE /api/keys/{id}", s.deleteKey, false},
		{"GET /api/usage", s.usageHandler, false},
		{"GET /api/users", s.listUsers, true},
		{"PATCH /api/users/{id}", s.patchUser, true},
	}
	for _, route := range management {
		mux.HandleFunc(route.pattern, s.authorize(route.handler, route.admin))
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 404, map[string]string{"error": "Unknown management endpoint"})
	})
	mux.Handle("/v1/", http.HandlerFunc(s.serveInference))
	mux.HandleFunc("/v1beta/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 404, map[string]string{"error": "This protocol is not supported by vrouter"})
	})
	mux.HandleFunc("/", staticHandler(assets))
	return mux
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
	return err == nil && origin.Host == r.Host && (origin.Scheme == "http" || origin.Scheme == "https") && header == s.cfg.PublicURL
}

// authorize admits signed-in users, and only admins when admin is set. The
// handler finds the user with currentUser.
func (s *server) authorize(next http.HandlerFunc, admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.sameOrigin(r) {
			writeJSON(w, 403, map[string]string{"error": "Cross-origin management requests are not allowed"})
			return
		}
		user, ok := s.sessionUser(r)
		if !ok {
			writeJSON(w, 401, map[string]string{"error": "Sign in to continue"})
			return
		}
		if admin && !user.admin() {
			writeJSON(w, 403, map[string]string{"error": "Only admins can do this"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)))
	}
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	state := State{ObservedAt: time.Now().UTC(), Models: []Model{}, Accounts: []Account{}, Warnings: []string{}}
	var err error
	var wg sync.WaitGroup
	wg.Add(2)
	user := currentUser(r)
	go func() { defer wg.Done(); state.Models, err = s.models(ctx, user.ID) }()
	go func() { defer wg.Done(); state.Accounts = s.accounts(ctx, user) }()
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
	_ = json.NewEncoder(w).Encode(value) //nolint:errchkjson // the status is already sent, so a failed encode cannot be reported
}
