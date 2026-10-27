package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const (
	dataFileName = "vrouter.json"
	dataMaxBytes = 128 << 20
	dataVersion  = 2
	dataV1Backup = "vrouter.v1.json"
	// defaultGatewayV1 is the only gateway version 1 data could use.
	defaultGatewayV1 = "default"
)

// Configuration changes commit together in JSON. Request accounting lives
// in SQLite so inference never rewrites credentials or model policy.
type diskData struct {
	diskState
	diskRegistry

	Version int `json:"version"`
}

type dataStore struct {
	mu     sync.Mutex
	path   string
	state  diskData
	lock   *os.File
	closed bool
	usage  *usageStore
}

func openDataStore(dir string) (*dataStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("gateway: data directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := storeRefuseSymlink(dir); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directories need the execute bit; 0700 is owner-only
		return nil, err
	}
	s := &dataStore{path: filepath.Join(dir, dataFileName)}
	lock, err := storeAcquireLock(filepath.Join(dir, "vrouter.lock"))
	if err != nil {
		return nil, err
	}
	s.lock = lock
	if err := s.load(); err != nil {
		_ = s.close()
		return nil, err
	}
	s.usage, err = openUsageStore(dir)
	if err != nil {
		_ = s.close()
		return nil, err
	}
	return s, nil
}

func (s *dataStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var usageErr error
	if s.usage != nil {
		usageErr = s.usage.db.Close()
	}
	err := errors.Join(usageErr, syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN), s.lock.Close())
	s.lock = nil
	return err
}

func dataClone(d diskData) diskData {
	return diskData{Version: d.Version, diskState: storeClone(d.diskState), diskRegistry: registryClone(d.diskRegistry)}
}

func normalizeData(d *diskData) error {
	if d.Version != dataVersion {
		return errors.New("gateway: data version is not supported")
	}
	storeNormalize(&d.diskState)
	if err := storeValidate(d.diskState); err != nil {
		return err
	}
	registryNormalize(&d.diskRegistry)
	if err := registryValidate(d.diskRegistry); err != nil {
		return err
	}
	return validateOwners(d.Accounts, d.Users)
}

func (s *dataStore) update(mutate func(*diskData) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	draft := dataClone(s.state)
	if err := mutate(&draft); err != nil {
		return err
	}
	if err := normalizeData(&draft); err != nil {
		return err
	}
	if err := s.write(draft); err != nil {
		return err
	}
	s.state = draft
	return nil
}

func (s *dataStore) write(d diskData) error {
	raw, err := json.Marshal(d) //nolint:errchkjson // a failed encoding must never replace the saved credentials
	if err != nil {
		return errors.New("gateway: could not encode data")
	}
	if len(raw)+1 > dataMaxBytes {
		return errors.New("gateway: data exceeds the size limit")
	}
	return atomicWritePrivate(s.path, "data", append(raw, '\n'))
}

func readPrivateState(path string, limit int64) ([]byte, error) {
	if err := storeRefuseSymlink(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("gateway: invalid state file")
	}
	f, err := os.Open(path) //nolint:gosec // path is the state file inside the configured data directory
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("gateway: invalid state file")
	}
	if err := f.Chmod(0o600); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("gateway: could not read bounded state")
	}
	return raw, nil
}

func (s *dataStore) load() error {
	raw, err := readPrivateState(s.path, dataMaxBytes)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.state = diskData{Version: dataVersion}
	case err != nil:
		return err
	case dataFileVersion(raw) == 1:
		return s.upgradeV1(raw)
	case decodeStrict(raw, &s.state) != nil:
		return errors.New("gateway: data is malformed")
	}
	if err := normalizeData(&s.state); err != nil {
		return err
	}
	return s.write(s.state)
}

// dataFileVersion reads only the version, so an older layout can be told
// apart before it is decoded. Unreadable data reports zero.
func dataFileVersion(raw []byte) int {
	var header struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(raw, &header)
	return header.Version
}

func decodeStrict(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// upgradeV1 rewrites version 1 data in the current layout. The old file is
// copied aside first, because a version 1 build cannot read the new one.
func (s *dataStore) upgradeV1(raw []byte) error {
	state, err := upgradeDataV1(raw)
	if err != nil {
		return err
	}
	if err := normalizeData(&state); err != nil {
		return err
	}
	if err := atomicWritePrivate(filepath.Join(filepath.Dir(s.path), dataV1Backup), "data backup", raw); err != nil {
		return err
	}
	if err := s.write(state); err != nil {
		return err
	}
	s.state = state
	slog.Info("data upgraded to version 2", "backup", dataV1Backup)
	return nil
}

// dataV1 is the layout before gateways were removed. Every gateway had its
// own accounts and model policy, and every key named its gateway. Only the
// default gateway could be used.
type dataV1 struct {
	Version  int                    `json:"version"`
	States   map[string]dataV1State `json:"accounts"`
	Registry struct {
		Version  int `json:"version"`
		Gateways []struct {
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			OwnerID   string          `json:"ownerId"`
			CreatedAt json.RawMessage `json:"createdAt"`
		} `json:"gateways"`
		Keys      []keyRecord     `json:"keys"`
		Telemetry json.RawMessage `json:"telemetry,omitempty"`
	} `json:"registry"`
}

type dataV1State struct {
	diskState

	Version int `json:"version"`
}

// upgradeDataV1 keeps the default gateway's accounts, model policy, and keys.
// Data with other gateways is refused rather than merged or dropped.
func upgradeDataV1(raw []byte) (diskData, error) {
	var old dataV1
	if err := decodeStrict(raw, &old); err != nil || old.Version != 1 {
		return diskData{}, errors.New("gateway: version 1 data is malformed")
	}
	for _, gateway := range old.Registry.Gateways {
		if gateway.ID != defaultGatewayV1 {
			return diskData{}, errors.New("gateway: the data has gateways besides the default one, which this version cannot upgrade")
		}
	}
	for id := range old.States {
		if id != defaultGatewayV1 {
			return diskData{}, errors.New("gateway: the data has gateways besides the default one, which this version cannot upgrade")
		}
	}
	for _, key := range old.Registry.Keys {
		if key.GatewayID != defaultGatewayV1 {
			return diskData{}, errors.New("gateway: a key belongs to an unknown gateway")
		}
	}
	return diskData{Version: dataVersion, diskState: old.States[defaultGatewayV1].diskState, diskRegistry: diskRegistry{Keys: old.Registry.Keys}}, nil
}
