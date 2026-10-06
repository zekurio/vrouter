package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// keyRecord is one gateway API key. Only the SHA-256 hash of the secret is
// stored; the full key is shown once at creation and never persisted.
type keyRecord struct {
	ID             string     `json:"id"`
	GatewayID      string     `json:"gatewayId"`
	Name           string     `json:"name"`
	Prefix         string     `json:"prefix"`
	Hash           string     `json:"hash"`
	CreatedAt      time.Time  `json:"createdAt"`
	RevokedAt      *time.Time `json:"revokedAt,omitempty"`
	LimitRequests  int64      `json:"limitRequests"`
	LimitTokens    int64      `json:"limitTokens"`
	UsedRequests   int64      `json:"usedRequests"`
	UsedTokens     int64      `json:"usedTokens"`
	UsageUncertain bool       `json:"usageUncertain,omitempty"`
	// InFlight counts reservations that have not settled yet. It is persisted
	// so a crash cannot hide unfinished work: on load, any nonzero count turns
	// into UsageUncertain and is cleared.
	InFlight int `json:"inFlight,omitempty"`
}

// keyView is the public metadata shape. It never contains the hash or secret.
type keyView struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Prefix         string     `json:"prefix"`
	CreatedAt      time.Time  `json:"createdAt"`
	RevokedAt      *time.Time `json:"revokedAt,omitempty"`
	LimitRequests  int64      `json:"limitRequests"`
	LimitTokens    int64      `json:"limitTokens"`
	UsedRequests   int64      `json:"usedRequests"`
	UsedTokens     int64      `json:"usedTokens"`
	UsageUncertain bool       `json:"usageUncertain"`
}

func (k keyRecord) view() keyView {
	return keyView{ID: k.ID, Name: k.Name, Prefix: k.Prefix, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt, LimitRequests: k.LimitRequests, LimitTokens: k.LimitTokens, UsedRequests: k.UsedRequests, UsedTokens: k.UsedTokens, UsageUncertain: k.UsageUncertain}
}

const keySecretPrefix = "vr_"

// maxKeyLimit is the largest accepted lifetime limit. It stays inside the
// JavaScript safe-integer range so the UI can round-trip it exactly, and it is
// far below the internal counter clamp so a measured counter can always reach
// an accepted limit.
const maxKeyLimit = int64(1)<<53 - 1

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
func secureKeySecret() (secret, hash string, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	secret = keySecretPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(secret))
	return secret, hex.EncodeToString(sum[:]), nil
}

func keyPrefix(secret string) string {
	if len(secret) > 11 {
		return secret[:11]
	}
	return secret
}

// keysUnavailable answers demo requests. Key management has no meaning
// without durable storage, so it fails with 409 instead of pretending.
func (s *server) keysUnavailable(w http.ResponseWriter) bool {
	if s.manager == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Key management is unavailable in demo mode"})
		return true
	}
	return false
}

func (s *server) listKeys(w http.ResponseWriter, r *http.Request) {
	if s.keysUnavailable(w) {
		return
	}
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
	if s.keysUnavailable(w) {
		return
	}
	var input struct {
		Name          string `json:"name"`
		LimitRequests *int64 `json:"limitRequests"`
		LimitTokens   *int64 `json:"limitTokens"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
		writeJSON(w, 400, map[string]string{"error": "Enter a key name and lifetime limits"})
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > keyNameMax {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("Key names must be 1 to %d characters", keyNameMax)})
		return
	}
	limitRequests, ok := keyLimit(input.LimitRequests)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("Request limits must be 0 or a whole number up to %d", maxKeyLimit)})
		return
	}
	limitTokens, ok := keyLimit(input.LimitTokens)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("Token limits must be 0 or a whole number up to %d", maxKeyLimit)})
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
	key := keyRecord{ID: id, GatewayID: s.gatewayID, Name: name, Prefix: keyPrefix(secret), Hash: hash, CreatedAt: time.Now().UTC(), LimitRequests: limitRequests, LimitTokens: limitTokens}
	err = s.manager.update(func(registry *diskRegistry) error {
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
	if s.keysUnavailable(w) {
		return
	}
	var input struct {
		Revoked       *bool   `json:"revoked"`
		Name          *string `json:"name"`
		LimitRequests *int64  `json:"limitRequests"`
		LimitTokens   *int64  `json:"limitTokens"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
		writeJSON(w, 400, map[string]string{"error": "Invalid key update"})
		return
	}
	if input.Revoked == nil && input.Name == nil && input.LimitRequests == nil && input.LimitTokens == nil {
		writeJSON(w, 400, map[string]string{"error": "Nothing to update"})
		return
	}
	if input.Revoked != nil && !*input.Revoked {
		writeJSON(w, 400, map[string]string{"error": "Revocation cannot be undone"})
		return
	}
	name := ""
	hasName := input.Name != nil
	if hasName {
		name = strings.TrimSpace(*input.Name)
		if name == "" || len(name) > keyNameMax {
			writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("Key names must be 1 to %d characters", keyNameMax)})
			return
		}
	}
	limitRequests, ok := keyLimit(input.LimitRequests)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("Request limits must be 0 or a whole number up to %d", maxKeyLimit)})
		return
	}
	limitTokens, ok := keyLimit(input.LimitTokens)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("Token limits must be 0 or a whole number up to %d", maxKeyLimit)})
		return
	}
	revoked := input.Revoked != nil && *input.Revoked
	now := time.Now().UTC()
	var updated keyRecord
	err := s.manager.update(func(registry *diskRegistry) error {
		key := findKey(registry, r.PathValue("id"))
		if key == nil || key.GatewayID != s.gatewayID {
			return errKeyNotFound
		}
		if revoked {
			if key.RevokedAt == nil {
				key.RevokedAt = &now
			}
			updated = *key
			return nil
		}
		if hasName {
			key.Name = name
		}
		if input.LimitRequests != nil {
			key.LimitRequests = limitRequests
		}
		if input.LimitTokens != nil {
			key.LimitTokens = limitTokens
		}
		// An explicit token-budget change is the documented owner action that
		// clears an uncertain-usage block; a name change is not.
		if input.LimitTokens != nil {
			key.UsageUncertain = false
		}
		updated = *key
		return nil
	})
	if err != nil {
		if errors.Is(err, errKeyNotFound) {
			writeJSON(w, 404, map[string]string{"error": "Key not found"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": "Could not save the key"})
		return
	}
	writeJSON(w, 200, map[string]any{"key": updated.view()})
}

func (s *server) deleteKey(w http.ResponseWriter, r *http.Request) {
	if s.keysUnavailable(w) {
		return
	}
	err := s.manager.update(func(registry *diskRegistry) error {
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

// keyLimit decodes an optional lifetime limit. A missing or zero value means
// unlimited; negative, fractional and out-of-range values are rejected.
func keyLimit(value *int64) (int64, bool) {
	if value == nil {
		return 0, true
	}
	if *value < 0 || *value > maxKeyLimit {
		return 0, false
	}
	return *value, true
}
