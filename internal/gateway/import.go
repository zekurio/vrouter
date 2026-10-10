package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// importMaxBytes bounds one credential file. OAuth documents are a few
	// kilobytes, so anything larger is refused before decoding.
	importMaxBytes = 1 << 20

	importOpenAIAuthClaim    = "https://api.openai.com/auth"
	importOpenAIProfileClaim = "https://api.openai.com/profile"
)

// ImportCredentials copies provider credentials from JSON files into the
// account store in dataDir and returns how many accounts it imported and
// skipped. It accepts the Codex CLI's auth.json and the Claude CLI's
// .credentials.json.
//
// All files are parsed before the store is touched, so a malformed or
// unsupported file leaves every existing account unchanged. A credential
// whose source ID or access token is already stored is skipped instead of
// replaced: a live account may have refreshed its tokens after the source
// file was written, and the store's copy is newer.
func ImportCredentials(dataDir string, paths []string) (int, int, error) {
	if len(paths) == 0 {
		return 0, 0, errors.New("no credential files to import")
	}
	accounts := make([]storedAccount, 0, len(paths))
	for _, path := range paths {
		account, err := importCredential(path)
		if err != nil {
			return 0, 0, fmt.Errorf("import %s: %w", path, err)
		}
		accounts = append(accounts, account)
	}
	store, err := openStore(dataDir)
	if err != nil {
		return 0, 0, fmt.Errorf("open account store: %w", err)
	}
	defer store.close()
	imported, skipped := 0, 0
	if err := store.update(func(state *diskState) error {
		for _, account := range accounts {
			if credentialStored(state.Accounts, account) {
				skipped++
				continue
			}
			state.Accounts = append(state.Accounts, account)
			imported++
		}
		return nil
	}); err != nil {
		return 0, 0, fmt.Errorf("save imported accounts: %w", err)
	}
	return imported, skipped, nil
}

// credentialStored reports whether an equivalent account already exists. The
// deterministic source ID is the primary key; a matching access token catches
// the same account imported from a second file.
func credentialStored(accounts []storedAccount, candidate storedAccount) bool {
	for _, account := range accounts {
		if account.ID == candidate.ID {
			return true
		}
		if candidate.AccessToken != "" && account.Provider == candidate.Provider && account.AccessToken == candidate.AccessToken {
			return true
		}
	}
	return false
}

func importCredential(path string) (storedAccount, error) {
	data, err := readImportFile(path)
	if err != nil {
		return storedAccount{}, err
	}
	var file importFile
	if err := json.Unmarshal(data, &file); err != nil {
		return storedAccount{}, errors.New("credential file is not valid JSON")
	}
	provider, err := file.provider()
	if err != nil {
		return storedAccount{}, err
	}
	canonical, err := canonicalImportPath(path)
	if err != nil {
		return storedAccount{}, errors.New("could not resolve the credential file path")
	}
	return file.account(provider, canonical)
}

// readImportFile reads at most 1 MiB plus one byte so an oversized file is
// rejected before it can be decoded.
func readImportFile(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // the operator names the credential files to import
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, importMaxBytes+1))
	if err != nil {
		return nil, errors.New("could not read credential file")
	}
	if len(data) > importMaxBytes {
		return nil, errors.New("credential file is larger than 1 MiB")
	}
	return data, nil
}

// canonicalImportPath resolves a file to one stable name so the same source
// always maps to the same account ID, even when reached through a symlink or a
// relative path.
func canonicalImportPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved, nil
	}
	return filepath.Clean(absolute), nil
}

// credentialSourceID is the deterministic ID of an imported credential:
// sha256 over the provider and canonical source path. Re-importing the same
// source therefore targets the same record instead of adding a duplicate.
func credentialSourceID(provider, canonicalPath string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + canonicalPath))
	return hex.EncodeToString(sum[:])
}

// importTokens is the nested "tokens" object of the Codex CLI's auth.json.
type importTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
}

// importClaudeOAuth is the native Claude CLI credential object.
type importClaudeOAuth struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"`
	SubscriptionType string   `json:"subscriptionType"`
	Scopes           []string `json:"scopes"`
}

// Native files must contain exactly one provider credential object.
type importFile struct {
	Tokens        *importTokens      `json:"tokens"`
	ClaudeAiOauth *importClaudeOAuth `json:"claudeAiOauth"`
}

func (f *importFile) provider() (string, error) {
	switch {
	case f.Tokens != nil && f.ClaudeAiOauth != nil:
		return "", errors.New("credential file contains more than one provider")
	case f.Tokens != nil:
		return "codex", nil
	case f.ClaudeAiOauth != nil:
		return "claude", nil
	default:
		return "", errors.New("expected a native Codex or Claude CLI credential file")
	}
}

func (f *importFile) account(provider, canonicalPath string) (storedAccount, error) {
	account := storedAccount{
		ID:        credentialSourceID(provider, canonicalPath),
		Provider:  provider,
		Label:     providerLabel(provider),
		CreatedAt: time.Now().UTC(),
	}
	if provider == "codex" {
		account.AccessToken = strings.TrimSpace(f.Tokens.AccessToken)
		account.RefreshToken = strings.TrimSpace(f.Tokens.RefreshToken)
		account.IDToken = strings.TrimSpace(f.Tokens.IDToken)
		account.AuthMode = "codex"
		account.ClientID = codexNativeClientID
		account.AccountID = strings.TrimSpace(f.Tokens.AccountID)
	} else {
		account.AccessToken = strings.TrimSpace(f.ClaudeAiOauth.AccessToken)
		account.RefreshToken = strings.TrimSpace(f.ClaudeAiOauth.RefreshToken)
		account.AuthMode = "oauth"
		account.ClientID = claudeClientID
		account.Plan = strings.TrimSpace(f.ClaudeAiOauth.SubscriptionType)
		account.Scopes = f.ClaudeAiOauth.Scopes
		if f.ClaudeAiOauth.ExpiresAt > 0 {
			account.ExpiresAt = time.UnixMilli(f.ClaudeAiOauth.ExpiresAt).UTC()
		}
	}
	if account.AccessToken == "" || account.RefreshToken == "" {
		return storedAccount{}, errors.New("missing OAuth credentials")
	}
	accessClaims := tokenClaims(account.AccessToken)
	idClaims := tokenClaims(account.IDToken)
	account.Email = firstNonEmpty(
		claimString(idClaims, "email"),
		claimNested(idClaims, importOpenAIProfileClaim, "email"),
		claimString(accessClaims, "email"),
		claimNested(accessClaims, importOpenAIProfileClaim, "email"),
	)
	if provider == "codex" {
		account.AccountID = firstNonEmpty(
			account.AccountID,
			claimNested(idClaims, importOpenAIAuthClaim, "chatgpt_account_id"),
			claimNested(accessClaims, importOpenAIAuthClaim, "chatgpt_account_id"),
		)
		account.Plan = firstNonEmpty(
			claimNested(accessClaims, importOpenAIAuthClaim, "chatgpt_plan_type"),
			claimNested(idClaims, importOpenAIAuthClaim, "chatgpt_plan_type"),
		)
	}
	if account.ExpiresAt.IsZero() {
		account.ExpiresAt = claimTime(accessClaims, "exp")
	}
	if account.ExpiresAt.IsZero() {
		account.ExpiresAt = claimTime(idClaims, "exp")
	}
	return account, nil
}

// tokenClaims decodes a JWT payload without verifying its signature. The
// result supplies display metadata for a locally imported credential only; it
// is never used to authenticate the credential or its owner.
func tokenClaims(token string) map[string]any {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 64<<10 {
		return nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

func claimString(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}
	value, _ := claims[key].(string)
	return strings.TrimSpace(value)
}

func claimNested(claims map[string]any, namespace, key string) string {
	if claims == nil {
		return ""
	}
	nested, ok := claims[namespace].(map[string]any)
	if !ok {
		return ""
	}
	value, _ := nested[key].(string)
	return strings.TrimSpace(value)
}

func claimTime(claims map[string]any, key string) time.Time {
	if claims == nil {
		return time.Time{}
	}
	value, ok := claims[key].(float64)
	if !ok || value <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(value), 0).UTC()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
