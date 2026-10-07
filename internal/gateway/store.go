package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// storeStateVersion is the only on-disk schema this build can load. A
	// different version is refused instead of being partially interpreted.
	storeStateVersion = 1
	storeFileName     = "state.json"
	storeLockName     = "state.lock"
	// storeMaxBytes bounds how much of a state file is decoded. The file holds
	// a handful of credentials, so anything larger is treated as corruption.
	storeMaxBytes = 16 << 20
)

// storedAccount is one provider credential owned by this router. It is
// persisted only in the private state file; tokens must never reach logs,
// HTTP responses, or error strings.
type storedAccount struct {
	WindowTriggerAt time.Time `json:"window_trigger_at,omitempty"`
	ID              string    `json:"id"`
	Provider        string    `json:"provider"`
	Label           string    `json:"label"`
	Email           string    `json:"email,omitempty"`
	Plan            string    `json:"plan,omitempty"`
	AccountID       string    `json:"account_id,omitempty"`
	// Subject is the verified ID-token subject of a native Codex sign-in. It
	// binds a saved record to one user, so reconnecting a workspace shared by
	// several users can never silently switch accounts. Legacy imports have no
	// subject until their first native reconnect.
	Subject      string    `json:"subject,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Disabled     bool      `json:"disabled"`
	AuthMode     string    `json:"auth_mode"`
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
}

// chatGPTRegistration preserves old state files without losing stored data.
// Experimental ChatGPT connections are no longer usable or refreshable.
type chatGPTRegistration struct {
	HostID       string `json:"host_id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	RedirectURI  string `json:"redirect_uri"`
}

// diskState is the complete persisted state. It is replaced atomically as a
// whole, so readers never observe a partially written document.
type diskState struct {
	Version       int                     `json:"version"`
	Accounts      []storedAccount         `json:"accounts"`
	Policy        modelPolicy             `json:"policy"`
	Registration  chatGPTRegistration     `json:"chatgpt_registration"`
	ResetAttempts map[string]resetAttempt `json:"reset_attempts,omitempty"`
}

var (
	errStoreClosed = errors.New("gateway: account store is closed")
	errStoreInUse  = errors.New("gateway: account store is owned by another process")
)

// accountStore owns a single state.json beneath dir. Access is serialized in
// memory and ownership is enforced across processes with a non-blocking file
// lock, so two routers can never write the same state.
type accountStore struct {
	mu     sync.Mutex
	dir    string
	path   string
	lock   *os.File
	state  diskState
	closed bool
}

// openStore opens or initializes the store in dir. The directory is created
// with 0700 permissions and the state and lock files with 0600. A malformed
// or unsupported state file, or a second owner of the directory, is refused.
func openStore(dir string) (*accountStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("gateway: account store directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("gateway: create account store directory: %w", err)
	}
	if err := storeRefuseSymlink(dir); err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("gateway: open account store directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("gateway: account store path is not a directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("gateway: restrict account store directory permissions: %w", err)
	}
	s := &accountStore{dir: dir, path: filepath.Join(dir, storeFileName)}
	lock, err := storeAcquireLock(filepath.Join(dir, storeLockName))
	if err != nil {
		return nil, err
	}
	s.lock = lock
	if err := s.load(); err != nil {
		_ = s.releaseLock()
		return nil, err
	}
	return s, nil
}

// close releases the cross-process lock. It is idempotent and safe to call
// after the server has stopped serving requests.
func (s *accountStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.releaseLock()
}

// snapshot returns a deep copy of the current state. Callers may read and
// mutate the result freely; the store is unaffected.
func (s *accountStore) snapshot() diskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return storeClone(s.state)
}

// update applies mutate to a private copy of the state and persists it. The
// in-memory state is swapped only after the file has been replaced
// atomically, so a failed callback or write leaves both memory and disk on
// the previous revision.
func (s *accountStore) update(mutate func(*diskState) error) error {
	if mutate == nil {
		return errors.New("gateway: account store update requires a callback")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStoreClosed
	}
	draft := storeClone(s.state)
	if err := mutate(&draft); err != nil {
		return err
	}
	storeNormalize(&draft)
	if err := storeValidate(draft); err != nil {
		return err
	}
	if err := storeWriteFile(s.path, draft); err != nil {
		return err
	}
	s.state = draft
	return nil
}

// load reads state.json, or creates it with an empty revision when absent.
func (s *accountStore) load() error {
	if err := storeRefuseSymlink(s.path); err != nil {
		return err
	}
	f, err := os.Open(s.path)
	var data []byte
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(f, storeMaxBytes+1))
		_ = f.Close()
	}
	switch {
	case err == nil:
		state, err := storeDecode(data)
		if err != nil {
			return err
		}
		if err := os.Chmod(s.path, 0o600); err != nil {
			return fmt.Errorf("gateway: restrict account state permissions: %w", err)
		}
		s.state = state
		return nil
	case errors.Is(err, fs.ErrNotExist):
		s.state = diskState{Version: storeStateVersion}
		storeNormalize(&s.state)
		return storeWriteFile(s.path, s.state)
	default:
		return fmt.Errorf("gateway: read account state: %w", err)
	}
}

func (s *accountStore) releaseLock() error {
	if s.lock == nil {
		return nil
	}
	f := s.lock
	s.lock = nil
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	switch {
	case unlockErr != nil:
		return fmt.Errorf("gateway: unlock account store: %w", unlockErr)
	case closeErr != nil:
		return fmt.Errorf("gateway: close account store lock: %w", closeErr)
	}
	return nil
}

func storeAcquireLock(path string) (*os.File, error) {
	if err := storeRefuseSymlink(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("gateway: open account store lock: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("gateway: restrict account store lock permissions: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errStoreInUse
		}
		return nil, fmt.Errorf("gateway: lock account store: %w", err)
	}
	return f, nil
}

// storeDecode parses a persisted state. Errors never quote file contents
// beyond what encoding/json reports about structure, and no account value is
// included by this package.
func storeDecode(data []byte) (diskState, error) {
	if len(data) > storeMaxBytes {
		return diskState{}, errors.New("gateway: account state file is too large")
	}
	var state diskState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return diskState{}, errors.New("gateway: account state is malformed")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return diskState{}, errors.New("gateway: account state is malformed: unexpected trailing data")
	}
	if state.Version != storeStateVersion {
		return diskState{}, fmt.Errorf("gateway: account state version %d is not supported", state.Version)
	}
	storeNormalize(&state)
	if err := storeValidate(state); err != nil {
		return diskState{}, err
	}
	return state, nil
}

// storeWriteFile replaces path atomically with a private 0600 file: the new
// content is written to a temporary file in the same directory, fsynced, and
// renamed over the old revision.
func storeWriteFile(path string, state diskState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("gateway: encode account state: %w", err)
	}
	data = append(data, '\n')
	return atomicWritePrivate(path, "account state", data)
}

// atomicWritePrivate is the shared durable-write primitive for state and
// registry files. The label only shapes error text.
func atomicWritePrivate(path, label string, data []byte) error {
	if err := storeRefuseSymlink(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("gateway: write %s: %w", label, err)
	}
	tempName := temp.Name()
	discard := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	if _, err := temp.Write(data); err != nil {
		discard()
		return fmt.Errorf("gateway: write %s: %w", label, err)
	}
	if err := temp.Chmod(0o600); err != nil {
		discard()
		return fmt.Errorf("gateway: secure %s: %w", label, err)
	}
	if err := temp.Sync(); err != nil {
		discard()
		return fmt.Errorf("gateway: sync %s: %w", label, err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("gateway: close %s: %w", label, err)
	}
	if err := os.Rename(tempName, path); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("gateway: replace %s: %w", label, err)
	}
	storeSyncDir(dir)
	return nil
}

// storeRefuseSymlink rejects a path that is a symlink. MkdirAll and ReadFile
// would otherwise follow one, letting a link redirect secrets elsewhere.
func storeRefuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("gateway: inspect account store path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("gateway: account store path %q is a symbolic link", path)
	}
	return nil
}

// storeSyncDir makes the rename durable. Directory fsync is best effort: some
// unix filesystems reject it, and the write itself has already succeeded.
func storeSyncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
}

// storeNormalize makes nil collections concrete and lowercases the closed
// provider and auth-mode vocabularies so on-disk state is canonical.
func storeNormalize(state *diskState) {
	if state.Accounts == nil {
		state.Accounts = []storedAccount{}
	}
	if state.Policy.Excluded == nil {
		state.Policy.Excluded = map[string][]string{}
	}
	if state.Policy.Aliases == nil {
		state.Policy.Aliases = map[string][]modelAlias{}
	}
	state.Policy.LegacyContext = nil
	for i := range state.Accounts {
		state.Accounts[i].Provider = strings.ToLower(strings.TrimSpace(state.Accounts[i].Provider))
		state.Accounts[i].AuthMode = strings.ToLower(strings.TrimSpace(state.Accounts[i].AuthMode))
	}
}

// storeValidate enforces the invariants of the credential vocabulary. Errors
// name the field and position, never the stored values.
func storeValidate(state diskState) error {
	if state.Version != storeStateVersion {
		return fmt.Errorf("gateway: account state version %d is not supported", state.Version)
	}
	seen := make(map[string]struct{}, len(state.Accounts))
	for i := range state.Accounts {
		account := &state.Accounts[i]
		if account.ID == "" {
			return fmt.Errorf("gateway: account %d is missing an ID", i)
		}
		if _, duplicate := seen[account.ID]; duplicate {
			return errors.New("gateway: account IDs must be unique")
		}
		seen[account.ID] = struct{}{}
		switch account.Provider {
		case "codex", "claude":
		default:
			return fmt.Errorf("gateway: account %d has an unsupported provider", i)
		}
		switch account.AuthMode {
		case "chatgpt", "codex", "oauth", "api_key":
		default:
			return fmt.Errorf("gateway: account %d has an unsupported auth mode", i)
		}
	}
	return nil
}

// storeClone deep-copies a state. Slices and maps are duplicated so no caller
// can alias memory owned by the store.
func storeClone(state diskState) diskState {
	out := state
	if state.ResetAttempts != nil {
		out.ResetAttempts = make(map[string]resetAttempt, len(state.ResetAttempts))
		for id, attempt := range state.ResetAttempts {
			out.ResetAttempts[id] = attempt
		}
	}
	out.Policy = storeClonePolicy(state.Policy)
	if state.Accounts != nil {
		out.Accounts = make([]storedAccount, len(state.Accounts))
		for i, account := range state.Accounts {
			out.Accounts[i] = account
			if account.Scopes != nil {
				scopes := make([]string, len(account.Scopes))
				copy(scopes, account.Scopes)
				out.Accounts[i].Scopes = scopes
			}
		}
	}
	return out
}

func storeClonePolicy(policy modelPolicy) modelPolicy {
	out := modelPolicy{}
	if policy.Excluded != nil {
		out.Excluded = make(map[string][]string, len(policy.Excluded))
		for key, values := range policy.Excluded {
			cloned := make([]string, len(values))
			copy(cloned, values)
			out.Excluded[key] = cloned
		}
	}
	if policy.Aliases != nil {
		out.Aliases = make(map[string][]modelAlias, len(policy.Aliases))
		for key, values := range policy.Aliases {
			cloned := make([]modelAlias, len(values))
			copy(cloned, values)
			out.Aliases[key] = cloned
		}
	}
	return out
}
