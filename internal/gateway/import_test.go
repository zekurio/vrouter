package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeImportFixture writes a credential source file into a temporary
// directory. Fixtures only ever contain obvious test values.
func writeImportFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return path
}

// snapshotAccounts returns the stored accounts and releases the store lock so
// a later import call in the same test can open the directory again.
func snapshotAccounts(t *testing.T, dir string) []storedAccount {
	t.Helper()
	store, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore(%q): %v", dir, err)
	}
	accounts := store.snapshot().Accounts
	if err := store.close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return accounts
}

func importedAccounts(t *testing.T, dir string) map[string]storedAccount {
	t.Helper()
	accounts := map[string]storedAccount{}
	for _, account := range snapshotAccounts(t, dir) {
		accounts[account.AccessToken] = account
	}
	return accounts
}

func mustImport(t *testing.T, dir string, paths []string) (int, int) {
	t.Helper()
	imported, skipped, err := ImportCredentials(dir, paths)
	if err != nil {
		t.Fatalf("ImportCredentials: %v", err)
	}
	return imported, skipped
}

// importTestJWT builds a JWT with the given claims and an unverified dummy
// signature, matching the provider tokens the importer treats as metadata.
func importTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".not-a-real-signature"
}

func codexFixture(access, refresh string) string {
	return fmt.Sprintf(`{"type":"codex","label":"Local Codex","access_token":%q,"refresh_token":%q,"expired":"2031-02-03T04:05:06Z"}`, access, refresh)
}

func TestImportLegacyAndCLISchemas(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "vrouter")
	fixtures := t.TempDir()
	codexPath := writeImportFixture(t, fixtures, "codex-local.json", `{
  "type": "codex",
  "label": "Local Codex",
  "access_token": "codex-access-token",
  "refresh_token": "codex-refresh-token",
  "id_token": "codex-id-token",
  "account_id": "account-codex",
  "email": "codex@example.com",
  "plan_type": "pro",
  "expired": "2031-02-03T04:05:06Z"
}`)
	claudePath := writeImportFixture(t, fixtures, "claude-local.json", `{
  "type": "claude",
  "label": "Local Claude",
  "access_token": "claude-access-token",
  "refresh_token": "claude-refresh-token",
  "expired": "2031-03-04T05:06:07Z",
  "subscription_type": "max",
  "scopes": ["user:inference", "user:profile"]
}`)
	rawCodexPath := writeImportFixture(t, fixtures, "codex-auth.json", `{
  "OPENAI_API_KEY": null,
  "tokens": {
    "access_token": "raw-codex-access-token",
    "refresh_token": "raw-codex-refresh-token",
    "id_token": "raw-codex-id-token",
    "account_id": "account-raw-codex"
  },
  "last_refresh": "2026-01-02T03:04:05Z"
}`)
	rawClaudePath := writeImportFixture(t, fixtures, "credentials.json", `{
  "claudeAiOauth": {
    "accessToken": "raw-claude-access-token",
    "refreshToken": "raw-claude-refresh-token",
    "expiresAt": 1930000000000,
    "subscriptionType": "pro",
    "scopes": ["user:inference"]
  }
}`)
	paths := []string{codexPath, claudePath, rawCodexPath, rawClaudePath}
	imported, skipped := mustImport(t, storeDir, paths)
	if imported != 4 || skipped != 0 {
		t.Fatalf("first import = %d imported, %d skipped; want 4, 0", imported, skipped)
	}
	before := snapshotAccounts(t, storeDir)
	if len(before) != 4 {
		t.Fatalf("stored accounts = %d; want 4", len(before))
	}
	accounts := importedAccounts(t, storeDir)

	codex := accounts["codex-access-token"]
	codexExpiry := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	if codex.Provider != "codex" || codex.AuthMode != "codex" || codex.Label != "Local Codex" {
		t.Fatalf("codex identity = %+v", codex)
	}
	if codex.Email != "codex@example.com" || codex.Plan != "pro" || codex.AccountID != "account-codex" {
		t.Fatalf("codex metadata = %+v", codex)
	}
	if codex.RefreshToken != "codex-refresh-token" || codex.IDToken != "codex-id-token" || !codex.ExpiresAt.Equal(codexExpiry) {
		t.Fatalf("codex tokens = %+v", codex)
	}
	if codex.CreatedAt.IsZero() || codex.Disabled || len(codex.Scopes) != 0 {
		t.Fatalf("codex state = %+v", codex)
	}
	canonical, err := canonicalImportPath(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if codex.ID != credentialSourceID("codex", canonical) {
		t.Fatalf("codex ID is not the deterministic source ID: %q", codex.ID)
	}

	claude := accounts["claude-access-token"]
	claudeExpiry := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	if claude.Provider != "claude" || claude.AuthMode != "oauth" || claude.Label != "Local Claude" {
		t.Fatalf("claude identity = %+v", claude)
	}
	if claude.Plan != "max" || !claude.ExpiresAt.Equal(claudeExpiry) {
		t.Fatalf("claude metadata = %+v", claude)
	}
	if !reflect.DeepEqual(claude.Scopes, []string{"user:inference", "user:profile"}) {
		t.Fatalf("claude scopes = %v", claude.Scopes)
	}

	rawCodex := accounts["raw-codex-access-token"]
	if rawCodex.Provider != "codex" || rawCodex.AuthMode != "codex" || rawCodex.AccountID != "account-raw-codex" {
		t.Fatalf("raw codex = %+v", rawCodex)
	}
	if !rawCodex.ExpiresAt.IsZero() || rawCodex.Label != "Codex" {
		t.Fatalf("raw codex default = %+v", rawCodex)
	}

	rawClaude := accounts["raw-claude-access-token"]
	rawClaudeExpiry := time.UnixMilli(1930000000000).UTC()
	if rawClaude.Provider != "claude" || rawClaude.AuthMode != "oauth" || rawClaude.Plan != "pro" {
		t.Fatalf("raw claude = %+v", rawClaude)
	}
	if !rawClaude.ExpiresAt.Equal(rawClaudeExpiry) {
		t.Fatalf("raw claude expiry = %v; want %v", rawClaude.ExpiresAt, rawClaudeExpiry)
	}

	// Re-importing the same sources in a different order must not add or
	// rewrite anything, so tokens refreshed in the store stay intact.
	reversed := []string{rawClaudePath, rawCodexPath, claudePath, codexPath}
	imported, skipped = mustImport(t, storeDir, reversed)
	if imported != 0 || skipped != 4 {
		t.Fatalf("repeat import = %d imported, %d skipped; want 0, 4", imported, skipped)
	}
	after := snapshotAccounts(t, storeDir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("repeat import changed stored accounts:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func TestImportKeepsRefreshedTokens(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "vrouter")
	fixture := writeImportFixture(t, t.TempDir(), "codex.json", codexFixture("stale-access", "stale-refresh"))
	canonical, err := canonicalImportPath(fixture)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	updateTestStore(t, store, func(state *diskState) {
		state.Accounts = append(state.Accounts, storedAccount{
			ID:           credentialSourceID("codex", canonical),
			Provider:     "codex",
			Label:        "Refreshed",
			AccessToken:  "refreshed-access",
			RefreshToken: "refreshed-refresh",
			ExpiresAt:    storeTestTime.Add(2 * time.Hour),
			CreatedAt:    storeTestTime,
			AuthMode:     "codex",
		})
	})
	if err := store.close(); err != nil {
		t.Fatal(err)
	}

	imported, skipped := mustImport(t, storeDir, []string{fixture})
	if imported != 0 || skipped != 1 {
		t.Fatalf("import over a refreshed account = %d imported, %d skipped; want 0, 1", imported, skipped)
	}
	accounts := snapshotAccounts(t, storeDir)
	if len(accounts) != 1 {
		t.Fatalf("stored accounts = %d; want 1", len(accounts))
	}
	if accounts[0].AccessToken != "refreshed-access" || accounts[0].RefreshToken != "refreshed-refresh" || accounts[0].Label != "Refreshed" {
		t.Fatalf("refreshed tokens were overwritten: %+v", accounts[0])
	}
}

func TestImportSkipsDuplicateAccessTokens(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "vrouter")
	fixtures := t.TempDir()
	first := writeImportFixture(t, fixtures, "first.json", codexFixture("shared-access", "shared-refresh"))
	second := writeImportFixture(t, fixtures, "second.json", codexFixture("shared-access", "shared-refresh"))

	if imported, skipped := mustImport(t, storeDir, []string{first}); imported != 1 || skipped != 0 {
		t.Fatalf("first import = %d/%d; want 1/0", imported, skipped)
	}
	if imported, skipped := mustImport(t, storeDir, []string{first, second}); imported != 0 || skipped != 2 {
		t.Fatalf("duplicate import = %d/%d; want 0/2", imported, skipped)
	}

	// The same path listed twice inside one call is the same source, not two
	// accounts.
	secondDir := filepath.Join(t.TempDir(), "vrouter")
	if imported, skipped := mustImport(t, secondDir, []string{first, first}); imported != 1 || skipped != 1 {
		t.Fatalf("repeated path import = %d/%d; want 1/1", imported, skipped)
	}
}

func TestImportRejectsBadFilesWithoutPartialImport(t *testing.T) {
	good := `{"type":"codex","label":"Good","access_token":"good-access","refresh_token":"good-refresh","expired":"2031-02-03T04:05:06Z"}`
	cases := []struct{ name, content string }{
		{"malformed JSON", `{"type":"codex","access_token":"import-secret-value","refresh_token":`},
		{"unsupported type", `{"type":"gemini","access_token":"import-secret-value","refresh_token":"import-secret-refresh"}`},
		{"missing tokens", `{"type":"codex","label":"No tokens"}`},
		{"unrecognized file", `{"hello":"world","access_token":"import-secret-value"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storeDir := filepath.Join(t.TempDir(), "vrouter")
			fixtures := t.TempDir()
			goodPath := writeImportFixture(t, fixtures, "good.json", good)
			badPath := writeImportFixture(t, fixtures, "bad.json", tc.content)

			imported, skipped, err := ImportCredentials(storeDir, []string{goodPath, badPath})
			if err == nil {
				t.Fatal("unsupported input was accepted")
			}
			if imported != 0 || skipped != 0 {
				t.Fatalf("failed import reported %d/%d", imported, skipped)
			}
			if strings.Contains(err.Error(), "import-secret") {
				t.Fatalf("error leaks credential material: %v", err)
			}
			if accounts := snapshotAccounts(t, storeDir); len(accounts) != 0 {
				t.Fatalf("partial import stored accounts: %+v", accounts)
			}
		})
	}

	t.Run("oversized", func(t *testing.T) {
		storeDir := filepath.Join(t.TempDir(), "vrouter")
		fixtures := t.TempDir()
		goodPath := writeImportFixture(t, fixtures, "good.json", good)
		oversized := `{"type":"codex","access_token":"import-secret-value","refresh_token":"import-secret-refresh","padding":"` + strings.Repeat("x", importMaxBytes) + `"}`
		badPath := writeImportFixture(t, fixtures, "huge.json", oversized)

		imported, skipped, err := ImportCredentials(storeDir, []string{goodPath, badPath})
		if err == nil {
			t.Fatal("oversized input was accepted")
		}
		if imported != 0 || skipped != 0 {
			t.Fatalf("failed import reported %d/%d", imported, skipped)
		}
		if strings.Contains(err.Error(), "import-secret") {
			t.Fatalf("error leaks credential material: %v", err)
		}
		if accounts := snapshotAccounts(t, storeDir); len(accounts) != 0 {
			t.Fatalf("partial import stored accounts: %+v", accounts)
		}
	})
}

func TestImportUsesJWTClaimsAsMetadata(t *testing.T) {
	expires := time.Date(2032, 5, 6, 7, 8, 9, 0, time.UTC)
	access := importTestJWT(t, map[string]any{
		"exp":                            expires.Unix(),
		"https://api.openai.com/auth":    map[string]any{"chatgpt_plan_type": "pro"},
		"https://api.openai.com/profile": map[string]any{"email": "profile@example.com"},
	})
	identity := importTestJWT(t, map[string]any{
		"email":                       "id@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "account-jwt"},
	})
	storeDir := filepath.Join(t.TempDir(), "vrouter")
	fixture := writeImportFixture(t, t.TempDir(), "codex-jwt.json", fmt.Sprintf(
		`{"type":"codex","access_token":%q,"refresh_token":"refresh-jwt","id_token":%q}`, access, identity))
	mustImport(t, storeDir, []string{fixture})

	accounts := importedAccounts(t, storeDir)
	account := accounts[access]
	if account.Email != "id@example.com" || account.Plan != "pro" || account.AccountID != "account-jwt" {
		t.Fatalf("JWT metadata not used: %+v", account)
	}
	if !account.ExpiresAt.Equal(expires) {
		t.Fatalf("JWT expiry = %v; want %v", account.ExpiresAt, expires)
	}

	// An explicit file timestamp is authoritative over the JWT claim.
	sourceExpiry := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	otherDir := filepath.Join(t.TempDir(), "vrouter")
	other := writeImportFixture(t, t.TempDir(), "codex-source-expiry.json", fmt.Sprintf(
		`{"type":"codex","access_token":%q,"refresh_token":"refresh-2","expired":"2030-01-01T00:00:00Z"}`, access))
	mustImport(t, otherDir, []string{other})
	if got := importedAccounts(t, otherDir)[access].ExpiresAt; !got.Equal(sourceExpiry) {
		t.Fatalf("source expiry = %v; want %v", got, sourceExpiry)
	}
}

func TestImportPreservesDisabledFlag(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "vrouter")
	fixture := writeImportFixture(t, t.TempDir(), "codex-disabled.json",
		`{"type":"codex","access_token":"disabled-access","refresh_token":"disabled-refresh","disabled":true}`)
	mustImport(t, storeDir, []string{fixture})
	account := importedAccounts(t, storeDir)["disabled-access"]
	if !account.Disabled {
		t.Fatalf("disabled flag not preserved: %+v", account)
	}
}

func TestImportRequiresPaths(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "vrouter")
	if _, _, err := ImportCredentials(storeDir, nil); err == nil {
		t.Fatal("empty path list was accepted")
	}
	if _, err := os.Stat(storeDir); !os.IsNotExist(err) {
		t.Fatalf("empty import touched the data directory: %v", err)
	}
}
