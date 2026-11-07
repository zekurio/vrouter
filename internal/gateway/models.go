package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// The Codex catalog gates models by client version. Keep this in sync with
// the native client protocol supported by the router.
const codexCatalogClientVersion = "0.160.1"

type catalogCache struct {
	Models  []Model
	Expires time.Time
	Err     error
	// Good is the last catalog the provider sent. It serves for a while when
	// a later load fails for reasons other than the account's own access.
	Good   []Model
	GoodAt time.Time
}

const (
	catalogTTL   = time.Minute
	catalogRetry = 10 * time.Second
	// catalogStaleMax bounds how long a good catalog outlives a provider
	// outage.
	catalogStaleMax = 6 * time.Hour
	// catalogLoadTimeout bounds a background load.
	catalogLoadTimeout = 30 * time.Second
)

// stale reports whether the last good catalog may still serve.
func (c catalogCache) stale(now time.Time) bool {
	return c.Good != nil && now.Sub(c.GoodAt) < catalogStaleMax
}

// result answers from a current entry. When the last load failed for a
// reason other than the account's own access, the last good catalog answers.
func (c catalogCache) result(now time.Time) ([]Model, error) {
	if c.Err != nil && c.stale(now) {
		return c.Good, nil
	}
	return c.Models, c.Err
}

type providerModel struct {
	ReasoningLevels []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
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

// accountModels returns an account's catalog. One load per account runs at a
// time. Once the cached catalog expires, the previous one keeps answering
// while a background load replaces it, so requests rarely wait on a provider
// catalog and a short outage does not hide an account's models.
func (s *server) accountModels(ctx context.Context, a storedAccount) ([]Model, error) {
	if !routableAuth(a) {
		return nil, errAccountAuth
	}
	for {
		now := time.Now()
		s.catalogMu.Lock()
		cached, ok := s.catalogs[a.ID]
		if ok && now.Before(cached.Expires) {
			s.catalogMu.Unlock()
			return cached.result(now)
		}
		pending := s.catalogPending[a.ID]
		if pending == nil {
			pending = make(chan struct{})
			s.catalogPending[a.ID] = pending
			s.catalogMu.Unlock()
			if !cached.stale(now) {
				return s.loadCatalog(ctx, a, pending)
			}
			started := s.runBackground(func(ctx context.Context) {
				ctx, cancel := context.WithTimeout(ctx, catalogLoadTimeout)
				defer cancel()
				_, _ = s.loadCatalog(ctx, a, pending)
			})
			if !started {
				s.catalogMu.Lock()
				if s.catalogPending[a.ID] == pending {
					delete(s.catalogPending, a.ID)
				}
				s.catalogMu.Unlock()
				close(pending)
			}
			return cached.Good, nil
		}
		s.catalogMu.Unlock()
		if cached.stale(now) {
			return cached.Good, nil
		}
		select {
		case <-pending:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// loadCatalog reads an account's catalog from its provider and caches the
// outcome, unless the account changed while it loaded. It closes pending when
// done.
func (s *server) loadCatalog(ctx context.Context, a storedAccount, pending chan struct{}) ([]Model, error) {
	defer func() {
		s.catalogMu.Lock()
		if s.catalogPending[a.ID] == pending {
			delete(s.catalogPending, a.ID)
		}
		s.catalogMu.Unlock()
		close(pending)
	}()
	models, err := s.fetchCatalog(ctx, a)
	if err != nil && ctx.Err() != nil {
		// A caller that gave up says nothing about the provider; do not
		// cache its failure for everyone else.
		return models, err
	}
	now := time.Now()
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	entry := s.catalogs[a.ID]
	entry.Models, entry.Err, entry.Expires = models, err, now.Add(catalogTTL)
	switch {
	case err == nil:
		entry.Good, entry.GoodAt = models, now
	case accountRefused(err):
		entry.Good, entry.Expires = nil, now.Add(catalogRetry)
	default:
		entry.Expires = now.Add(catalogRetry)
		slog.Warn("provider catalog unavailable", "account", a.ID, "provider", a.Provider, "error", err, "serving_previous", entry.stale(now))
	}
	if s.catalogPending[a.ID] == pending {
		s.catalogs[a.ID] = entry
	}
	return entry.result(now)
}

// forgetCatalog drops an account's cached catalog after its connection
// changed. A load already running is not cached.
func (s *server) forgetCatalog(id string) {
	s.catalogMu.Lock()
	delete(s.catalogs, id)
	delete(s.catalogPending, id)
	s.catalogMu.Unlock()
}

func (s *server) fetchCatalog(ctx context.Context, a storedAccount) ([]Model, error) {
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
		result = catalogModels(a, rows)
	}
	return result, err
}

// catalogModels converts an account's provider catalog, skipping unnamed,
// duplicate, and (for Codex) unlisted models.
func catalogModels(a storedAccount, rows []providerModel) []Model {
	result := []Model{}
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
		result = append(result, m.gatewayModel(a, id))
	}
	return result
}

func (m providerModel) gatewayModel(a storedAccount, id string) Model {
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
	reasoning := make([]string, 0, len(m.ReasoningLevels))
	for _, level := range m.ReasoningLevels {
		reasoning = append(reasoning, level.Effort)
	}
	return Model{Reasoning: reasoning, ReasoningSupported: len(reasoning) > 0, ID: id, Name: m.Name, Provider: provider(a.Provider), Context: m.Context, MaxOutput: m.MaxOutput, Inputs: m.Inputs}
}

// rawModels lists the native models of the accounts a user's requests can
// use, before model settings apply.
func (s *server) rawModels(ctx context.Context, userID string) ([]Model, error) {
	var pool []storedAccount
	for _, a := range s.store.snapshot().Accounts {
		if !a.Disabled && routableAuth(a) && accountVisible(a, userID) {
			pool = append(pool, a)
		}
	}
	// Catalogs load side by side so one slow provider does not hold up the rest.
	catalogs := make([][]Model, len(pool))
	failed := make([]error, len(pool))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i, a := range pool {
		wg.Add(1)
		go func(i int, a storedAccount) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				failed[i] = ctx.Err()
				return
			}
			catalogs[i], failed[i] = s.accountModels(ctx, a)
		}(i, a)
	}
	wg.Wait()
	result := []Model{}
	seen := map[string]bool{}
	var failures []error
	for i := range pool {
		if failed[i] != nil {
			failures = append(failures, failed[i])
			continue
		}
		for _, m := range catalogs[i] {
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

// models lists the public models a user's requests can use.
func (s *server) models(ctx context.Context, userID string) ([]Model, error) {
	models, err := s.rawModels(ctx, userID)
	p := s.store.snapshot().Policy
	result := []Model{}
	for _, m := range models {
		channel := policyChannel(m.Provider)
		blocked, _ := excludedModel(p, channel, m.ID)
		if blocked {
			continue
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
