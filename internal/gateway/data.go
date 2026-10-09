package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const dataFileName = "vrouter.json"
const dataMaxBytes = 128 << 20

// One file owns accounts, client keys, policy, and accounting. The scoped
// views cannot commit one half of a change without the other half.
type diskData struct {
	Version  int                  `json:"version"`
	States   map[string]diskState `json:"accounts"`
	Registry diskRegistry         `json:"registry"`
}

type dataStore struct {
	mu     sync.Mutex
	path   string
	state  diskData
	lock   *os.File
	closed bool
}

func openDataStore(dir string) (*dataStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("gateway: data directory is required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := storeRefuseSymlink(dir); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
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
	return s, nil
}

func (s *dataStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := errors.Join(syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN), s.lock.Close())
	s.lock = nil
	return err
}

func dataClone(d diskData) diskData {
	out := diskData{Version: d.Version, Registry: registryClone(d.Registry), States: make(map[string]diskState, len(d.States))}
	for id, state := range d.States {
		out.States[id] = storeClone(state)
	}
	return out
}

func normalizeData(d *diskData) error {
	if d.Version != 1 {
		return errors.New("gateway: data version is not supported")
	}
	registryNormalize(&d.Registry)
	if err := registryValidate(d.Registry); err != nil {
		return err
	}
	if len(d.States) != len(d.Registry.Gateways) {
		return errors.New("gateway: account stores do not match gateways")
	}
	for _, gateway := range d.Registry.Gateways {
		state, ok := d.States[gateway.ID]
		if !ok {
			return errors.New("gateway: missing gateway accounts")
		}
		storeNormalize(&state)
		if err := storeValidate(state); err != nil {
			return err
		}
		d.States[gateway.ID] = state
	}
	return nil
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
	raw, err := json.Marshal(d)
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
	f, err := os.Open(path)
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
	if err := f.Chmod(0600); err != nil {
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
	if errors.Is(err, os.ErrNotExist) {
		s.state = diskData{Version: 1, States: map[string]diskState{defaultGatewayID: {Version: storeStateVersion}}, Registry: diskRegistry{Version: registryVersion}}
		err = nil
	} else if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&s.state) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return errors.New("gateway: data is malformed")
		}
	}
	if err != nil {
		return err
	}
	if err := normalizeData(&s.state); err != nil {
		return err
	}
	return s.write(s.state)
}
