package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const keyNameMax = 64

var (
	errKeyNotFound = errors.New("gateway: API key not found")

	recordIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	keyHashPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// diskRegistry holds the users and their client keys in the data file.
type diskRegistry struct {
	Users []userRecord `json:"users"`
	Keys  []keyRecord  `json:"keys"`
}

// registryStore is the key view of the same data store as the accounts. It
// has no file, lock, or independent persistence path.
type registryStore struct{ data *dataStore }

func (r *registryStore) snapshot() diskRegistry {
	r.data.mu.Lock()
	defer r.data.mu.Unlock()
	return registryClone(r.data.state.diskRegistry)
}

func (r *registryStore) update(mutate func(*diskRegistry) error) error {
	if mutate == nil {
		return errors.New("gateway: key update requires a callback")
	}
	return r.data.update(func(d *diskData) error { return mutate(&d.diskRegistry) })
}

// registryNormalize makes nil collections concrete and drops counters older
// builds kept on each key. SQLite owns accounting.
func registryNormalize(state *diskRegistry) {
	if state.Users == nil {
		state.Users = []userRecord{}
	}
	if state.Keys == nil {
		state.Keys = []keyRecord{}
	}
	for i := range state.Keys {
		state.Keys[i].Name = strings.TrimSpace(state.Keys[i].Name)
		state.Keys[i].legacyKeyFields = legacyKeyFields{}
		state.Keys[i].UsedRequests = 0
		state.Keys[i].UsedTokens = 0
	}
}

// registryValidate enforces the invariants of the user and key records.
// Errors name the field and position, never stored values.
func registryValidate(state diskRegistry) error {
	users, err := registryValidateUsers(state.Users)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(state.Keys))
	for i := range state.Keys {
		key := &state.Keys[i]
		if !recordIDPattern.MatchString(key.ID) {
			return fmt.Errorf("gateway: key %d has an invalid ID", i)
		}
		if _, duplicate := seen[key.ID]; duplicate {
			return errors.New("gateway: key IDs must be unique")
		}
		seen[key.ID] = struct{}{}
		if _, exists := users[key.UserID]; key.UserID != "" && !exists {
			return fmt.Errorf("gateway: key %d belongs to an unknown user", i)
		}
		if err := registryKeyFieldsError(i, key); err != nil {
			return err
		}
	}
	return nil
}

func registryKeyFieldsError(i int, key *keyRecord) error {
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
	return nil
}

// registryClone deep-copies a registry so no caller can alias memory owned by
// the store.
func registryClone(state diskRegistry) diskRegistry {
	out := diskRegistry{Users: make([]userRecord, len(state.Users)), Keys: make([]keyRecord, len(state.Keys))}
	for i, user := range state.Users {
		out.Users[i] = user
		if user.DisabledAt != nil {
			disabled := *user.DisabledAt
			out.Users[i].DisabledAt = &disabled
		}
	}
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

func (s *server) hasAnyKeys() bool {
	for _, key := range s.registry.snapshot().Keys {
		if key.RevokedAt == nil && !key.expired(time.Now()) {
			return true
		}
	}
	return false
}

// keyByHash finds a live API key by its secret. Revoked keys are reported as
// missing so their state is never confirmed over HTTP. Expired keys are
// returned; the caller tells their holder why they stopped working.
func (s *server) keyByHash(secret string) (keyRecord, bool) {
	if secret == "" {
		return keyRecord{}, false
	}
	sum := sha256.Sum256([]byte(secret))
	digest := hex.EncodeToString(sum[:])
	for _, key := range s.registry.snapshot().Keys {
		if key.RevokedAt != nil {
			continue
		}
		if len(key.Hash) == len(digest) && subtle.ConstantTimeCompare([]byte(key.Hash), []byte(digest)) == 1 {
			return key, true
		}
	}
	return keyRecord{}, false
}
