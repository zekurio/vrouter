package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// storeTestTime is a fixed instant so restarts compare time.Time values
// exactly. All test state lives in temporary directories; no real home
// credentials are read or written.
var storeTestTime = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)

func storeStatePath(dir string) string { return filepath.Join(dir, storeFileName) }

func openTestStore(t *testing.T, dir string) *accountStore {
	t.Helper()
	store, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = store.close() })
	return store
}

func updateTestStore(t *testing.T, store *accountStore, mutate func(*diskState)) {
	t.Helper()
	if err := store.update(func(state *diskState) error {
		mutate(state)
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func readStoreState(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(storeStatePath(dir))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return data
}

func sampleStoredAccount(id string) storedAccount {
	return storedAccount{
		ID:           id,
		Provider:     "codex",
		Label:        "Work",
		Email:        "work@example.com",
		Plan:         "pro",
		AccountID:    "account-" + id,
		AccessToken:  "access-token-" + id,
		RefreshToken: "refresh-token-" + id,
		IDToken:      "id-token-" + id,
		ExpiresAt:    storeTestTime.Add(time.Hour),
		CreatedAt:    storeTestTime,
		AuthMode:     "chatgpt",
		ClientID:     "client-1",
		ClientSecret: "client-secret-1",
		Scopes:       []string{"openid", "profile"},
	}
}

func TestStoreInitializesPrivately(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "vrouter")
	store := openTestStore(t, dir)

	if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, err = %v; want 0700", info.Mode().Perm(), err)
	}
	stateInfo, err := os.Lstat(storeStatePath(dir))
	if err != nil || stateInfo.Mode().Perm() != 0o600 || !stateInfo.Mode().IsRegular() {
		t.Fatalf("state mode = %v, err = %v; want regular 0600", stateInfo.Mode().Perm(), err)
	}
	lockInfo, err := os.Lstat(filepath.Join(dir, storeLockName))
	if err != nil || lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode = %v, err = %v; want 0600", lockInfo.Mode().Perm(), err)
	}

	state := store.snapshot()
	if state.Version != storeStateVersion || len(state.Accounts) != 0 {
		t.Fatalf("initial state = %+v", state)
	}
	if state.Policy.Excluded == nil || state.Policy.Aliases == nil {
		t.Fatalf("initial policy maps are nil: %+v", state.Policy)
	}
	if state.Registration != (chatGPTRegistration{}) {
		t.Fatalf("initial registration = %+v", state.Registration)
	}
	var onDisk diskState
	if err := json.Unmarshal(readStoreState(t, dir), &onDisk); err != nil {
		t.Fatalf("state file is not JSON: %v", err)
	}
	if onDisk.Version != storeStateVersion {
		t.Fatalf("on-disk version = %d", onDisk.Version)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestStoreRestartPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store := openTestStore(t, dir)
	updateTestStore(t, store, func(state *diskState) {
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-1"))
		state.Policy.Excluded["codex"] = []string{"gpt-5*"}
		state.Policy.Aliases["claude"] = []modelAlias{{Name: "claude-sonnet-4", Alias: "sonnet"}}
		state.Registration = chatGPTRegistration{HostID: "host-1", ClientID: "client-1", ClientSecret: "registration-secret", RedirectURI: "http://localhost:1455/auth/callback"}
	})
	before := store.snapshot()

	if err := store.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	reopened := openTestStore(t, dir)
	after := reopened.snapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("state changed across restart:\nbefore = %+v\nafter  = %+v", before, after)
	}
	account := after.Accounts[0]
	if account.AccessToken != "access-token-acct-1" || account.RefreshToken != "refresh-token-acct-1" || len(account.Scopes) != 2 {
		t.Fatalf("account not persisted: %+v", account)
	}
	if after.Policy.Excluded["codex"][0] != "gpt-5*" || after.Policy.Aliases["claude"][0].Alias != "sonnet" {
		t.Fatalf("policy not persisted: %+v", after.Policy)
	}
	if after.Registration.ClientSecret != "registration-secret" {
		t.Fatalf("registration not persisted: %+v", after.Registration)
	}
}

func TestStoreSnapshotIsolation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store := openTestStore(t, dir)
	updateTestStore(t, store, func(state *diskState) {
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-1"))
		state.Policy.Excluded["codex"] = []string{"gpt-5*"}
		state.Policy.Aliases["claude"] = []modelAlias{{Name: "claude-sonnet-4", Alias: "sonnet"}}
	})

	first := store.snapshot()
	first.Accounts[0].Label = "mutated"
	first.Accounts[0].Scopes[0] = "mutated"
	first.Policy.Excluded["codex"][0] = "mutated"
	first.Policy.Aliases["claude"][0].Alias = "mutated"

	second := store.snapshot()
	if second.Accounts[0].Label != "Work" || second.Accounts[0].Scopes[0] != "openid" {
		t.Fatalf("snapshot mutation reached the store: %+v", second.Accounts[0])
	}
	if second.Policy.Excluded["codex"][0] != "gpt-5*" || second.Policy.Aliases["claude"][0].Alias != "sonnet" {
		t.Fatalf("policy mutation reached the store: %+v", second.Policy)
	}

	updateTestStore(t, store, func(state *diskState) { state.Accounts[0].Label = "Changed" })
	if second.Accounts[0].Label != "Work" {
		t.Fatal("later update altered an earlier snapshot")
	}
	if store.snapshot().Accounts[0].Label != "Changed" {
		t.Fatal("update did not apply")
	}

	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	persisted := openTestStore(t, dir).snapshot()
	if persisted.Accounts[0].Label != "Changed" || persisted.Policy.Excluded["codex"][0] != "gpt-5*" {
		t.Fatalf("mutations leaked to disk: %+v", persisted)
	}
}

func TestStoreUpdateRollbackOnCallbackError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store := openTestStore(t, dir)
	updateTestStore(t, store, func(state *diskState) {
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-1"))
	})
	before := store.snapshot()
	beforeFile := append([]byte(nil), readStoreState(t, dir)...)

	rejected := errors.New("callback rejected the change")
	err := store.update(func(state *diskState) error {
		state.Accounts[0].AccessToken = "mutated-token"
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-2"))
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatalf("callback error = %v, want the callback's own error", err)
	}
	if !reflect.DeepEqual(before, store.snapshot()) {
		t.Fatal("failed callback changed in-memory state")
	}
	if !bytes.Equal(beforeFile, readStoreState(t, dir)) {
		t.Fatal("failed callback rewrote the state file")
	}

	updateTestStore(t, store, func(state *diskState) {
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-2"))
	})
	if got := store.snapshot(); len(got.Accounts) != 2 {
		t.Fatalf("store unusable after a rejected update: %+v", got.Accounts)
	}
}

func TestStoreUpdateRollbackOnWriteFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store := openTestStore(t, dir)
	updateTestStore(t, store, func(state *diskState) {
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-1"))
	})
	before := store.snapshot()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	err := store.update(func(state *diskState) error {
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-2"))
		return nil
	})
	if err == nil {
		t.Fatal("update succeeded without a writable directory")
	}
	if errors.Is(err, errStoreClosed) {
		t.Fatalf("store reported closed instead of a write failure: %v", err)
	}
	if !reflect.DeepEqual(before, store.snapshot()) {
		t.Fatal("failed write changed in-memory state")
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	updateTestStore(t, store, func(state *diskState) {
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-3"))
	})
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	persisted := openTestStore(t, dir).snapshot()
	if len(persisted.Accounts) != 2 || persisted.Accounts[0].ID != "acct-1" || persisted.Accounts[1].ID != "acct-3" {
		t.Fatalf("recovery persisted wrong accounts: %+v", persisted.Accounts)
	}
}

func TestStoreUpdatePermissionFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permissions are not enforced")
	}
	dir := filepath.Join(t.TempDir(), "store")
	store := openTestStore(t, dir)
	updateTestStore(t, store, func(state *diskState) {
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-1"))
	})
	before := store.snapshot()

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := store.update(func(state *diskState) error {
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-2"))
		return nil
	})
	if err == nil {
		t.Fatal("update succeeded on a read-only directory")
	}
	if !reflect.DeepEqual(before, store.snapshot()) {
		t.Fatal("failed write changed in-memory state")
	}
}

func TestStoreUpdateValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store := openTestStore(t, dir)
	updateTestStore(t, store, func(state *diskState) {
		state.Accounts = append(state.Accounts, sampleStoredAccount("acct-1"))
	})
	before := store.snapshot()

	cases := []struct {
		name   string
		mutate func(*diskState)
	}{
		{"missing id", func(state *diskState) {
			state.Accounts = append(state.Accounts, storedAccount{Provider: "codex", AuthMode: "chatgpt"})
		}},
		{"unsupported provider", func(state *diskState) {
			state.Accounts = append(state.Accounts, storedAccount{ID: "acct-2", Provider: "gemini", AuthMode: "chatgpt"})
		}},
		{"unsupported auth mode", func(state *diskState) {
			state.Accounts = append(state.Accounts, storedAccount{ID: "acct-2", Provider: "codex", AuthMode: "password"})
		}},
		{"duplicate id", func(state *diskState) {
			state.Accounts = append(state.Accounts, sampleStoredAccount("acct-1"))
		}},
		{"unsupported version", func(state *diskState) { state.Version = storeStateVersion + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.update(func(state *diskState) error {
				tc.mutate(state)
				return nil
			})
			if err == nil {
				t.Fatal("invalid state was accepted")
			}
			if !reflect.DeepEqual(before, store.snapshot()) {
				t.Fatal("invalid draft changed in-memory state")
			}
		})
	}

	updateTestStore(t, store, func(state *diskState) {
		state.Accounts = append(state.Accounts, storedAccount{ID: "acct-mixed", Provider: "  Codex ", AuthMode: "ChatGPT", AccessToken: "token", CreatedAt: storeTestTime})
	})
	mixed := store.snapshot().Accounts[1]
	if mixed.Provider != "codex" || mixed.AuthMode != "chatgpt" {
		t.Fatalf("expected canonical provider/auth mode, got %q/%q", mixed.Provider, mixed.AuthMode)
	}
}

func TestStoreConcurrentUpdates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store := openTestStore(t, dir)
	const writers = 32

	var wg sync.WaitGroup
	errCh := make(chan error, writers+1)
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			state := store.snapshot()
			if state.Version != storeStateVersion {
				errCh <- fmt.Errorf("snapshot version = %d", state.Version)
				return
			}
			for _, account := range state.Accounts {
				if account.ID == "" || account.AccessToken == "" {
					errCh <- errors.New("snapshot contained an incomplete account")
					return
				}
			}
			runtime.Gosched()
		}
	}()

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := store.update(func(state *diskState) error {
				state.Accounts = append(state.Accounts, storedAccount{
					ID:          fmt.Sprintf("acct-%03d", i),
					Provider:    "claude",
					AuthMode:    "oauth",
					AccessToken: fmt.Sprintf("token-%03d", i),
					CreatedAt:   storeTestTime,
				})
				return nil
			})
			if err != nil {
				errCh <- fmt.Errorf("writer %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	<-readerDone
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	final := store.snapshot()
	if len(final.Accounts) != writers {
		t.Fatalf("account count = %d, want %d", len(final.Accounts), writers)
	}
	seen := map[string]bool{}
	for _, account := range final.Accounts {
		if seen[account.ID] {
			t.Fatalf("duplicate account %q", account.ID)
		}
		seen[account.ID] = true
	}

	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	persisted := openTestStore(t, dir).snapshot()
	if !reflect.DeepEqual(final, persisted) {
		t.Fatal("persisted state diverged from memory")
	}
}

func TestStoreSingleOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	first := openTestStore(t, dir)

	if _, err := openStore(dir); !errors.Is(err, errStoreInUse) {
		t.Fatalf("second open error = %v, want errStoreInUse", err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	second := openTestStore(t, dir)
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
}

// storeLockHelper re-executes this test binary in a fresh process so the lock
// is exercised across real process boundaries, not just two file descriptors.
func storeLockHelper(t *testing.T, dir, mode string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreLockHelper$", "-test.count=1", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "VROUTER_STORE_HELPER_DIR="+dir, "VROUTER_STORE_HELPER_MODE="+mode)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestStoreCrossProcessLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store := openTestStore(t, dir)

	if out, err := storeLockHelper(t, dir, "blocked"); err != nil {
		t.Fatalf("helper opened a locked store: %v\n%s", err, out)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	if out, err := storeLockHelper(t, dir, "open"); err != nil {
		t.Fatalf("helper could not open an unlocked store: %v\n%s", err, out)
	}
}

func TestStoreLockHelper(t *testing.T) {
	dir := os.Getenv("VROUTER_STORE_HELPER_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	switch mode := os.Getenv("VROUTER_STORE_HELPER_MODE"); mode {
	case "blocked":
		if _, err := openStore(dir); !errors.Is(err, errStoreInUse) {
			t.Fatalf("helper open = %v, want errStoreInUse", err)
		}
	case "open":
		store, err := openStore(dir)
		if err != nil {
			t.Fatalf("helper open: %v", err)
		}
		if err := store.close(); err != nil {
			t.Fatalf("helper close: %v", err)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func TestStoreClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	store := openTestStore(t, dir)
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("close is not idempotent: %v", err)
	}
	if err := store.update(func(*diskState) error { return nil }); !errors.Is(err, errStoreClosed) {
		t.Fatalf("update after close = %v, want errStoreClosed", err)
	}
	if state := store.snapshot(); state.Version != storeStateVersion {
		t.Fatalf("snapshot after close = %+v", state)
	}
}

func TestStoreRejectsMalformedState(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"invalid json", `{"version":1,"accounts":[`},
		{"not an object", `[]`},
		{"missing version", `{"accounts":[]}`},
		{"unsupported version", `{"version":99,"accounts":[]}`},
		{"unsupported version with secret", `{"version":99,"accounts":[{"id":"a","provider":"codex","auth_mode":"chatgpt","access_token":"super-secret-token"}]}`},
		{"unknown field", `{"version":1,"accounts":[],"extra":"super-secret-token"}`},
		{"wrong field type", `{"version":1,"accounts":"nope"}`},
		{"trailing data", `{"version":1,"accounts":[]}{"version":1}`},
		{"missing account id", `{"version":1,"accounts":[{"provider":"codex","auth_mode":"chatgpt"}]}`},
		{"unsupported provider", `{"version":1,"accounts":[{"id":"a","provider":"other","auth_mode":"chatgpt"}]}`},
		{"unsupported auth mode", `{"version":1,"accounts":[{"id":"a","provider":"codex","auth_mode":"password"}]}`},
		{"duplicate account id", `{"version":1,"accounts":[{"id":"a","provider":"codex","auth_mode":"chatgpt"},{"id":"a","provider":"codex","auth_mode":"chatgpt"}]}`},
		{"syntax error hides secret", `{"version":1,"access_token":"super-secret-token",`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "store")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(storeStatePath(dir), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := openStore(dir); err == nil {
				t.Fatal("malformed state was accepted")
			} else if strings.Contains(err.Error(), "super-secret-token") {
				t.Fatalf("error leaked a secret: %v", err)
			}
			if _, err := openStore(dir); err == nil || errors.Is(err, errStoreInUse) {
				t.Fatalf("failed open left the store locked: %v", err)
			}
		})
	}
}

func TestStoreLoadsValidState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"version":1,"accounts":[{"id":"a","provider":"Codex","auth_mode":"ChatGPT","access_token":"token","created_at":"2025-03-04T05:06:07Z"}]}`
	if err := os.WriteFile(storeStatePath(dir), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, dir)
	state := store.snapshot()
	if len(state.Accounts) != 1 || state.Accounts[0].ID != "a" {
		t.Fatalf("accounts = %+v", state.Accounts)
	}
	if state.Accounts[0].Provider != "codex" || state.Accounts[0].AuthMode != "chatgpt" {
		t.Fatalf("enums not canonical: %+v", state.Accounts[0])
	}
	if state.Policy.Excluded == nil || state.Policy.Aliases == nil {
		t.Fatalf("policy not normalized: %+v", state.Policy)
	}
	if info, err := os.Lstat(storeStatePath(dir)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode not restricted to 0600: %v, %v", info.Mode().Perm(), err)
	}
}

func TestStoreRefusesSymlinks(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Symlink("target", probe); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	t.Run("directory", func(t *testing.T) {
		realDir := filepath.Join(t.TempDir(), "real")
		if err := os.MkdirAll(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "store-link")
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		if _, err := openStore(link); err == nil {
			t.Fatal("symlinked store directory was accepted")
		}
	})

	t.Run("state file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "store")
		store := openTestStore(t, dir)
		if err := store.close(); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(outside, []byte(`{"version":1,"accounts":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(storeStatePath(dir)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, storeStatePath(dir)); err != nil {
			t.Fatal(err)
		}
		if _, err := openStore(dir); err == nil {
			t.Fatal("symlinked state file was accepted")
		}
	})

	t.Run("lock file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "store")
		store := openTestStore(t, dir)
		if err := store.close(); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.lock")
		if err := os.WriteFile(outside, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, storeLockName)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, storeLockName)); err != nil {
			t.Fatal(err)
		}
		if _, err := openStore(dir); err == nil {
			t.Fatal("symlinked lock file was accepted")
		}
	})
}

func TestStoreArgumentValidation(t *testing.T) {
	if _, err := openStore(""); err == nil {
		t.Fatal("empty store directory was accepted")
	}
	store := openTestStore(t, filepath.Join(t.TempDir(), "store"))
	if err := store.update(nil); err == nil {
		t.Fatal("nil update callback was accepted")
	}
}
