package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestCatalogTokenLimits(t *testing.T) {
	for _, tc := range []struct {
		name, provider, auth, body string
		context, output            int
	}{
		{"Claude native limits", "claude", "oauth", `{"data":[{"id":"claude-opus-5-5","max_input_tokens":1000000,"max_tokens":128000}]}`, 1000000, 128000},
		{"Claude native precedence", "claude", "oauth", `{"data":[{"id":"claude-test","max_input_tokens":200000,"context_window":100,"max_tokens":64000,"max_output_tokens":10}]}`, 200000, 64000},
		{"Claude missing limits", "claude", "oauth", `{"data":[{"id":"claude-test","max_input_tokens":null,"max_tokens":null}]}`, 0, 0},
		{"Codex limits", "codex", "codex", `{"models":[{"slug":"gpt-6.1-sol","visibility":"list","context_window":272000,"max_output_tokens":128000}]}`, 272000, 128000},
		{"Context length fallback", "codex", "codex", `{"models":[{"slug":"gpt-test","visibility":"list","context_length":128000}]}`, 128000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := oauthServer(t)
			a := nativeAccount("a")
			a.Provider = tc.provider
			a.AuthMode = tc.auth
			nativeSeed(t, s, a)
			nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
				if tc.auth == "codex" && r.URL.Query().Get("client_version") != "0.160.1" {
					t.Error("catalog request uses an outdated client version")
				}
				return oauthReply(200, json.RawMessage(tc.body)), nil
			}))
			models, err := s.rawModels(context.Background())
			if err != nil || len(models) != 1 {
				t.Fatalf("catalog: %v %+v", err, models)
			}
			if models[0].Context != tc.context || models[0].MaxOutput != tc.output {
				t.Fatalf("wrong token limits: %+v", models[0])
			}
		})
	}
}

func TestCodexCatalogVisibility(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("a"))
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		return oauthReply(200, map[string]any{"models": []map[string]string{
			{"slug": "gpt-6.1-sol", "visibility": "list"},
			{"slug": "gpt-6-astra", "visibility": "list"},
			{"slug": "gpt-6-sol", "visibility": "list"},
			{"slug": "gpt-6-luna", "visibility": "list"},
			{"slug": "internal", "visibility": "hide"},
		}}), nil
	}))
	models, err := s.rawModels(context.Background())
	if err != nil || len(models) != 4 {
		t.Fatalf("catalog: %v %+v", err, models)
	}
}

func TestContextOverrideLifecycle(t *testing.T) {
	s := oauthServer(t)
	a := nativeAccount("claude")
	a.Provider = "claude"
	a.AuthMode = "oauth"
	nativeSeed(t, s, a, nativeAccount("codex"))
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.anthropic.com" {
			return oauthReply(200, json.RawMessage(`{"data":[{"id":"claude-test","max_input_tokens":1000000,"max_tokens":128000}]}`)), nil
		}
		return oauthReply(200, json.RawMessage(`{"models":[{"slug":"gpt-6.1-sol","visibility":"list","context_window":272000}]}`)), nil
	}))
	read := func() modelSettings {
		t.Helper()
		w := nativeCall(s, "GET", "/api/model-settings", "admin", "")
		var settings modelSettings
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &settings) != nil {
			t.Fatalf("read: %d %s", w.Code, w.Body)
		}
		return settings
	}
	save := func(revision, changes string, status int) modelSettings {
		t.Helper()
		w := nativeCall(s, "PUT", "/api/model-settings", "admin", fmt.Sprintf(`{"revision":%q,"models":%s}`, revision, changes))
		if w.Code != status {
			t.Fatalf("save: %d %s", w.Code, w.Body)
		}
		var settings modelSettings
		_ = json.Unmarshal(w.Body.Bytes(), &settings)
		return settings
	}
	initial := read()
	legacy := save(initial.Revision, `[{"id":"claude-test","provider":"Claude","enabled":true}]`, 200)
	if legacy.Revision != read().Revision {
		t.Fatal("legacy update returned a revision different from persisted policy")
	}
	initial = read()
	saved := save(initial.Revision, `[{"id":"claude-test","provider":"Claude","enabled":true,"alias":"my-claude","context":500000},{"id":"gpt-6.1-sol","provider":"Codex","enabled":true,"context":100000}]`, 200)
	if saved.Revision == initial.Revision {
		t.Fatal("context changes did not change revision")
	}
	check := func(settings modelSettings, want, override int) {
		t.Helper()
		for _, m := range settings.Models {
			if m.ID == "claude-test" {
				if m.Context != want || m.ContextOverride != override || m.DefaultContext != 1000000 {
					t.Fatalf("wrong context: %+v", m)
				}
				return
			}
		}
		t.Fatal("missing Claude model")
	}
	check(saved, 500000, 500000)
	// Reload from the persisted store, not just a settings response or catalog cache.
	dir := s.store.dir
	if err := s.store.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.store = reopened
	check(read(), 500000, 500000)
	public := nativeCall(s, "GET", "/v1/models", "client", "")
	var catalog struct {
		Data []struct {
			ID      string `json:"id"`
			Context int    `json:"context_window"`
		} `json:"data"`
	}
	if public.Code != 200 || json.Unmarshal(public.Body.Bytes(), &catalog) != nil {
		t.Fatal(public.Body)
	}
	if len(catalog.Data) != 2 || catalog.Data[0].ID != "my-claude" || catalog.Data[0].Context != 500000 || catalog.Data[1].Context != 100000 {
		t.Fatalf("wrong public metadata: %+v", catalog)
	}
	save(initial.Revision, `[{"id":"claude-test","provider":"Claude","enabled":true,"context":1}]`, 409)
	// Old clients that omit context must preserve the override.
	saved = save(saved.Revision, `[{"id":"claude-test","provider":"Claude","enabled":true,"alias":"my-claude"}]`, 200)
	check(saved, 500000, 500000)
	for _, bad := range []string{"-1", "1.5", "2147483648", `"500K"`} {
		save(saved.Revision, fmt.Sprintf(`[{"id":"claude-test","provider":"Claude","enabled":true,"context":%s}]`, bad), 400)
		check(read(), 500000, 500000)
	}
	// A later invalid edit must not partially commit an earlier valid edit.
	save(saved.Revision, `[{"id":"claude-test","provider":"Claude","enabled":true,"context":250000},{"id":"gpt-6.1-sol","provider":"Codex","enabled":true,"context":-1}]`, 400)
	check(read(), 500000, 500000)
	reset := save(saved.Revision, `[{"id":"claude-test","provider":"Claude","enabled":true,"context":0}]`, 200)
	check(reset, 1000000, 0)
	if s.store.snapshot().Policy.Context["codex"]["gpt-6.1-sol"] != 100000 {
		t.Fatal("reset changed another provider's override")
	}
	raw, err := s.rawModels(context.Background())
	if err != nil || raw[0].Context != 1000000 {
		t.Fatalf("override mutated cached provider metadata: %v %+v", err, raw)
	}
	// Preserve overrides for a temporarily unadvertised model so they can be edited.
	if err := s.store.update(func(d *diskState) error { d.Policy.Context["claude"]["unavailable"] = 123456; return nil }); err != nil {
		t.Fatal(err)
	}
	settings := read()
	found := false
	for _, m := range settings.Models {
		if m.ID == "unavailable" {
			found = m.Context == 123456 && m.ContextOverride == 123456 && m.DefaultContext == 0
		}
	}
	if !found {
		t.Fatal("unadvertised override missing from settings")
	}
	public = nativeCall(s, "GET", "/v1/models", "client", "")
	if strings.Contains(public.Body.String(), "unavailable") {
		t.Fatal("override advertised an unavailable model")
	}
}
