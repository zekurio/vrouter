package gateway

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Tenancy model
//
// The legacy single-gateway deployment keeps working exactly as before: the
// public New returns a *server rooted at DATA_DIR whose accounts, model policy
// and OAuth sessions live in DATA_DIR/state.json. Every additional gateway is
// an independent child server with its own store below DATA_DIR/gateways/<id>.
//
// The registry below is the durable, process-wide index that ties gateways,
// their hashed API keys and bounded request telemetry together. It lives
// beside the legacy state file, never inside it, so existing installations
// keep their state untouched.
const (
	// gatewayHeader selects a gateway for management requests. Inference
	// requests never use it: /v1 selects its gateway from the presented key.
	gatewayHeader = "X-Vrouter-Gateway"

	// defaultGatewayID names the legacy gateway backed by DATA_DIR/state.json.
	// The former owner field is retained for registry compatibility.
	defaultGatewayID = "default"
	localAdminID     = "local-admin"

	registryVersion  = 1
	registryFileName = "registry.json"
	registryLockName = "registry.lock"
	// registryMaxBytes bounds how much of the registry is decoded. Telemetry
	// is capped per gateway, so anything larger indicates corruption.
	registryMaxBytes   = 64 << 20
	telemetryRetention = 1000
	gatewayNameMax     = 64
	keyNameMax         = 64
)

var (
	errGatewayNotFound  = errors.New("gateway: gateway not found")
	errKeyNotFound      = errors.New("gateway: API key not found")
	errRegistryNoChange = errors.New("gateway: registry change rejected")

	recordIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	keyHashPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// gatewayRecord is one tenant gateway. The legacy gateway is created
// implicitly so older data directories show up without a migration step.
type gatewayRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OwnerID   string    `json:"ownerId"`
	CreatedAt time.Time `json:"createdAt"`
}

// diskRegistry is the complete persisted tenancy index. It is replaced
// atomically as a whole, exactly like the account store.
type diskRegistry struct {
	Version   int                          `json:"version"`
	Gateways  []gatewayRecord              `json:"gateways"`
	Keys      []keyRecord                  `json:"keys"`
	Telemetry map[string][]telemetryRecord `json:"telemetry,omitempty"`
}

// registryStore owns a single registry.json beneath dir. Access is serialized
// in memory and ownership is enforced across processes with a non-blocking
// file lock, so two routers can never write the same registry.
type registryStore struct {
	mu     sync.Mutex
	path   string
	lock   *os.File
	state  diskRegistry
	closed bool
}

func openRegistry(dir string) (*registryStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("gateway: registry directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("gateway: create registry directory: %w", err)
	}
	if err := storeRefuseSymlink(dir); err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("gateway: open registry directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("gateway: registry path is not a directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("gateway: restrict registry directory permissions: %w", err)
	}
	r := &registryStore{path: filepath.Join(dir, registryFileName)}
	lock, err := storeAcquireLock(filepath.Join(dir, registryLockName))
	if err != nil {
		return nil, err
	}
	r.lock = lock
	if err := r.load(); err != nil {
		// Release the lock on every initialization failure so a broken
		// registry can never leave a permanently orphaned lock behind.
		_ = r.releaseLock()
		return nil, err
	}
	return r, nil
}

func (r *registryStore) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.releaseLock()
}

func (r *registryStore) releaseLock() error {
	if r.lock == nil {
		return nil
	}
	f := r.lock
	r.lock = nil
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	switch {
	case unlockErr != nil:
		return fmt.Errorf("gateway: unlock registry: %w", unlockErr)
	case closeErr != nil:
		return fmt.Errorf("gateway: close registry lock: %w", closeErr)
	}
	return nil
}

// snapshot returns a deep copy of the registry. Callers may read and mutate
// the result freely.
func (r *registryStore) snapshot() diskRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return registryClone(r.state)
}

// update applies mutate to a private copy of the registry and persists it.
// The in-memory state is swapped only after the file has been replaced
// atomically, so a failed callback or write leaves both memory and disk on the
// previous revision.
func (r *registryStore) update(mutate func(*diskRegistry) error) error {
	if mutate == nil {
		return errors.New("gateway: registry update requires a callback")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errStoreClosed
	}
	draft := registryClone(r.state)
	if err := mutate(&draft); err != nil {
		return err
	}
	registryNormalize(&draft)
	if err := registryValidate(draft); err != nil {
		return err
	}
	if err := writeRegistryFile(r.path, draft); err != nil {
		return err
	}
	r.state = draft
	return nil
}

// markUncertain records, in memory only, that a key's measured usage can no
// longer be trusted. It releases this attempt's reservation, because the
// failed settlement rolled back the persisted decrement.
func (r *registryStore) markUncertain(keyID string) {
	if keyID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.state.Keys {
		if r.state.Keys[i].ID == keyID {
			r.state.Keys[i].PercentUncertain = map[string]bool{"claude": true, "codex": true}
			if r.state.Keys[i].InFlight > 0 {
				r.state.Keys[i].InFlight--
			}
			return
		}
	}
}

// load reads registry.json, or creates it with the legacy gateway seeded when
// absent.
func (r *registryStore) load() error {
	if err := storeRefuseSymlink(r.path); err != nil {
		return err
	}
	f, err := os.Open(r.path)
	var data []byte
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(f, registryMaxBytes+1))
		_ = f.Close()
	}
	switch {
	case err == nil:
		state, err := registryDecode(data)
		if err != nil {
			return err
		}
		if err := os.Chmod(r.path, 0o600); err != nil {
			return fmt.Errorf("gateway: restrict registry permissions: %w", err)
		}
		r.state = state
		return nil
	case errors.Is(err, fs.ErrNotExist):
		state := diskRegistry{Version: registryVersion}
		registryNormalize(&state)
		if err := writeRegistryFile(r.path, state); err != nil {
			return err
		}
		r.state = state
		return nil
	default:
		return fmt.Errorf("gateway: read registry: %w", err)
	}
}

func registryDecode(data []byte) (diskRegistry, error) {
	if len(data) > registryMaxBytes {
		return diskRegistry{}, errors.New("gateway: registry file is too large")
	}
	var state diskRegistry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return diskRegistry{}, errors.New("gateway: registry is malformed")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return diskRegistry{}, errors.New("gateway: registry is malformed: unexpected trailing data")
	}
	if state.Version != registryVersion {
		return diskRegistry{}, fmt.Errorf("gateway: registry version %d is not supported", state.Version)
	}
	registryNormalize(&state)
	if err := registryValidate(state); err != nil {
		return diskRegistry{}, err
	}
	// A reservation that survived a restart cannot still be running. Its
	// usage is unknown, so the key becomes uncertain until its owner acts.
	for i := range state.Keys {
		if state.Keys[i].InFlight > 0 {
			state.Keys[i].PercentUncertain = map[string]bool{"claude": true, "codex": true}
			state.Keys[i].InFlight = 0
		}
	}
	return state, nil
}

// writeRegistryFile replaces path atomically with a private 0600 file. The
// encoded document must stay inside the same bound the loader enforces, so a
// successful write can never make the next startup refuse its own file.
func writeRegistryFile(path string, state diskRegistry) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("gateway: encode registry: %w", err)
	}
	data = append(data, '\n')
	if len(data) > registryMaxBytes {
		return errors.New("gateway: registry exceeds the size limit")
	}
	return atomicWritePrivate(path, "registry", data)
}

// registryNormalize makes collections concrete, seeds the legacy gateway, and
// trims telemetry to the retention bound. Telemetry is stored oldest first;
// the tail is the retained recent portion.
func registryNormalize(state *diskRegistry) {
	if state.Gateways == nil {
		state.Gateways = []gatewayRecord{}
	}
	if state.Keys == nil {
		state.Keys = []keyRecord{}
	}
	for i := range state.Keys {
		state.Keys[i].LegacyLimitRequests, state.Keys[i].LegacyLimitTokens, state.Keys[i].LegacyUsageUncertain = nil, nil, nil
	}
	if state.Telemetry == nil {
		state.Telemetry = map[string][]telemetryRecord{}
	}
	seeded := false
	for _, gateway := range state.Gateways {
		if gateway.ID == defaultGatewayID {
			seeded = true
			break
		}
	}
	if !seeded {
		now := time.Now().UTC()
		state.Gateways = append([]gatewayRecord{{ID: defaultGatewayID, Name: "Default gateway", OwnerID: localAdminID, CreatedAt: now}}, state.Gateways...)
	}
	for id, records := range state.Telemetry {
		if len(records) > telemetryRetention {
			state.Telemetry[id] = append([]telemetryRecord(nil), records[len(records)-telemetryRetention:]...)
		}
	}
	for i := range state.Keys {
		state.Keys[i].Name = strings.TrimSpace(state.Keys[i].Name)
	}
}

// registryValidate enforces the invariants of the registry vocabulary. Errors
// name the field and position, never stored values.
func registryValidate(state diskRegistry) error {
	if state.Version != registryVersion {
		return fmt.Errorf("gateway: registry version %d is not supported", state.Version)
	}
	seenGateways := make(map[string]struct{}, len(state.Gateways))
	for i := range state.Gateways {
		gateway := &state.Gateways[i]
		if !recordIDPattern.MatchString(gateway.ID) {
			return fmt.Errorf("gateway: gateway %d has an invalid ID", i)
		}
		if _, duplicate := seenGateways[gateway.ID]; duplicate {
			return errors.New("gateway: gateway IDs must be unique")
		}
		seenGateways[gateway.ID] = struct{}{}
		if strings.TrimSpace(gateway.Name) == "" || len(gateway.Name) > 128 {
			return fmt.Errorf("gateway: gateway %d has an invalid name", i)
		}
		if strings.TrimSpace(gateway.OwnerID) == "" || len(gateway.OwnerID) > 256 {
			return fmt.Errorf("gateway: gateway %d has an invalid owner", i)
		}
	}
	seenKeys := make(map[string]struct{}, len(state.Keys))
	for i := range state.Keys {
		key := &state.Keys[i]
		if !recordIDPattern.MatchString(key.ID) {
			return fmt.Errorf("gateway: key %d has an invalid ID", i)
		}
		if _, duplicate := seenKeys[key.ID]; duplicate {
			return errors.New("gateway: key IDs must be unique")
		}
		seenKeys[key.ID] = struct{}{}
		if _, exists := seenGateways[key.GatewayID]; !exists {
			return fmt.Errorf("gateway: key %d references an unknown gateway", i)
		}
		if !keyHashPattern.MatchString(key.Hash) {
			return fmt.Errorf("gateway: key %d has an invalid hash", i)
		}
		if !strings.HasPrefix(key.Prefix, "vr_") || len(key.Prefix) > 32 {
			return fmt.Errorf("gateway: key %d has an invalid prefix", i)
		}
		if strings.TrimSpace(key.Name) == "" || len(key.Name) > 128 {
			return fmt.Errorf("gateway: key %d has an invalid name", i)
		}
		if key.ExpiresAt != nil && key.ExpiresAt.IsZero() {
			return fmt.Errorf("gateway: key %d has an invalid expiry", i)
		}
		if key.UsedRequests < 0 || key.UsedRequests > maxTokenCount || key.UsedTokens < 0 || key.UsedTokens > maxTokenCount {
			return fmt.Errorf("gateway: key %d has invalid counters", i)
		}
		if err := validatePercentQuotas(key.ProviderQuotas); err != nil {
			return err
		}
		for _, charge := range key.PercentCharges {
			if (charge.Provider != "claude" && charge.Provider != "codex") || (charge.Window != "weekly" && charge.Window != "five-hour") || charge.ResetAt.IsZero() || math.IsNaN(charge.Percent) || math.IsInf(charge.Percent, 0) || charge.Percent < 0 {
				return errors.New("invalid provider percentage accounting")
			}
		}
		if key.InFlight < 0 {
			return fmt.Errorf("gateway: key %d has a negative in-flight count", i)
		}
	}
	for gatewayID, records := range state.Telemetry {
		if _, exists := seenGateways[gatewayID]; !exists {
			return fmt.Errorf("gateway: telemetry references an unknown gateway")
		}
		if len(records) > telemetryRetention {
			return fmt.Errorf("gateway: telemetry exceeds the retention limit")
		}
		for i := range records {
			if err := validateTelemetryRecord(records[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// registryClone deep-copies a registry so no caller can alias memory owned by
// the store.
func registryClone(state diskRegistry) diskRegistry {
	out := state
	out.Gateways = append([]gatewayRecord(nil), state.Gateways...)
	out.Keys = make([]keyRecord, len(state.Keys))
	for i, key := range state.Keys {
		out.Keys[i] = key
		clonePercentKey(&out.Keys[i], key)
		if key.RevokedAt != nil {
			revoked := *key.RevokedAt
			out.Keys[i].RevokedAt = &revoked
		}
		if key.ExpiresAt != nil {
			expires := *key.ExpiresAt
			out.Keys[i].ExpiresAt = &expires
		}
	}
	out.Telemetry = make(map[string][]telemetryRecord, len(state.Telemetry))
	for id, records := range state.Telemetry {
		out.Telemetry[id] = append([]telemetryRecord(nil), records...)
	}
	return out
}

func findKey(state *diskRegistry, id string) *keyRecord {
	for i := range state.Keys {
		if state.Keys[i].ID == id {
			return &state.Keys[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Manager

// manager coordinates the existing gateway stores: the durable
// registry, the legacy root engine and lazily opened child engines.
type manager struct {
	mu       sync.Mutex
	cfg      Config
	assets   fs.FS
	dir      string
	root     *server
	registry *registryStore
	engines  map[string]*server
	degraded atomic.Bool
	closed   bool
}

// openManager opens the registry and attaches it to the root engine. The
// caller owns root and must not have attached a manager yet.
func openManager(root *server, assets fs.FS) (*manager, error) {
	registry, err := openRegistry(root.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	m := &manager{cfg: root.cfg, assets: assets, dir: root.cfg.DataDir, root: root, registry: registry, engines: map[string]*server{}}
	root.manager = m
	return m, nil
}

func (m *manager) close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	engines := make([]*server, 0, len(m.engines))
	for _, engine := range m.engines {
		engines = append(engines, engine)
	}
	m.engines = nil
	m.mu.Unlock()
	var err error
	for _, engine := range engines {
		err = errors.Join(err, engine.Close())
	}
	return errors.Join(err, m.registry.close())
}

// engine returns the running engine for a gateway, opening its child store on
// first use. The legacy gateway reuses the root engine.
func (m *manager) engine(id string) (*server, error) {
	if !recordIDPattern.MatchString(id) {
		return nil, errGatewayNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errStoreClosed
	}
	if id == defaultGatewayID {
		return m.root, nil
	}
	if engine, ok := m.engines[id]; ok {
		return engine, nil
	}
	record := m.gatewayRecord(id)
	if record == nil {
		return nil, errGatewayNotFound
	}
	cfg := m.cfg
	cfg.DataDir = filepath.Join(m.dir, "gateways", id)
	// Child engines never accept the process-wide legacy key and never
	// re-authorize management requests; the facade authorizes first.
	cfg.APIKey = ""
	cfg.AdminToken = ""
	engine, err := newGateway(cfg, m.assets)
	if err != nil {
		return nil, err
	}
	engine.gatewayID = id
	engine.manager = m
	m.engines[id] = engine
	return engine, nil
}

func (m *manager) gatewayRecord(id string) *gatewayRecord {
	registry := m.registry.snapshot()
	for i := range registry.Gateways {
		if registry.Gateways[i].ID == id {
			return &registry.Gateways[i]
		}
	}
	return nil
}

func (m *manager) hasKeys(gatewayID string) bool {
	for _, key := range m.registry.snapshot().Keys {
		if key.GatewayID == gatewayID {
			return true
		}
	}
	return false
}

func (m *manager) hasAnyKeys() bool {
	return len(m.registry.snapshot().Keys) > 0
}

// keyByHash finds a live API key by its secret. Revoked keys are reported as
// missing so their state is never confirmed over HTTP. Expired keys are
// returned; the caller tells their holder why they stopped working.
func (m *manager) keyByHash(secret string) (keyRecord, bool) {
	if secret == "" {
		return keyRecord{}, false
	}
	sum := sha256.Sum256([]byte(secret))
	digest := hex.EncodeToString(sum[:])
	for _, key := range m.registry.snapshot().Keys {
		if key.RevokedAt != nil {
			continue
		}
		if len(key.Hash) == len(digest) && subtle.ConstantTimeCompare([]byte(key.Hash), []byte(digest)) == 1 {
			return key, true
		}
	}
	return keyRecord{}, false
}

// gatewayViews keeps older account stores accessible to the administrator.
func (m *manager) gatewayViews() []gatewayView {
	registry := m.registry.snapshot()
	views := make([]gatewayView, 0, len(registry.Gateways))
	for _, gateway := range registry.Gateways {
		views = append(views, gatewayView{ID: gateway.ID, Name: gateway.Name, OwnerID: gateway.OwnerID, CreatedAt: gateway.CreatedAt})
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].ID < views[j].ID
		}
		return views[i].CreatedAt.Before(views[j].CreatedAt)
	})
	return views
}

type gatewayView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OwnerID   string    `json:"ownerId"`
	CreatedAt time.Time `json:"createdAt"`
}

type gatewaysResponse struct {
	Gateways []gatewayView `json:"gateways"`
}

// gateways lists preserved account stores.
func (s *server) gateways(w http.ResponseWriter, r *http.Request) {
	s.manager.handleGateways(w, r)
}

func (m *manager) handleGateways(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, gatewaysResponse{Gateways: m.gatewayViews()})

	default:
		w.Header().Set("Allow", "GET")
		writeJSON(w, 405, map[string]string{"error": "Use GET for gateways"})
	}
}

// gatewayHeaderValue returns the explicitly selected gateway, or "" when the
// client made no selection.
func gatewayHeaderValue(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(gatewayHeader))
}

// gatewayIndependentPath reports endpoints that are deliberately not scoped to
// a selected gateway: listing gateways must work regardless of
// the header, including while a selection is stale.
func gatewayIndependentPath(path string) bool {
	return path == "/api/gateways"
}

// legacyDispatch serves one management request on the gateway selected by the
// X-Vrouter-Gateway header. The legacy deployment accepts an absent header and
// falls back to the default gateway.
func (s *server) legacyDispatch(w http.ResponseWriter, r *http.Request) {
	if s.manager == nil || gatewayIndependentPath(r.URL.Path) {
		s.mgmt.ServeHTTP(w, r)
		return
	}
	selected := gatewayHeaderValue(r)
	if selected != "" && selected != s.gatewayID {
		target, err := s.manager.engine(selected)
		if err != nil {
			writeJSON(w, gatewayErrorStatus(err), map[string]string{"error": gatewayErrorMessage(err)})
			return
		}
		target.mgmt.ServeHTTP(w, r)
		return
	}
	s.mgmt.ServeHTTP(w, r)
}

func gatewayErrorStatus(err error) int {
	if errors.Is(err, errGatewayNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func gatewayErrorMessage(err error) string {
	if errors.Is(err, errGatewayNotFound) {
		return "Gateway not found"
	}
	return "Gateway is unavailable"
}
