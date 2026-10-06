package gateway

import (
	"strings"
	"testing"
)

func TestLoadIdentityConfigFromEnvEmpty(t *testing.T) {
	t.Setenv(publicURLEnv, "")
	t.Setenv(oauthProvidersEnv, "")
	cfg, err := LoadIdentityConfigFromEnv()
	if err != nil {
		t.Fatalf("empty environment rejected: %v", err)
	}
	if cfg.PublicURL != "" || len(cfg.Providers) != 0 {
		t.Fatalf("empty environment produced %+v", cfg)
	}
	if err := validateIdentityConfig(cfg); err != nil {
		t.Fatalf("empty config is invalid: %v", err)
	}
}

func TestLoadIdentityConfigFromEnvResolvesProvider(t *testing.T) {
	t.Setenv(publicURLEnv, "https://vrouter.example/")
	t.Setenv("VROUTER_TEST_SECRET", "correct horse battery staple")
	t.Setenv(oauthProvidersEnv, `[{
		"id":"idp",
		"name":"Example IdP",
		"issuer":"https://idp.example/realms/main/",
		"clientId":"vrouter",
		"clientSecretEnv":"VROUTER_TEST_SECRET",
		"roleMappings":[
			{"claim":"groups","value":"vrouter-operators","role":"user"},
			{"claim":"realm_access.roles","value":"vrouter-admins","role":"admin"}
		]
	}]`)
	cfg, err := LoadIdentityConfigFromEnv()
	if err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	if cfg.PublicURL != "https://vrouter.example" {
		t.Fatalf("public URL not normalized: %q", cfg.PublicURL)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("expected one provider, got %d", len(cfg.Providers))
	}
	provider := cfg.Providers[0]
	if provider.ID != "idp" || provider.Name != "Example IdP" || provider.ClientID != "vrouter" {
		t.Fatalf("provider fields not preserved: %+v", provider)
	}
	if provider.Issuer != "https://idp.example/realms/main/" {
		t.Fatalf("issuer must be preserved exactly, including a trailing slash: %q", provider.Issuer)
	}
	if provider.clientSecret != "correct horse battery staple" {
		t.Fatal("client secret was not resolved from the named environment variable")
	}
	if len(provider.Scopes) != 3 || provider.Scopes[0] != "openid" || provider.Scopes[1] != "profile" || provider.Scopes[2] != "email" {
		t.Fatalf("default scopes wrong: %v", provider.Scopes)
	}
	if len(provider.RoleMappings) != 2 || provider.RoleMappings[1].Role != "admin" || provider.RoleMappings[1].Claim != "realm_access.roles" {
		t.Fatalf("role mappings wrong: %+v", provider.RoleMappings)
	}
	if err := validateIdentityConfig(cfg); err != nil {
		t.Fatalf("loaded config does not validate: %v", err)
	}
}

func TestLoadIdentityConfigFromEnvNormalizesScopesAndRoles(t *testing.T) {
	t.Setenv(publicURLEnv, "http://127.0.0.1:8080")
	t.Setenv(oauthProvidersEnv, `[{
		"id":"idp","name":"IdP","issuer":"https://idp.example","clientId":"client",
		"scopes":["openid"," openid ","profile"],
		"roleMappings":[{"claim":" groups ","value":" ops ","role":"ADMIN"}]
	}]`)
	cfg, err := LoadIdentityConfigFromEnv()
	if err != nil {
		t.Fatalf("loopback HTTP config rejected: %v", err)
	}
	provider := cfg.Providers[0]
	if len(provider.Scopes) != 2 || provider.Scopes[0] != "openid" || provider.Scopes[1] != "profile" {
		t.Fatalf("scopes not normalized: %v", provider.Scopes)
	}
	if provider.RoleMappings[0].Role != "admin" || provider.RoleMappings[0].Claim != "groups" || provider.RoleMappings[0].Value != "ops" {
		t.Fatalf("mappings not normalized: %+v", provider.RoleMappings)
	}
	if provider.ClientSecretEnv != "" || provider.clientSecret != "" {
		t.Fatal("public client grew a secret")
	}
}

func TestLoadIdentityConfigFromEnvPublicClientWithoutSecret(t *testing.T) {
	t.Setenv(publicURLEnv, "https://vrouter.example")
	t.Setenv(oauthProvidersEnv, `[{"id":"idp","name":"IdP","issuer":"https://idp.example","clientId":"client","roleMappings":[{"claim":"role","value":"user","role":"user"}]}]`)
	cfg, err := LoadIdentityConfigFromEnv()
	if err != nil {
		t.Fatalf("public client rejected: %v", err)
	}
	if cfg.Providers[0].clientSecret != "" {
		t.Fatal("public client has a secret")
	}
}

func TestLoadIdentityConfigFromEnvRejectsInvalid(t *testing.T) {
	base := `"issuer":"https://idp.example","clientId":"client","roleMappings":[{"claim":"role","value":"user","role":"user"}]`
	cases := []struct {
		name         string
		publicURL    string
		providers    string
		secretEnv    string
		secretValue  string
		wantContains []string
	}{
		{name: "malformed json", providers: `[{"id":`, wantContains: []string{oauthProvidersEnv}},
		{name: "null providers", providers: `null`, wantContains: []string{"JSON array"}},
		{name: "unknown field", providers: `[{"id":"idp","name":"IdP",` + base + `,"surprise":true}]`, wantContains: []string{"unknown field"}},
		{name: "trailing data", providers: `[] []`, wantContains: []string{"single JSON array"}},
		{name: "providers without public url", providers: `[{"id":"idp","name":"IdP",` + base + `}]`, wantContains: []string{publicURLEnv}},
		{name: "public url not absolute", publicURL: "vrouter.example", providers: `[]`, wantContains: []string{publicURLEnv}},
		{name: "public url insecure remote", publicURL: "http://vrouter.example", providers: `[]`, wantContains: []string{"HTTPS"}},
		{name: "public url with path", publicURL: "https://vrouter.example/app", providers: `[]`, wantContains: []string{"origin"}},
		{name: "public url with query", publicURL: "https://vrouter.example?x=1", providers: `[]`, wantContains: []string{"query"}},
		{name: "missing provider id", publicURL: "https://vrouter.example", providers: `[{"name":"IdP",` + base + `}]`, wantContains: []string{"id is required"}},
		{name: "provider id with slash", publicURL: "https://vrouter.example", providers: `[{"id":"bad/id","name":"IdP",` + base + `}]`, wantContains: []string{"id must be"}},
		{name: "duplicate provider id", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"One",` + base + `},{"id":"idp","name":"Two",` + base + `}]`, wantContains: []string{"duplicate"}},
		{name: "missing provider name", publicURL: "https://vrouter.example", providers: `[{"id":"idp",` + base + `}]`, wantContains: []string{"name is required"}},
		{name: "missing issuer", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","clientId":"client","roleMappings":[{"claim":"role","value":"user","role":"user"}]}]`, wantContains: []string{"issuer is required"}},
		{name: "insecure remote issuer", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","issuer":"http://idp.example","clientId":"client","roleMappings":[{"claim":"role","value":"user","role":"user"}]}]`, wantContains: []string{"HTTPS"}},
		{name: "issuer with query", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","issuer":"https://idp.example?x=1","clientId":"client","roleMappings":[{"claim":"role","value":"user","role":"user"}]}]`, wantContains: []string{"query"}},
		{name: "missing client id", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","issuer":"https://idp.example","roleMappings":[{"claim":"role","value":"user","role":"user"}]}]`, wantContains: []string{"clientId is required"}},
		{name: "empty secret", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","issuer":"https://idp.example","clientId":"client","clientSecretEnv":"VROUTER_MISSING_SECRET","roleMappings":[{"claim":"role","value":"user","role":"user"}]}]`, wantContains: []string{"VROUTER_MISSING_SECRET"}},
		{name: "invalid secret env name", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","issuer":"https://idp.example","clientId":"client","clientSecretEnv":"BAD NAME","roleMappings":[{"claim":"role","value":"user","role":"user"}]}]`, secretEnv: "BAD NAME", secretValue: "x", wantContains: []string{"not a valid environment variable name"}},
		{name: "scopes without openid", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","issuer":"https://idp.example","clientId":"client","scopes":["profile"],"roleMappings":[{"claim":"role","value":"user","role":"user"}]}]`, wantContains: []string{"openid"}},
		{name: "no role mappings", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","issuer":"https://idp.example","clientId":"client","roleMappings":[]}]`, wantContains: []string{"role mapping"}},
		{name: "invalid role", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","issuer":"https://idp.example","clientId":"client","roleMappings":[{"claim":"role","value":"user","role":"owner"}]}]`, wantContains: []string{`"admin" or "user"`}},
		{name: "empty claim", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","issuer":"https://idp.example","clientId":"client","roleMappings":[{"claim":"a..b","value":"user","role":"user"}]}]`, wantContains: []string{"claim"}},
		{name: "empty mapping value", publicURL: "https://vrouter.example", providers: `[{"id":"idp","name":"IdP","issuer":"https://idp.example","clientId":"client","roleMappings":[{"claim":"role","value":"  ","role":"user"}]}]`, wantContains: []string{"no value"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(publicURLEnv, tc.publicURL)
			t.Setenv(oauthProvidersEnv, tc.providers)
			t.Setenv("VROUTER_MISSING_SECRET", "")
			if tc.secretEnv != "" {
				t.Setenv(tc.secretEnv, tc.secretValue)
			}
			_, err := LoadIdentityConfigFromEnv()
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestLoadIdentityConfigFromEnvEmptyProviderArray(t *testing.T) {
	t.Setenv(publicURLEnv, "")
	t.Setenv(oauthProvidersEnv, "[]")
	cfg, err := LoadIdentityConfigFromEnv()
	if err != nil {
		t.Fatalf("empty provider array rejected: %v", err)
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("expected no providers, got %d", len(cfg.Providers))
	}
}

func TestLoadIdentityConfigFromEnvRejectsTooManyProviders(t *testing.T) {
	t.Setenv(publicURLEnv, "https://vrouter.example")
	var builder strings.Builder
	builder.WriteString("[")
	for i := 0; i < maxOIDCProviders+1; i++ {
		if i > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(`{"id":"idp`)
		builder.WriteString(strings.Repeat("x", i%5))
		builder.WriteString(`","name":"IdP","issuer":"https://idp.example","clientId":"client","roleMappings":[{"claim":"role","value":"user","role":"user"}]}`)
	}
	builder.WriteString("]")
	t.Setenv(oauthProvidersEnv, builder.String())
	if _, err := LoadIdentityConfigFromEnv(); err == nil {
		t.Fatal("too many providers accepted")
	}
}

func TestNormalizeOIDCProviderIssuerExact(t *testing.T) {
	// Only surrounding whitespace may be trimmed: the issuer is the exact
	// identity string an ID token must carry, so a trailing slash stays.
	cases := map[string]string{
		"https://idp.example/realms/main/":     "https://idp.example/realms/main/",
		"  https://idp.example/realms/main/  ": "https://idp.example/realms/main/",
		"https://idp.example":                  "https://idp.example",
		"\thttps://idp.example/\n":             "https://idp.example/",
	}
	for raw, want := range cases {
		provider := normalizeOIDCProvider(OIDCProviderConfig{Issuer: raw})
		if provider.Issuer != want {
			t.Fatalf("normalizeOIDCProvider(%q).Issuer = %q; want %q", raw, provider.Issuer, want)
		}
	}
}

func TestNormalizePublicURL(t *testing.T) {
	valid := map[string]string{
		"https://vrouter.example":      "https://vrouter.example",
		"https://vrouter.example/":     "https://vrouter.example",
		"http://localhost:8080":        "http://localhost:8080",
		"http://127.0.0.1:8080/":       "http://127.0.0.1:8080",
		"http://[::1]:9090":            "http://[::1]:9090",
		"HTTPS://VROUTER.EXAMPLE":      "https://VROUTER.EXAMPLE",
		"https://vrouter.example:8443": "https://vrouter.example:8443",
	}
	for raw, want := range valid {
		got, err := normalizePublicURL(raw)
		if err != nil || got != want {
			t.Fatalf("normalizePublicURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	invalid := []string{"", "vrouter.example", "ftp://vrouter.example", "https://", "http://192.0.2.10", "https://vrouter.example/path", "https://vrouter.example#fragment", "https://user@vrouter.example"}
	for _, raw := range invalid {
		if got, err := normalizePublicURL(raw); err == nil {
			t.Fatalf("normalizePublicURL(%q) accepted as %q", raw, got)
		}
	}
}
