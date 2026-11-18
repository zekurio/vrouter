// Package gateway routes model API requests across provider accounts and serves
// the management API behind the web UI.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
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
	// AdminKey opens the dashboard and the management API. It is required.
	AdminKey string
	// PublicURL is the browser-facing HTTP(S) origin, without a path. Empty
	// trusts the origin each browser names, which suits a local instance.
	PublicURL string
	// StartWindows keeps a 5-hour window running on subscription accounts.
	StartWindows bool
	// WindowSkipPlans lists plans that never get a window trigger, as
	// comma-separated "plan" or "provider:plan". Empty skips the Codex Pro plans.
	WindowSkipPlans string
	// PriceCatalog is where list prices come from: empty for models.dev, off
	// for the built-in prices alone, or the URL of a catalog in its format.
	PriceCatalog string
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
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Provider       string        `json:"provider"`
	AuthMode       string        `json:"authMode,omitempty"`
	Status         string        `json:"status"`
	Plan           string        `json:"plan"`
	Remaining      *float64      `json:"remaining"`
	Window         string        `json:"window"`
	Email          string        `json:"email,omitempty"`
	StatusMessage  string        `json:"statusMessage,omitempty"`
	CreatedAt      *time.Time    `json:"createdAt,omitempty"`
	Reconnectable  bool          `json:"reconnectable"`
	Windows        []QuotaWindow `json:"windows,omitempty"`
	QuotaUpdatedAt *time.Time    `json:"quotaUpdatedAt,omitempty"`
	QuotaError     string        `json:"quotaError,omitempty"`
	QuotaRetryAt   *time.Time    `json:"quotaRetryAt,omitempty"` // next usage read after QuotaError
	// Resets are the usage resets the account holds, usable or not.
	Resets []AccountReset `json:"resets,omitempty"`
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
	client       *http.Client
	streamClient *http.Client
	// slowClient waits longer for response headers. A buffered response, an
	// image included, answers only once it is done.
	slowClient *http.Client
	// priceClient downloads the price catalog, which takes a while on a slow
	// link.
	priceClient  *http.Client
	quotaMu      sync.Mutex
	quotas       map[string]quotaCache
	quotaPending map[string]chan struct{}
	lastQuotas   map[string]quotaCache
	// planChecked holds when each Claude account's profile last named a plan.
	planChecked map[string]time.Time
	oauthMu     sync.Mutex
	oauth       map[string]oauthSession
	callbacks   map[string][]*http.Server
	modelMu     sync.Mutex
	catalogMu   sync.Mutex
	catalogs    map[string]catalogCache
	// catalogPending holds a channel per account whose catalog is loading. It
	// closes when the load ends.
	catalogPending map[string]chan struct{}
	refreshMu      sync.Mutex
	// refreshFailures holds each account's last failed token refresh. Guarded
	// by refreshMu.
	refreshFailures map[string]refreshFailure
	// trouble holds when accounts that just failed on their own may lead the
	// routing order again. rejected holds the expiry of each access token a
	// provider refused, so the next request refreshes it first.
	troubleMu   sync.Mutex
	trouble     map[string]time.Time
	rejected    map[string]time.Time
	windowMu    sync.Mutex
	windowWatch map[string]windowWatch
	stopWindows context.CancelFunc
	windowsDone chan struct{}
	resetMu     sync.Mutex
	sequence    atomic.Uint64
	// background runs work no request waits for, such as price and catalog
	// refreshes. Close cancels it and waits for the work to end.
	background     context.Context //nolint:containedctx // lifetime of the server's background work
	stopBackground context.CancelFunc
	backgroundMu   sync.Mutex
	backgroundWG   sync.WaitGroup
	closing        bool
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
	if err := validateAdminKey(cfg.AdminKey); err != nil {
		return nil, err
	}
	publicURL, err := parsePublicURL(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	cfg.PublicURL = publicURL
	priceSource, err := priceCatalogSource(cfg.PriceCatalog)
	if err != nil {
		return nil, err
	}
	base, _ := http.DefaultTransport.(*http.Transport)
	transport := base.Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
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
	slowTransport := transport.Clone()
	// Anthropic serves a buffered response for up to ten minutes.
	slowTransport.ResponseHeaderTimeout = 10 * time.Minute
	s := &server{
		cfg: cfg, store: store, registry: &registryStore{data: store.data},
		client:       &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: noRedirect},
		streamClient: &http.Client{Transport: transport, CheckRedirect: noRedirect},
		slowClient:   &http.Client{Transport: slowTransport, CheckRedirect: noRedirect},
		priceClient:  &http.Client{Transport: transport, Timeout: time.Minute},
		quotas:       map[string]quotaCache{}, lastQuotas: map[string]quotaCache{}, planChecked: map[string]time.Time{}, oauth: map[string]oauthSession{},
		catalogs: map[string]catalogCache{}, catalogPending: map[string]chan struct{}{}, refreshFailures: map[string]refreshFailure{},
		trouble: map[string]time.Time{}, rejected: map[string]time.Time{}, windowWatch: map[string]windowWatch{},
	}
	s.handler = s.routes(assets)
	s.background, s.stopBackground = context.WithCancel(context.Background())
	watch := s.loadStoredPrices(s.background, priceSource)
	s.runBackground(func(ctx context.Context) { s.watchPrices(ctx, watch) })
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
	if s.stopBackground != nil {
		s.backgroundMu.Lock()
		s.closing = true
		s.backgroundMu.Unlock()
		s.stopBackground()
		s.backgroundWG.Wait()
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
	if s.slowClient != nil {
		s.slowClient.CloseIdleConnections()
	}
	return s.store.close()
}

// runBackground starts work that outlives the request that asked for it. It
// reports false once the server is closing.
func (s *server) runBackground(work func(ctx context.Context)) bool {
	s.backgroundMu.Lock()
	defer s.backgroundMu.Unlock()
	if s.closing {
		return false
	}
	s.backgroundWG.Go(func() { work(s.background) })
	return true
}

func (s *server) routes(assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/auth", s.authStatus)
	mux.HandleFunc("POST /api/auth/session", s.startSession)
	mux.HandleFunc("DELETE /api/auth/session", s.endSession)
	management := []struct {
		pattern string
		handler http.HandlerFunc
	}{
		{"GET /api/state", s.state},
		{"GET /api/model-settings", s.getModelSettings},
		{"PUT /api/model-settings", s.putModelSettings},
		{"PATCH /api/accounts", s.updateAccount},
		{"DELETE /api/accounts", s.removeAccount},
		{"POST /api/accounts/{id}/resets", s.useReset},
		{"POST /api/oauth/{provider}", s.startOAuth},
		{"GET /api/oauth/sessions/{id}", s.oauthStatus},
		{"POST /api/oauth/sessions/{id}/callback", s.oauthCallback},
		{"DELETE /api/oauth/sessions/{id}", s.cancelOAuth},
		{"GET /api/keys", s.listKeys},
		{"POST /api/keys", s.createKey},
		{"PATCH /api/keys/{id}", s.patchKey},
		{"DELETE /api/keys/{id}", s.deleteKey},
		{"GET /api/usage", s.usageHandler},
		{"GET /api/quota-history", s.quotaHistoryHandler},
	}
	for _, route := range management {
		mux.HandleFunc(route.pattern, s.authorize(route.handler))
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
		case "prolite":
			return "Pro 100"
		case "pro":
			return "Pro 200"
		case "promax":
			return "Pro 500"
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
