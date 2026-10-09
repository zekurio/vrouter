package gateway

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCredentialFixture(t *testing.T, dir, name string, body any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportNativeCredentials(t *testing.T) {
	dir := t.TempDir()
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	claims, _ := json.Marshal(map[string]any{
		"email": "test@example.com", "exp": expires.Unix(), "sub": "unverified-user",
		importOpenAIAuthClaim: map[string]string{"chatgpt_account_id": "workspace", "chatgpt_plan_type": "plus"},
	})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	codex := writeCredentialFixture(t, dir, "codex.json", map[string]any{
		"tokens": map[string]string{"access_token": token, "refresh_token": "codex-refresh", "id_token": token},
	})
	claude := writeCredentialFixture(t, dir, "claude.json", map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken": "claude-access", "refreshToken": "claude-refresh",
			"expiresAt": expires.UnixMilli(), "subscriptionType": "max", "scopes": []string{"user:inference"},
		},
	})
	stateDir := filepath.Join(dir, "state")
	if added, skipped, err := ImportCredentials(stateDir, []string{codex, claude}); err != nil || added != 2 || skipped != 0 {
		t.Fatalf("import: added=%d skipped=%d err=%v", added, skipped, err)
	}
	store, err := openStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	accounts := store.snapshot().Accounts
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts", len(accounts))
	}
	for _, a := range accounts {
		if !a.ExpiresAt.Equal(expires) || a.Subject != "" {
			t.Fatal("import changed expiry or trusted an unverified subject")
		}
		switch a.Provider {
		case "codex":
			if a.ClientID != codexNativeClientID || a.AccountID != "workspace" || a.Plan != "plus" || a.Email != "test@example.com" || a.AuthMode != "codex" {
				t.Fatal("Codex metadata was not imported")
			}
		case "claude":
			if a.ClientID != claudeClientID || a.Plan != "max" || a.AuthMode != "oauth" || len(a.Scopes) != 1 {
				t.Fatal("Claude metadata was not imported")
			}
		default:
			t.Fatal("unexpected provider")
		}
	}
	if err := store.update(func(d *diskState) error {
		d.Accounts[0].AccessToken = "refreshed-access"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	if added, skipped, err := ImportCredentials(stateDir, []string{codex, claude}); err != nil || added != 0 || skipped != 2 {
		t.Fatalf("repeat import: added=%d skipped=%d err=%v", added, skipped, err)
	}
	store, err = openStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if store.snapshot().Accounts[0].AccessToken != "refreshed-access" {
		t.Fatal("repeat import replaced refreshed credentials")
	}
}

func TestImportRejectsUnsupportedFilesBeforeWriting(t *testing.T) {
	for name, body := range map[string]any{
		"flat": map[string]string{"type": "codex", "access_token": "access", "refresh_token": "refresh"},
		"mixed": map[string]any{
			"tokens":        map[string]string{"access_token": "access", "refresh_token": "refresh"},
			"claudeAiOauth": map[string]string{"accessToken": "access", "refreshToken": "refresh"},
		},
		"missing-refresh": map[string]any{"tokens": map[string]string{"access_token": "access"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			valid := writeCredentialFixture(t, dir, "valid.json", map[string]any{
				"claudeAiOauth": map[string]string{"accessToken": "access", "refreshToken": "refresh"},
			})
			invalid := writeCredentialFixture(t, dir, "invalid.json", body)
			stateDir := filepath.Join(dir, "state")
			if _, _, err := ImportCredentials(stateDir, []string{valid, invalid}); err == nil {
				t.Fatal("unsupported credentials were accepted")
			}
			if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
				t.Fatal("failed import touched the state directory")
			}
		})
	}
}
