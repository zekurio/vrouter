package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// All gateways share one data store. Runtime engines hold scoped views and caches.
const (
	// gatewayHeader selects a gateway for management requests. Inference
	// requests never use it: /v1 selects its gateway from the presented key.
	gatewayHeader = "X-Vrouter-Gateway"

	// defaultGatewayID names the initial gateway.
	defaultGatewayID = "default"
	localAdminID     = "local-admin"

	registryVersion    = 1
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

// gatewayRecord names one gateway in the shared store.
type gatewayRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OwnerID   string    `json:"ownerId"`
	CreatedAt time.Time `json:"createdAt"`
}

// diskRegistry holds keys and request accounting in the shared data file.
type diskRegistry struct {
	Version   int                          `json:"version"`
	Gateways  []gatewayRecord              `json:"gateways"`
	Keys      []keyRecord                  `json:"keys"`
	Telemetry map[string][]telemetryRecord `json:"telemetry,omitempty"`
}

// registryStore is a metadata view of the same store as provider accounts.
// It has no file, lock, or independent persistence path.
type registryStore struct{ data *dataStore }

func (r *registryStore) snapshot() diskRegistry {
	r.data.mu.Lock()
	defer r.data.mu.Unlock()
	return registryClone(r.data.state.Registry)
}

func (r *registryStore) update(mutate func(*diskRegistry) error) error {
	if mutate == nil {
		return errors.New("gateway: key update requires a callback")
	}
	return r.data.update(func(d *diskData) error { return mutate(&d.Registry) })
}

// registryNormalize makes collections concrete, seeds the default gateway, and
// trims telemetry to the retention bound. Telemetry is stored oldest first;
// the tail is the retained recent portion.
func registryNormalize(state *diskRegistry) {
	if state.Gateways == nil {
		state.Gateways = []gatewayRecord{}
	}
	if state.Keys == nil {
		state.Keys = []keyRecord{}
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
		state.Keys[i].legacyKeyFields = legacyKeyFields{}
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

// manager dispatches requests to gateway engines that share the data store.
type manager struct {
	mu       sync.Mutex
	cfg      Config
	assets   fs.FS
	root     *server
	registry *registryStore
	engines  map[string]*server
	closed   bool
}

// attachManager gives the root engine the shared registry view.
func attachManager(root *server, assets fs.FS) {
	registry := &registryStore{data: root.store.data}
	root.manager = &manager{cfg: root.cfg, assets: assets, root: root, registry: registry, engines: map[string]*server{}}
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
	return err
}

// engine creates each gateway's runtime caches on first use.
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
	if !m.hasGateway(id) {
		return nil, errGatewayNotFound
	}
	// Only the root mux is served, so the root has already authorized
	// management before a request reaches this engine.
	engine := newGatewayStore(m.cfg, m.assets, &accountStore{data: m.root.store.data, gatewayID: id})
	engine.gatewayID = id
	engine.manager = m
	m.engines[id] = engine
	return engine, nil
}

func (m *manager) hasGateway(id string) bool {
	for _, gateway := range m.registry.snapshot().Gateways {
		if gateway.ID == id {
			return true
		}
	}
	return false
}

func (m *manager) hasAnyKeys() bool {
	for _, key := range m.registry.snapshot().Keys {
		if key.RevokedAt == nil && !key.expired(time.Now()) {
			return true
		}
	}
	return false
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

// gateways lists every gateway, oldest first. It answers the same for any
// selected gateway, so a stale selection can still load the list.
func (s *server) gateways(w http.ResponseWriter, r *http.Request) {
	gateways := s.manager.registry.snapshot().Gateways
	sort.SliceStable(gateways, func(i, j int) bool {
		if gateways[i].CreatedAt.Equal(gateways[j].CreatedAt) {
			return gateways[i].ID < gateways[j].ID
		}
		return gateways[i].CreatedAt.Before(gateways[j].CreatedAt)
	})
	writeJSON(w, 200, map[string]any{"gateways": gateways})
}

// gatewayHeaderValue returns the explicitly selected gateway, or "" when the
// client made no selection.
func gatewayHeaderValue(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(gatewayHeader))
}

// dispatchManagement selects the requested gateway, or the default when absent.
func (s *server) dispatchManagement(w http.ResponseWriter, r *http.Request) {
	selected := gatewayHeaderValue(r)
	// The gateway list is not scoped to a selection.
	if selected == "" || selected == s.gatewayID || r.URL.Path == "/api/gateways" {
		s.mgmt.ServeHTTP(w, r)
		return
	}
	target, err := s.manager.engine(selected)
	if err != nil {
		writeJSON(w, gatewayErrorStatus(err), map[string]string{"error": gatewayErrorMessage(err)})
		return
	}
	target.mgmt.ServeHTTP(w, r)
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
