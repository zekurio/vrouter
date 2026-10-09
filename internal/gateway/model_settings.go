package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

type modelAlias struct {
	Name  string `json:"name"`
	Alias string `json:"alias"`
}
type modelPolicy struct {
	Excluded map[string][]string     `json:"excluded-models"`
	Aliases  map[string][]modelAlias `json:"model-alias"`
}
type managedModel struct {
	Model
	Enabled  bool   `json:"enabled"`
	Alias    string `json:"alias"`
	ReadOnly string `json:"readOnly,omitempty"`
}
type modelSettings struct {
	Models   []managedModel `json:"models"`
	Revision string         `json:"revision"`
}
type modelChange struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Enabled  *bool  `json:"enabled"`
	Alias    string `json:"alias"`
}

func policyRevision(p modelPolicy) string {
	data, _ := json.Marshal(p)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func policyChannel(provider string) string {
	switch provider {
	case "codex", "claude":
		return provider
	}
	return ""
}
func modelKey(provider, id string) string { return provider + "\x00" + id }
func wildcardMatches(pattern, id string) bool {
	pattern = "^" + strings.ReplaceAll(regexp.QuoteMeta(strings.ToLower(strings.TrimSpace(pattern))), `\*`, ".*") + "$"
	match, _ := regexp.MatchString(pattern, strings.ToLower(id))
	return match
}
func excludedModel(p modelPolicy, channel, id string) (bool, bool) {
	excluded, wildcard := false, false
	for _, pattern := range p.Excluded[channel] {
		if wildcardMatches(pattern, id) {
			excluded = true
			wildcard = wildcard || strings.Contains(pattern, "*")
		}
	}
	return excluded, wildcard
}
func (s *server) readModelSettings(ctx context.Context) (modelSettings, modelPolicy, error) {
	// Stay inside the dashboard's own request timeout when a provider stalls.
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	p := s.store.snapshot().Policy
	result := modelSettings{Models: []managedModel{}, Revision: policyRevision(p)}
	models, err := s.rawModels(ctx)
	rows := map[string]Model{}
	for _, m := range models {
		rows[modelKey(m.Provider, m.ID)] = m
	}
	// Hidden/renamed models remain editable even when no account advertises them.
	for _, name := range []string{"codex", "claude"} {
		channel := policyChannel(name)
		ids := append([]string{}, p.Excluded[channel]...)
		for _, a := range p.Aliases[channel] {
			ids = append(ids, a.Name)
		}
		for _, id := range ids {
			key := modelKey(name, id)
			if _, ok := rows[key]; !ok {
				rows[key] = Model{ID: id, Name: id, Provider: name}
			}
		}
	}
	for _, m := range rows {
		blocked, wildcard := excludedModel(p, policyChannel(m.Provider), m.ID)
		row := managedModel{Model: m, Enabled: !blocked}
		if wildcard {
			row.ReadOnly = "Excluded by a wildcard policy"
		}
		for _, a := range p.Aliases[policyChannel(m.Provider)] {
			if a.Name == m.ID {
				row.Alias = a.Alias
			}
		}
		result.Models = append(result.Models, row)
	}
	sort.Slice(result.Models, func(i, j int) bool {
		return modelKey(result.Models[i].Provider, result.Models[i].ID) < modelKey(result.Models[j].Provider, result.Models[j].ID)
	})
	return result, p, err
}
func (s *server) getModelSettings(w http.ResponseWriter, r *http.Request) {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	settings, _, err := s.readModelSettings(r.Context())
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "Could not load all provider catalogs. Refresh or reconnect unavailable accounts."})
		return
	}
	writeJSON(w, 200, settings)
}

var aliasID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (s *server) putModelSettings(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Revision string        `json:"revision"`
		Models   []modelChange `json:"models"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(input.Models) == 0 || len(input.Models) > 1000 {
		writeJSON(w, 400, map[string]string{"error": "Invalid model changes"})
		return
	}
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	settings, p, err := s.readModelSettings(r.Context())
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "Could not load provider catalogs"})
		return
	}
	if input.Revision != settings.Revision {
		writeJSON(w, 409, map[string]string{"error": "Model settings changed. Reload before saving."})
		return
	}
	next, patch, err := applyModelChanges(settings, p, input.Models)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	err = s.store.update(func(d *diskState) error {
		for k, v := range patch.Excluded {
			d.Policy.Excluded[k] = v
		}
		for k, v := range patch.Aliases {
			d.Policy.Aliases[k] = v
		}
		return nil
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not save model settings"})
		return
	}
	writeJSON(w, 200, next)
}

func applyModelChanges(settings modelSettings, p modelPolicy, changes []modelChange) (modelSettings, modelPolicy, error) {
	// Clone policy so failed validation cannot mutate a caller's snapshot.
	nextPolicy := storeClonePolicy(p)
	result := modelSettings{Models: append([]managedModel(nil), settings.Models...)}
	indices := map[string]int{}
	for i, m := range result.Models {
		indices[modelKey(m.Provider, m.ID)] = i
	}
	seen := map[string]bool{}
	touched := map[string]bool{}
	for _, change := range changes {
		key := modelKey(change.Provider, change.ID)
		i, exists := indices[key]
		if !exists || seen[key] || change.Enabled == nil {
			return result, p, fmt.Errorf("Unknown, duplicate, or incomplete model change: %s", change.ID)
		}
		seen[key] = true
		row := &result.Models[i]
		if row.ReadOnly != "" {
			return result, p, fmt.Errorf("%s: %s", row.ID, row.ReadOnly)
		}
		alias := strings.TrimSpace(change.Alias)
		if alias == row.ID {
			alias = ""
		}
		if alias != "" && !aliasID.MatchString(alias) {
			return result, p, errors.New("Aliases must start with a letter or number, use letters, numbers, dots, dashes or underscores, and be at most 128 characters.")
		}
		row.Enabled, row.Alias = *change.Enabled, alias
		channel := policyChannel(row.Provider)
		touched[channel] = true
		excluded := []string{}
		for _, id := range nextPolicy.Excluded[channel] {
			if !strings.EqualFold(id, row.ID) {
				excluded = append(excluded, id)
			}
		}
		if !row.Enabled {
			excluded = append(excluded, row.ID)
		}
		nextPolicy.Excluded[channel] = excluded
		aliases := []modelAlias{}
		for _, a := range nextPolicy.Aliases[channel] {
			if !strings.EqualFold(a.Name, row.ID) {
				aliases = append(aliases, a)
			}
		}
		if alias != "" {
			aliases = append(aliases, modelAlias{Name: row.ID, Alias: alias})
		}
		nextPolicy.Aliases[channel] = aliases
	}
	// Reserve native IDs too: an alias must never shadow another model or alias.
	owners := map[string]string{}
	for _, m := range result.Models {
		owners[strings.ToLower(m.ID)] = m.ID
	}
	for _, m := range result.Models {
		if m.Alias == "" {
			continue
		}
		key := strings.ToLower(m.Alias)
		if owner, exists := owners[key]; exists && owner != m.ID {
			return result, p, fmt.Errorf("Alias %q conflicts with another model ID or alias.", m.Alias)
		}
		owners[key] = m.ID
	}
	result.Revision = policyRevision(nextPolicy)
	patch := modelPolicy{Excluded: map[string][]string{}, Aliases: map[string][]modelAlias{}}
	for channel := range touched {
		patch.Excluded[channel] = nextPolicy.Excluded[channel]
		patch.Aliases[channel] = nextPolicy.Aliases[channel]
	}
	return result, patch, nil
}
