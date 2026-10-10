package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// keyRecord is one gateway API key. Only the SHA-256 hash of the secret is
// stored; the full key is shown once at creation and never persisted.
type keyRecord struct {
	legacyKeyFields

	ID        string     `json:"id"`
	GatewayID string     `json:"gatewayId"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	Hash      string     `json:"hash"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	// ExpiresAt is when the key stops authorizing requests. Nil never expires.
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	UsedRequests int64      `json:"usedRequests"`
	UsedTokens   int64      `json:"usedTokens"`
}

// legacyKeyFields holds per-key percentage accounting written by older
// builds. Loading accepts it and normalization drops it.
type legacyKeyFields struct {
	ProviderQuotas   json.RawMessage `json:"providerQuotas,omitempty"`
	PercentCharges   json.RawMessage `json:"percentCharges,omitempty"`
	PercentUncertain json.RawMessage `json:"percentUncertain,omitempty"`
	InFlight         json.RawMessage `json:"inFlight,omitempty"`
}

// expired reports whether the key's expiry has passed.
func (k keyRecord) expired(now time.Time) bool {
	return k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}

// keyView is the public metadata shape. It never contains the hash or secret.
type keyView struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Prefix       string     `json:"prefix"`
	CreatedAt    time.Time  `json:"createdAt"`
	RevokedAt    *time.Time `json:"revokedAt,omitempty"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	UsedRequests int64      `json:"usedRequests"`
	UsedTokens   int64      `json:"usedTokens"`
}

func (k keyRecord) view() keyView {
	return keyView{ID: k.ID, Name: k.Name, Prefix: k.Prefix, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt, ExpiresAt: k.ExpiresAt, UsedRequests: k.UsedRequests, UsedTokens: k.UsedTokens}
}

const keySecretPrefix = "vr_"

// secureID returns a short URL-safe random identifier.
func secureID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// secureKeySecret returns a fresh high-entropy API key secret. The returned
// hash is what callers persist.
func secureKeySecret() (string, string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	secret := keySecretPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(secret))
	return secret, hex.EncodeToString(sum[:]), nil
}

func keyPrefix(secret string) string {
	if len(secret) > 11 {
		return secret[:11]
	}
	return secret
}

func (s *server) listKeys(w http.ResponseWriter, _ *http.Request) {
	keys := []keyView{}
	for _, key := range s.manager.registry.snapshot().Keys {
		if key.GatewayID == s.gatewayID {
			keys = append(keys, key.view())
		}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].CreatedAt.Equal(keys[j].CreatedAt) {
			return keys[i].ID < keys[j].ID
		}
		return keys[i].CreatedAt.Before(keys[j].CreatedAt)
	})
	writeJSON(w, 200, map[string]any{"keys": keys})
}

func (s *server) createKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name      string          `json:"name"`
		ExpiresAt json.RawMessage `json:"expiresAt"`
	}
	if decodeKeyInput(w, r, &input) != nil {
		writeJSON(w, 400, map[string]string{"error": "Enter a key name and an optional expiry"})
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > keyNameMax {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("Key names must be 1 to %d characters", keyNameMax)})
		return
	}
	now := time.Now().UTC()
	expiresAt, ok := keyExpiry(input.ExpiresAt)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": keyExpiryFormatMessage})
		return
	}
	if expiresAt != nil && !expiresAt.After(now) {
		writeJSON(w, 400, map[string]string{"error": keyExpiryPastMessage})
		return
	}
	secret, hash, err := secureKeySecret()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Secure random source unavailable"})
		return
	}
	id, err := secureID()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Secure random source unavailable"})
		return
	}
	key := keyRecord{ID: id, GatewayID: s.gatewayID, Name: name, Prefix: keyPrefix(secret), Hash: hash, CreatedAt: now, ExpiresAt: expiresAt}
	err = s.manager.registry.update(func(registry *diskRegistry) error {
		registry.Keys = append(registry.Keys, key)
		return nil
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not save the key"})
		return
	}
	writeJSON(w, 201, map[string]any{"key": key.view(), "secret": secret})
}

func (s *server) patchKey(w http.ResponseWriter, r *http.Request) {
	patch, refusal := decodeKeyPatch(w, r)
	if refusal != "" {
		writeJSON(w, 400, map[string]string{"error": refusal})
		return
	}
	now := time.Now().UTC()
	var updated keyRecord
	err := s.manager.registry.update(func(registry *diskRegistry) error {
		key := findKey(registry, r.PathValue("id"))
		if key == nil || key.GatewayID != s.gatewayID {
			return errKeyNotFound
		}
		if err := patch.apply(key, now); err != nil {
			return err
		}
		updated = *key
		return nil
	})
	if err != nil {
		if errors.Is(err, errKeyExpiryPast) {
			writeJSON(w, 400, map[string]string{"error": keyExpiryPastMessage})
			return
		}
		if errors.Is(err, errKeyNotFound) {
			writeJSON(w, 404, map[string]string{"error": "Key not found"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": "Could not save the key"})
		return
	}
	writeJSON(w, 200, map[string]any{"key": updated.view()})
}

// keyPatch is a validated key update. A revocation ignores other fields.
type keyPatch struct {
	revoked   bool
	name      *string
	expiry    bool
	expiresAt *time.Time
}

// decodeKeyPatch reads a key update. It returns the message of a refusal.
func decodeKeyPatch(w http.ResponseWriter, r *http.Request) (keyPatch, string) {
	var input struct {
		Revoked   *bool           `json:"revoked"`
		Name      *string         `json:"name"`
		ExpiresAt json.RawMessage `json:"expiresAt"`
	}
	if decodeKeyInput(w, r, &input) != nil {
		return keyPatch{}, "Invalid key update"
	}
	if input.Revoked == nil && input.Name == nil && input.ExpiresAt == nil {
		return keyPatch{}, "Nothing to update"
	}
	if input.Revoked != nil && !*input.Revoked {
		return keyPatch{}, "Revocation cannot be undone"
	}
	patch := keyPatch{revoked: input.Revoked != nil && *input.Revoked, expiry: input.ExpiresAt != nil}
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" || len(name) > keyNameMax {
			return keyPatch{}, fmt.Sprintf("Key names must be 1 to %d characters", keyNameMax)
		}
		patch.name = &name
	}
	expiresAt, ok := keyExpiry(input.ExpiresAt)
	if !ok {
		return keyPatch{}, keyExpiryFormatMessage
	}
	patch.expiresAt = expiresAt
	return patch, ""
}

func (p keyPatch) apply(key *keyRecord, now time.Time) error {
	if p.revoked {
		if key.RevokedAt == nil {
			key.RevokedAt = &now
		}
		return nil
	}
	if p.name != nil {
		key.Name = *p.name
	}
	// The dashboard resends the stored expiry with every edit, so only a
	// changed value has to lie in the future.
	if p.expiry && !sameExpiry(key.ExpiresAt, p.expiresAt) {
		if p.expiresAt != nil && !p.expiresAt.After(now) {
			return errKeyExpiryPast
		}
		key.ExpiresAt = p.expiresAt
	}
	return nil
}

func (s *server) deleteKey(w http.ResponseWriter, r *http.Request) {
	err := s.manager.registry.update(func(registry *diskRegistry) error {
		for i := range registry.Keys {
			if registry.Keys[i].ID == r.PathValue("id") {
				if registry.Keys[i].GatewayID != s.gatewayID {
					return errKeyNotFound
				}
				registry.Keys = append(registry.Keys[:i], registry.Keys[i+1:]...)
				return nil
			}
		}
		return errKeyNotFound
	})
	if err != nil {
		if errors.Is(err, errKeyNotFound) {
			writeJSON(w, 404, map[string]string{"error": "Key not found"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": "Could not delete the key"})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

const (
	keyExpiryFormatMessage = "Expiry must be an RFC 3339 timestamp, or null for a key that never expires"
	keyExpiryPastMessage   = "Expiry must be in the future"
)

// keyExpiry decodes an optional expiry. A missing or null value means the key
// never expires; anything that is not an RFC 3339 timestamp is rejected.
func keyExpiry(raw json.RawMessage) (*time.Time, bool) {
	if raw == nil || string(raw) == "null" {
		return nil, true
	}
	var value time.Time
	if json.Unmarshal(raw, &value) != nil || value.IsZero() {
		return nil, false
	}
	value = value.UTC()
	return &value, true
}

func sameExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

var errKeyExpiryPast = errors.New("key expiry is in the past")

func decodeKeyInput(w http.ResponseWriter, r *http.Request, out any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing key data")
	}
	return nil
}
