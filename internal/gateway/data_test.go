package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDataPersistsAccountsKeysAndUsage(t *testing.T) {
	dir := t.TempDir()
	h, err := New(Config{DataDir: dir}, testAssets())
	if err != nil {
		t.Fatal(err)
	}
	s := h.(*server)
	defer s.Close()
	id, secret := newTestKey(t, s, `{"claude":{"fiveHour":25}}`)
	now := time.Now().UTC()
	expiry := now.Add(time.Hour)
	err = s.store.data.update(func(d *diskData) error {
		state := d.States[defaultGatewayID]
		state.Accounts = []storedAccount{{ID: "account", Provider: "claude", AuthMode: "oauth", AccessToken: "test-provider-secret"}}
		state.Policy.Aliases = map[string][]modelAlias{"claude": {{Name: "native", Alias: "public"}}}
		d.States[defaultGatewayID] = state
		d.Registry.Gateways = append(d.Registry.Gateways, gatewayRecord{ID: "other", Name: "Other", OwnerID: localAdminID, CreatedAt: now})
		d.States["other"] = diskState{Version: storeStateVersion, Accounts: []storedAccount{{ID: "second", Provider: "codex", AuthMode: "codex", AccessToken: "test-other-secret"}}}
		key := findKey(&d.Registry, id)
		key.UsedRequests, key.UsedTokens, key.ExpiresAt = 9, 123, &expiry
		d.Registry.Telemetry[defaultGatewayID] = []telemetryRecord{{ID: "request", GatewayID: defaultGatewayID, KeyID: id, Status: 200, Outcome: outcomeSuccess, TotalTokens: 123, UsageKnown: true}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := dataClone(s.store.data.state)
	engine, err := s.manager.engine("other")
	if err != nil {
		t.Fatal(err)
	}
	if engine.store.data != s.store.data || s.manager.registry.data != s.store.data {
		t.Fatal("views have separate stores")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h, err = New(Config{DataDir: dir}, testAssets())
	if err != nil {
		t.Fatal(err)
	}
	reopened := h.(*server)
	defer reopened.Close()
	if !reflect.DeepEqual(dataClone(reopened.store.data.state), before) {
		t.Fatal("restart changed accounts, keys, or usage")
	}
	if _, ok := reopened.manager.keyByHash(secret); !ok {
		t.Fatal("restart lost client key")
	}
	info, err := os.Stat(filepath.Join(dir, dataFileName))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("data permissions are not private")
	}
}

func TestDataRefusesMalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, dataFileName)
	raw := []byte(`{"version":999}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := openStore(dir); err == nil {
		s.close()
		t.Fatal("bad version accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(raw) {
		t.Fatal("failed load overwrote data")
	}
}

func TestDataLockAndFailedWriteKeepState(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if other, err := openStore(dir); !errors.Is(err, errStoreInUse) {
		if other != nil {
			other.close()
		}
		t.Fatalf("second owner: %v", err)
	}
	before := s.snapshot()
	realPath := s.data.path
	s.data.path = filepath.Join(dir, "missing", "file")
	if err := s.update(func(d *diskState) error {
		d.Accounts = append(d.Accounts, storedAccount{ID: "a", Provider: "claude", AuthMode: "oauth"})
		return nil
	}); err == nil {
		t.Fatal("failed write accepted")
	}
	s.data.path = realPath
	if !reflect.DeepEqual(s.snapshot(), before) {
		t.Fatal("failed write changed memory")
	}
	s.close()
	reopened, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if !reflect.DeepEqual(reopened.snapshot(), before) {
		t.Fatal("failed write changed disk")
	}
}

func TestDataRestartRetainsUnknownUsage(t *testing.T) {
	dir := t.TempDir()
	h, err := New(Config{DataDir: dir}, testAssets())
	if err != nil {
		t.Fatal(err)
	}
	s := h.(*server)
	id, _ := newTestKey(t, s, `{"claude":{"fiveHour":25}}`)
	if ok, _, _ := s.manager.reserve(inferenceKey{GatewayID: defaultGatewayID, KeyID: id, Provider: "claude"}); !ok {
		t.Fatal("reservation failed")
	}
	s.Close()
	for i := 0; i < 2; i++ {
		h, err = New(Config{DataDir: dir}, testAssets())
		if err != nil {
			t.Fatal(err)
		}
		s = h.(*server)
		key := s.manager.registry.snapshot().Keys[0]
		if key.InFlight != 0 || !key.PercentUncertain["claude"] || percentAdmission(key, "claude", time.Now()) == "" {
			t.Fatal("unfinished usage reopened quota")
		}
		s.Close()
	}
}

func TestDataViewsPreserveEachOthersWrites(t *testing.T) {
	s := testServer(t, Config{})
	id, _ := newTestKey(t, s, `{}`)
	if err := s.store.update(func(d *diskState) error {
		d.Accounts = append(d.Accounts, storedAccount{ID: "a", Provider: "claude", AuthMode: "oauth"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.manager.registry.update(func(d *diskRegistry) error { findKey(d, id).Name = "changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(s.store.snapshot().Accounts) != 1 {
		t.Fatal("key write lost accounts")
	}
	if err := s.store.update(func(d *diskState) error { d.Accounts[0].Label = "changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if s.manager.registry.snapshot().Keys[0].Name != "changed" {
		t.Fatal("account write lost keys")
	}
}
