package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const storeStateVersion = 1

// storedAccount is one provider credential owned by this router. It is
// persisted only in the private state file; tokens must never reach logs,
// HTTP responses, or error strings.
type storedAccount struct {
	WindowTriggerAt time.Time `json:"window_trigger_at,omitzero"`
	ID              string    `json:"id"`
	Provider        string    `json:"provider"`
	Label           string    `json:"label"`
	Email           string    `json:"email,omitempty"`
	Plan            string    `json:"plan,omitempty"`
	AccountID       string    `json:"account_id,omitempty"`
	// Subject is the verified ID-token subject of a native Codex sign-in. It
	// binds a saved record to one user, so reconnecting a workspace shared by
	// several users can never silently switch accounts. Imports have no
	// verified subject and require a new sign-in to bind one.
	Subject      string    `json:"subject,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	CreatedAt    time.Time `json:"created_at"`
	Disabled     bool      `json:"disabled"`
	AuthMode     string    `json:"auth_mode"`
	ClientID     string    `json:"client_id,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
}

// diskState holds one gateway's accounts and model settings in the shared file.
type diskState struct {
	Version       int                     `json:"version"`
	Accounts      []storedAccount         `json:"accounts"`
	Policy        modelPolicy             `json:"policy"`
	ResetAttempts map[string]resetAttempt `json:"reset_attempts,omitempty"`
}

var (
	errStoreClosed = errors.New("gateway: account store is closed")
	errStoreInUse  = errors.New("gateway: account store is owned by another process")
)

// accountStore is a gateway-scoped view of the shared data store.
// Only the root view owns the process lock.
type accountStore struct {
	data      *dataStore
	gatewayID string
	owner     bool
}

func openStore(dir string) (*accountStore, error) {
	data, err := openDataStore(dir)
	if err != nil {
		return nil, err
	}
	return &accountStore{data: data, gatewayID: defaultGatewayID, owner: true}, nil
}

func (s *accountStore) close() error {
	if s.owner {
		return s.data.close()
	}
	return nil
}

func (s *accountStore) snapshot() diskState {
	s.data.mu.Lock()
	defer s.data.mu.Unlock()
	return storeClone(s.data.state.States[s.gatewayID])
}

func (s *accountStore) update(mutate func(*diskState) error) error {
	if mutate == nil {
		return errors.New("gateway: account update requires a callback")
	}
	return s.data.update(func(d *diskData) error {
		state, ok := d.States[s.gatewayID]
		if !ok {
			return errGatewayNotFound
		}
		if err := mutate(&state); err != nil {
			return err
		}
		d.States[s.gatewayID] = state
		return nil
	})
}

func storeAcquireLock(path string) (*os.File, error) {
	if err := storeRefuseSymlink(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is the lock file inside the configured data directory
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

// atomicWritePrivate commits the shared data file with private permissions.
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
	f, err := os.Open(dir) //nolint:gosec // dir is the configured data directory
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
		case "codex", "oauth", "api_key":
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
		maps.Copy(out.ResetAttempts, state.ResetAttempts)
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
