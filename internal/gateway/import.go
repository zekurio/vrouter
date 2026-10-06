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
	"strconv"
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
// account store in dataDir. It understands the files the previous
// CLIProxyAPI-based scripts wrote, the Codex CLI's auth.json, and the Claude
// CLI's .credentials.json.
//
// All files are parsed before the store is touched, so a malformed or
// unsupported file leaves every existing account unchanged. A credential
// whose source ID or access token is already stored is skipped instead of
// replaced: a live account may have refreshed its tokens after the source
// file was written, and the store's copy is newer.
func ImportCredentials(dataDir string, paths []string) (imported, skipped int, err error) {
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
	file, err := os.Open(path)
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

func (t *importTokens) accessToken() string {
	if t == nil {
		return ""
	}
	return t.AccessToken
}

func (t *importTokens) refreshToken() string {
	if t == nil {
		return ""
	}
	return t.RefreshToken
}

func (t *importTokens) idToken() string {
	if t == nil {
		return ""
	}
	return t.IDToken
}

func (t *importTokens) accountID() string {
	if t == nil {
		return ""
	}
	return t.AccountID
}

// importClaudeOAuth is the nested "claudeAiOauth" object of the Claude CLI's
// .credentials.json.
type importClaudeOAuth struct {
	AccessToken      string     `json:"accessToken"`
	RefreshToken     string     `json:"refreshToken"`
	ExpiresAt        importTime `json:"expiresAt"`
	SubscriptionType string     `json:"subscriptionType"`
	Scopes           []string   `json:"scopes"`
}

func (c *importClaudeOAuth) accessToken() string {
	if c == nil {
		return ""
	}
	return c.AccessToken
}

func (c *importClaudeOAuth) refreshToken() string {
	if c == nil {
		return ""
	}
	return c.RefreshToken
}

func (c *importClaudeOAuth) subscriptionType() string {
	if c == nil {
		return ""
	}
	return c.SubscriptionType
}

func (c *importClaudeOAuth) scopes() []string {
	if c == nil {
		return nil
	}
	return c.Scopes
}

func (c *importClaudeOAuth) expiresAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	return c.ExpiresAt.value
}

// importFile is the union of the supported credential shapes. Unknown fields,
// such as the Codex CLI's OPENAI_API_KEY, are ignored.
type importFile struct {
	Type             string             `json:"type"`
	Label            string             `json:"label"`
	Email            string             `json:"email"`
	Disabled         *bool              `json:"disabled"`
	AccessToken      string             `json:"access_token"`
	RefreshToken     string             `json:"refresh_token"`
	IDToken          string             `json:"id_token"`
	AccountID        string             `json:"account_id"`
	PlanType         string             `json:"plan_type"`
	SubscriptionType string             `json:"subscription_type"`
	ClientID         string             `json:"client_id"`
	Expires          importTime         `json:"expired"`
	ExpiresAt        importTime         `json:"expiresAt"`
	Scopes           []string           `json:"scopes"`
	Tokens           *importTokens      `json:"tokens"`
	ClaudeAiOauth    *importClaudeOAuth `json:"claudeAiOauth"`
}

// provider classifies a file. Legacy import files carry a "type"; the raw CLI
// files are recognized by their nested objects, and a flat file by fields
// only one provider uses.
func (f *importFile) provider() (string, error) {
	switch strings.ToLower(strings.TrimSpace(f.Type)) {
	case "codex", "openai":
		return "codex", nil
	case "claude", "anthropic":
		return "claude", nil
	case "":
		// Fall through to shape-based detection.
	default:
		return "", errors.New("unsupported credential type")
	}
	if f.Tokens != nil && (f.Tokens.AccessToken != "" || f.Tokens.RefreshToken != "") {
		return "codex", nil
	}
	if f.ClaudeAiOauth != nil && (f.ClaudeAiOauth.AccessToken != "" || f.ClaudeAiOauth.RefreshToken != "") {
		return "claude", nil
	}
	if f.AccountID != "" || f.IDToken != "" || f.PlanType != "" {
		return "codex", nil
	}
	if f.SubscriptionType != "" || len(f.Scopes) > 0 {
		return "claude", nil
	}
	return "", errors.New("unrecognized credential file")
}

// account converts one parsed file into a stored credential. Legacy Codex
// logins keep the "codex" auth mode because they refresh through the Codex
// CLI client; Claude logins use the "oauth" mode. JWTs are decoded only to
// fill metadata such as email, plan, and expiry fallbacks.
func (f *importFile) account(provider, canonicalPath string) (storedAccount, error) {
	account := storedAccount{
		ID:        credentialSourceID(provider, canonicalPath),
		Provider:  provider,
		Label:     strings.TrimSpace(f.Label),
		CreatedAt: time.Now().UTC(),
	}
	if f.Disabled != nil {
		account.Disabled = *f.Disabled
	}
	var access, refresh, idToken string
	switch provider {
	case "codex":
		access = firstNonEmpty(f.AccessToken, f.Tokens.accessToken())
		refresh = firstNonEmpty(f.RefreshToken, f.Tokens.refreshToken())
		idToken = firstNonEmpty(f.IDToken, f.Tokens.idToken())
		account.AuthMode = "codex"
		account.AccountID = strings.TrimSpace(firstNonEmpty(f.AccountID, f.Tokens.accountID()))
		account.Plan = strings.TrimSpace(f.PlanType)
	default:
		access = firstNonEmpty(f.AccessToken, f.ClaudeAiOauth.accessToken())
		refresh = firstNonEmpty(f.RefreshToken, f.ClaudeAiOauth.refreshToken())
		account.AuthMode = "oauth"
		account.Plan = strings.TrimSpace(firstNonEmpty(f.SubscriptionType, f.ClaudeAiOauth.subscriptionType()))
		account.Scopes = firstNonEmptyScopes(f.Scopes, f.ClaudeAiOauth.scopes())
		account.ClientID = strings.TrimSpace(f.ClientID)
		if account.ClientID == "" {
			account.ClientID = claudeClientID
		}
	}
	if firstNonEmpty(access) == "" || firstNonEmpty(refresh) == "" {
		return storedAccount{}, errors.New("missing OAuth credentials")
	}
	account.AccessToken = access
	account.RefreshToken = refresh
	account.IDToken = idToken
	if provider == "codex" {
		account.ClientID = strings.TrimSpace(f.ClientID)
	}
	if account.Label == "" {
		if provider == "codex" {
			account.Label = "Codex"
		} else {
			account.Label = "Claude"
		}
	}
	accessClaims := tokenClaims(access)
	idClaims := tokenClaims(account.IDToken)
	if account.Email == "" {
		account.Email = firstNonEmpty(
			strings.TrimSpace(f.Email),
			claimString(idClaims, "email"),
			claimNested(idClaims, importOpenAIProfileClaim, "email"),
			claimString(accessClaims, "email"),
			claimNested(accessClaims, importOpenAIProfileClaim, "email"),
		)
	}
	if provider == "codex" {
		if account.AccountID == "" {
			account.AccountID = firstNonEmpty(
				claimNested(idClaims, importOpenAIAuthClaim, "chatgpt_account_id"),
				claimNested(accessClaims, importOpenAIAuthClaim, "chatgpt_account_id"),
			)
		}
		if account.Plan == "" {
			account.Plan = firstNonEmpty(
				claimNested(accessClaims, importOpenAIAuthClaim, "chatgpt_plan_type"),
				claimNested(idClaims, importOpenAIAuthClaim, "chatgpt_plan_type"),
			)
		}
	}
	account.ExpiresAt = f.expiry(idClaims, accessClaims)
	return account, nil
}

// expiry prefers a timestamp from the file itself and falls back to an
// unverified JWT exp claim. The claim is metadata only; it never authenticates
// the credential.
func (f *importFile) expiry(idClaims, accessClaims map[string]any) time.Time {
	for _, candidate := range []time.Time{
		f.Expires.value,
		f.ExpiresAt.value,
		f.ClaudeAiOauth.expiresAt(),
		claimTime(accessClaims, "exp"),
		claimTime(idClaims, "exp"),
	} {
		if !candidate.IsZero() {
			return candidate.UTC()
		}
	}
	return time.Time{}
}

// importTime accepts the timestamp shapes the supported files use: an RFC3339
// string, or unix seconds/milliseconds as a number or numeric string.
// Unrecognized metadata formats are ignored rather than rejected.
type importTime struct{ value time.Time }

func (t *importTime) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		return nil
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(data, &text) != nil {
			return nil
		}
		t.value = parseImportTimeString(text)
		return nil
	}
	var number float64
	if json.Unmarshal(data, &number) != nil || number <= 0 {
		return nil
	}
	t.value = importUnixTime(number)
	return nil
}

func parseImportTimeString(text string) time.Time {
	text = strings.TrimSpace(text)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed
		}
	}
	if number, err := strconv.ParseFloat(text, 64); err == nil && number > 0 {
		return importUnixTime(number)
	}
	return time.Time{}
}

// importUnixTime interprets a numeric timestamp: values that large must be
// milliseconds (the Claude CLI's expiresAt), smaller values are seconds
// (a JWT exp claim).
func importUnixTime(value float64) time.Time {
	if value > 1e12 {
		return time.UnixMilli(int64(value)).UTC()
	}
	return time.Unix(int64(value), 0).UTC()
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
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
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
	return importUnixTime(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonEmptyScopes(values ...[]string) []string {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}
