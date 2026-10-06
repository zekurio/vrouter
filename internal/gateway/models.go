package gateway

import (
	"context"
	"errors"
	"sort"
	"time"
)

// The Codex catalog gates models by client version. Keep this in sync with
// the native client protocol supported by the router.
const codexCatalogClientVersion = "0.160.1"

type catalogCache struct {
	Models  []Model
	Expires time.Time
	Err     error
}
type providerModel struct {
	ID            string   `json:"id"`
	Slug          string   `json:"slug"`
	Name          string   `json:"display_name"`
	Visibility    string   `json:"visibility"`
	Context       int      `json:"context_window"`
	ContextLength int      `json:"context_length"`
	MaxInput      int      `json:"max_input_tokens"`
	MaxOutput     int      `json:"max_output_tokens"`
	MaxTokens     int      `json:"max_tokens"`
	Inputs        []string `json:"input_modalities"`
}

func (s *server) accountModels(ctx context.Context, a storedAccount) ([]Model, error) {
	if !routableAuth(a) {
		return nil, errors.New("unsupported account authentication")
	}
	s.catalogMu.Lock()
	cached, ok := s.catalogs[a.ID]
	s.catalogMu.Unlock()
	if ok && time.Now().Before(cached.Expires) {
		return cached.Models, cached.Err
	}
	target := "https://api.openai.com/v1/models"
	if a.Provider == "claude" {
		target = "https://api.anthropic.com/v1/models?limit=1000"
	} else if a.AuthMode == "codex" {
		target = "https://chatgpt.com/backend-api/codex/models?client_version=" + codexCatalogClientVersion
	}
	var response struct {
		Data   []providerModel `json:"data"`
		Models []providerModel `json:"models"`
	}
	err := s.providerJSON(ctx, a, target, &response)
	result := []Model{}
	if err == nil {
		rows := response.Data
		if a.AuthMode == "codex" {
			rows = response.Models
		}
		if rows == nil {
			err = errors.New("provider model list missing")
		}
		seen := map[string]bool{}
		for _, m := range rows {
			id := m.ID
			if m.Slug != "" {
				id = m.Slug
			}
			if id == "" || seen[id] || ((a.AuthMode == "codex") && m.Visibility != "list") {
				continue
			}
			seen[id] = true
			if m.Name == "" {
				m.Name = id
			}
			if m.Context == 0 {
				m.Context = m.ContextLength
			}
			if a.Provider == "claude" {
				if m.MaxInput > 0 {
					m.Context = m.MaxInput
				}
				if m.MaxTokens > 0 {
					m.MaxOutput = m.MaxTokens
				}
			}
			result = append(result, Model{ID: id, Name: m.Name, Provider: provider(a.Provider), Context: m.Context, MaxOutput: m.MaxOutput, Inputs: m.Inputs})
		}
	}
	ttl := time.Minute
	if err != nil {
		ttl = 10 * time.Second
	}
	s.catalogMu.Lock()
	s.catalogs[a.ID] = catalogCache{Models: result, Expires: time.Now().Add(ttl), Err: err}
	s.catalogMu.Unlock()
	return result, err
}
func (s *server) rawModels(ctx context.Context) ([]Model, error) {
	result := []Model{}
	seen := map[string]bool{}
	var failures []error
	for _, a := range s.store.snapshot().Accounts {
		if a.Disabled || !routableAuth(a) {
			continue
		}
		models, err := s.accountModels(ctx, a)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, m := range models {
			key := modelKey(m.Provider, m.ID)
			if !seen[key] {
				result = append(result, m)
				seen[key] = true
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, errors.Join(failures...)
}
func (s *server) models(ctx context.Context) ([]Model, error) {
	models, err := s.rawModels(ctx)
	p := s.store.snapshot().Policy
	result := []Model{}
	for _, m := range models {
		channel := policyChannel(m.Provider)
		blocked, _ := excludedModel(p, channel, m.ID)
		if blocked {
			continue
		}
		if override := p.Context[channel][m.ID]; override > 0 {
			m.Context = override
		}
		for _, a := range p.Aliases[channel] {
			if a.Name == m.ID {
				m.ID = a.Alias
				break
			}
		}
		result = append(result, m)
	}
	return result, err
}
