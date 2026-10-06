package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
)

// Application sign-in is configured entirely from the environment:
//
//	VROUTER_PUBLIC_URL      external origin of this vrouter, used for redirects
//	VROUTER_OAUTH_PROVIDERS JSON array of OIDC provider configurations
//
// Provider account connections (Codex, Claude) have their own built-in
// credentials and are unrelated to this configuration.
const (
	publicURLEnv      = "VROUTER_PUBLIC_URL"
	oauthProvidersEnv = "VROUTER_OAUTH_PROVIDERS"
	maxOIDCProviders  = 32
)

// defaultOIDCScopes are requested when a provider config omits scopes. The
// openid scope is mandatory for an ID token; profile and email are display
// metadata only.
var defaultOIDCScopes = []string{"openid", "profile", "email"}

// RoleMappingConfig grants a role when a verified ID token claim equals a
// value. claim is a dot-separated path; the leaf may be a string or an array
// of strings, and any array along the path is traversed.
type RoleMappingConfig struct {
	Claim string `json:"claim"`
	Value string `json:"value"`
	Role  string `json:"role"`
}

// OIDCProviderConfig is one application sign-in provider. clientSecretEnv
// names the environment variable holding the client secret; when empty the
// provider is treated as a public PKCE client.
type OIDCProviderConfig struct {
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	Issuer          string              `json:"issuer"`
	ClientID        string              `json:"clientId"`
	ClientSecretEnv string              `json:"clientSecretEnv,omitempty"`
	Scopes          []string            `json:"scopes,omitempty"`
	RoleMappings    []RoleMappingConfig `json:"roleMappings"`

	// clientSecret is resolved from ClientSecretEnv at load time. It is never
	// serialized and never appears in error messages.
	clientSecret string
}

// IdentityConfig is the application sign-in configuration. With no providers
// vrouter keeps its legacy modes: token when an admin token is configured,
// local loopback access otherwise.
type IdentityConfig struct {
	PublicURL string
	Providers []OIDCProviderConfig
}

// LoadIdentityConfigFromEnv reads and validates the sign-in environment. An
// invalid configuration is a startup error. Secret values are read from the
// environment but never echoed in returned errors.
func LoadIdentityConfigFromEnv() (IdentityConfig, error) {
	cfg := IdentityConfig{PublicURL: os.Getenv(publicURLEnv)}
	if raw := strings.TrimSpace(os.Getenv(oauthProvidersEnv)); raw != "" {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg.Providers); err != nil {
			return IdentityConfig{}, fmt.Errorf("%s is not a valid provider array: %w", oauthProvidersEnv, err)
		}
		if cfg.Providers == nil {
			return IdentityConfig{}, fmt.Errorf("%s must contain a JSON array of providers", oauthProvidersEnv)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return IdentityConfig{}, fmt.Errorf("%s must contain a single JSON array", oauthProvidersEnv)
		}
	}
	cfg = normalizeIdentityConfig(cfg)
	for i := range cfg.Providers {
		if name := cfg.Providers[i].ClientSecretEnv; name != "" {
			cfg.Providers[i].clientSecret = os.Getenv(name)
		}
	}
	if err := validateIdentityConfig(cfg); err != nil {
		return IdentityConfig{}, err
	}
	return cfg, nil
}

// normalizeIdentityConfig trims configuration values and applies defaults. It
// preserves resolved client secrets.
func normalizeIdentityConfig(cfg IdentityConfig) IdentityConfig {
	normalized := IdentityConfig{}
	if cfg.PublicURL != "" {
		if origin, err := normalizePublicURL(cfg.PublicURL); err == nil {
			normalized.PublicURL = origin
		} else {
			// Leave the invalid value for validation to report.
			normalized.PublicURL = strings.TrimSpace(cfg.PublicURL)
		}
	}
	if len(cfg.Providers) == 0 {
		return normalized
	}
	normalized.Providers = make([]OIDCProviderConfig, 0, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		normalized.Providers = append(normalized.Providers, normalizeOIDCProvider(provider))
	}
	return normalized
}

func normalizeOIDCProvider(provider OIDCProviderConfig) OIDCProviderConfig {
	provider.ID = strings.TrimSpace(provider.ID)
	provider.Name = strings.TrimSpace(provider.Name)
	provider.Issuer = strings.TrimSpace(provider.Issuer)
	provider.ClientID = strings.TrimSpace(provider.ClientID)
	provider.ClientSecretEnv = strings.TrimSpace(provider.ClientSecretEnv)

	scopes := make([]string, 0, len(provider.Scopes))
	for _, scope := range provider.Scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || containsString(scopes, scope) {
			continue
		}
		scopes = append(scopes, scope)
	}
	if len(scopes) == 0 {
		scopes = append(scopes, defaultOIDCScopes...)
	}
	provider.Scopes = scopes

	mappings := make([]RoleMappingConfig, 0, len(provider.RoleMappings))
	for _, mapping := range provider.RoleMappings {
		mapping.Claim = strings.TrimSpace(mapping.Claim)
		mapping.Value = strings.TrimSpace(mapping.Value)
		mapping.Role = strings.ToLower(strings.TrimSpace(mapping.Role))
		mappings = append(mappings, mapping)
	}
	provider.RoleMappings = mappings
	return provider
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// validateIdentityConfig checks a normalized configuration. Rules that guard
// against silently weakening authentication fail startup instead of falling
// back to a default.
func validateIdentityConfig(cfg IdentityConfig) error {
	if cfg.PublicURL != "" {
		if _, err := normalizePublicURL(cfg.PublicURL); err != nil {
			return err
		}
	}
	if len(cfg.Providers) == 0 {
		return nil
	}
	if cfg.PublicURL == "" {
		return fmt.Errorf("%s is required when %s configures OIDC providers", publicURLEnv, oauthProvidersEnv)
	}
	if len(cfg.Providers) > maxOIDCProviders {
		return fmt.Errorf("%s configures %d providers; the maximum is %d", oauthProvidersEnv, len(cfg.Providers), maxOIDCProviders)
	}
	seen := make(map[string]bool, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		if err := validateOIDCProvider(provider); err != nil {
			label := provider.ID
			if label == "" {
				label = provider.Name
			}
			if label == "" {
				return err
			}
			return fmt.Errorf("OIDC provider %q: %w", label, err)
		}
		if seen[provider.ID] {
			return fmt.Errorf("duplicate OIDC provider id %q", provider.ID)
		}
		seen[provider.ID] = true
	}
	return nil
}

func validateOIDCProvider(provider OIDCProviderConfig) error {
	if provider.ID == "" {
		return errors.New("id is required")
	}
	if !validProviderID(provider.ID) {
		return errors.New("id must be 1-64 characters of letters, digits, '.', '_' or '-'")
	}
	if provider.Name == "" {
		return errors.New("name is required")
	}
	if provider.Issuer == "" {
		return errors.New("issuer is required")
	}
	if err := validateIssuerURL(provider.Issuer); err != nil {
		return err
	}
	if provider.ClientID == "" {
		return errors.New("clientId is required")
	}
	if provider.ClientSecretEnv != "" {
		if !validEnvName(provider.ClientSecretEnv) {
			return fmt.Errorf("clientSecretEnv %q is not a valid environment variable name", provider.ClientSecretEnv)
		}
		if provider.clientSecret == "" {
			return fmt.Errorf("environment variable %s is empty or unset", provider.ClientSecretEnv)
		}
	}
	if !containsString(provider.Scopes, "openid") {
		return errors.New("scopes must include openid")
	}
	if len(provider.RoleMappings) == 0 {
		return errors.New("at least one role mapping is required; there is no default role")
	}
	for _, mapping := range provider.RoleMappings {
		if !validClaimPath(mapping.Claim) {
			return fmt.Errorf("role mapping claim %q must be a non-empty dot-separated path", mapping.Claim)
		}
		if mapping.Value == "" {
			return fmt.Errorf("role mapping for claim %q has no value", mapping.Claim)
		}
		if mapping.Role != "admin" && mapping.Role != "user" {
			return fmt.Errorf("role mapping role must be \"admin\" or \"user\"")
		}
	}
	return nil
}

// normalizePublicURL returns the external origin without a trailing slash.
// HTTPS is required except for loopback development addresses.
func normalizePublicURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return "", fmt.Errorf("%s must be an absolute URL such as https://vrouter.example", publicURLEnv)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", fmt.Errorf("%s must not contain a query, fragment, or user information", publicURLEnv)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("%s must be an origin such as https://vrouter.example without a path", publicURLEnv)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !loopbackHost(parsed.Hostname()) {
			return "", fmt.Errorf("%s must use HTTPS except for loopback development addresses", publicURLEnv)
		}
	default:
		return "", fmt.Errorf("%s must use http or https", publicURLEnv)
	}
	return scheme + "://" + parsed.Host, nil
}

func validateIssuerURL(issuer string) error {
	parsed, err := url.Parse(issuer)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return errors.New("issuer must be an absolute URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return errors.New("issuer must not contain a query, fragment, or user information")
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !loopbackHost(parsed.Hostname()) {
			return errors.New("issuer must use HTTPS except for loopback development addresses")
		}
	default:
		return errors.New("issuer must use http or https")
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validProviderID(id string) bool {
	if id == "" || len(id) > 64 || id == "." || id == ".." {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func validClaimPath(claim string) bool {
	if claim == "" {
		return false
	}
	for _, segment := range strings.Split(claim, ".") {
		if strings.TrimSpace(segment) == "" {
			return false
		}
	}
	return true
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
		if i == 0 && !letter {
			return false
		}
		if !letter && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
