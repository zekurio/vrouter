package gateway

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OpenAI is the only identity provider vrouter accepts for sign-ins. The
// issuer and key set are fixed so a tampered discovery response cannot point
// validation at a different provider.
const (
	openAIIssuer    = "https://auth.openai.com"
	openAIJWKSURL   = "https://auth.openai.com/.well-known/jwks.json"
	maxIDTokenBytes = 64 << 10 // an ID token larger than this is not credible
	maxJWKSBytes    = 1 << 20  // the published key set is a few kilobytes
	idTokenSkew     = 5 * time.Second
)

var jwtSegments = base64.RawURLEncoding.Strict()

// verifiedIdentity is the validated account identity carried by an OpenAI ID
// token. The subject is the account identity; email and subject are not
// workspace identifiers, so callers keep the issued client ID beside this.
// WorkspaceID and PlanType are filled from the signed
// "https://api.openai.com/auth" block when present; a native Codex sign-in
// requires the workspace and stores the subject as the user binding.
type verifiedIdentity struct {
	Subject     string
	Email       string
	Name        string
	WorkspaceID string
	PlanType    string
}

// verifyIDToken validates an OpenAI ID token for a pending sign-in. It fetches
// the fixed public JWKS with the server's HTTP client, verifies the RS256
// signature, then checks the issuer, audience, lifetime, and nonce. Errors
// never contain the token or any claim value.
//
// Browser sign-in always requires the requested nonce. Device authorization
// uses a separate entry point because that protocol does not accept a nonce.
//
// The JWKS is fetched for each sign-in attempt. A bounded cache can replace
// this later if sign-in volume makes the extra request matter.
func (s *server) verifyIDToken(ctx context.Context, token, clientID, nonce string) (verifiedIdentity, error) {
	if clientID == "" || nonce == "" {
		return verifiedIdentity{}, errors.New("id token verification requires an expected client ID and nonce")
	}
	return s.verifyOpenAIIDToken(ctx, token, clientID, &nonce)
}

// Only for tokens exchanged from the fixed device endpoint, bound to the
// pending device_auth_id and provider-issued PKCE verifier. Browser-submitted
// callbacks cannot reach this path. Signature, issuer, audience, lifetime and
// the saved workspace/subject binding are still checked.
func (s *server) verifyDeviceIDToken(ctx context.Context, token, clientID string) (verifiedIdentity, error) {
	return s.verifyOpenAIIDToken(ctx, token, clientID, nil)
}

func (s *server) verifyOpenAIIDToken(ctx context.Context, token, clientID string, nonce *string) (verifiedIdentity, error) {
	if clientID == "" {
		return verifiedIdentity{}, errors.New("id token verification requires an expected client ID")
	}
	if token == "" || len(token) > maxIDTokenBytes {
		return verifiedIdentity{}, errors.New("id token is malformed")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return verifiedIdentity{}, errors.New("id token is malformed")
	}
	headerJSON, err := decodeJWTSegment(parts[0])
	if err != nil {
		return verifiedIdentity{}, errors.New("id token is malformed")
	}
	claimsJSON, err := decodeJWTSegment(parts[1])
	if err != nil {
		return verifiedIdentity{}, errors.New("id token is malformed")
	}
	signature, err := decodeJWTSegment(parts[2])
	if err != nil {
		return verifiedIdentity{}, errors.New("id token is malformed")
	}
	var header idTokenHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return verifiedIdentity{}, errors.New("id token is malformed")
	}
	// JWT headers must explicitly name RS256. Optional JWK metadata is
	// separate from the mandatory signed algorithm header.
	if header.Alg != "RS256" {
		return verifiedIdentity{}, errors.New("id token signing algorithm is not supported")
	}
	if len(header.Crit) > 0 {
		return verifiedIdentity{}, errors.New("id token uses unsupported critical headers")
	}
	if header.Kid == "" {
		return verifiedIdentity{}, errors.New("id token has no key ID")
	}
	jwks, err := s.fetchIDTokenKeys(ctx)
	if err != nil {
		return verifiedIdentity{}, err
	}
	key, err := selectRSASigningKey(jwks, header.Kid)
	if err != nil {
		return verifiedIdentity{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return verifiedIdentity{}, errors.New("id token signature is invalid")
	}
	return validateOpenAIIDTokenClaims(claimsJSON, clientID, nonce)
}

// fetchIDTokenKeys returns the raw JWKS body from the fixed OpenAI endpoint.
func (s *server) fetchIDTokenKeys(ctx context.Context) ([]byte, error) {
	if s.client == nil {
		return nil, errors.New("id token verification has no HTTP client")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, openAIJWKSURL, nil)
	if err != nil {
		return nil, errors.New("could not build the OpenAI signing key request")
	}
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("could not fetch OpenAI signing keys: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("OpenAI signing key endpoint did not return 200")
	}
	// A client that follows redirects would otherwise accept keys from
	// anywhere; the response must still come from the fixed endpoint.
	if response.Request != nil && response.Request.URL != nil && response.Request.URL.String() != openAIJWKSURL {
		return nil, errors.New("OpenAI signing keys came from an unexpected location")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, errors.New("could not read OpenAI signing keys")
	}
	if len(body) > maxJWKSBytes {
		return nil, errors.New("OpenAI signing key response is too large")
	}
	return body, nil
}

type idTokenHeader struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Crit []string `json:"crit"`
}

type idTokenClaims struct {
	Iss   string          `json:"iss"`
	Sub   string          `json:"sub"`
	Aud   json.RawMessage `json:"aud"`
	Azp   string          `json:"azp"`
	Nonce string          `json:"nonce"`
	Exp   json.RawMessage `json:"exp"`
	Iat   json.RawMessage `json:"iat"`
	Nbf   json.RawMessage `json:"nbf"`
	Email string          `json:"email"`
	Name  string          `json:"name"`
	Auth  json.RawMessage `json:"https://api.openai.com/auth"`
}

// idTokenAuthClaims is the workspace identity block of an OpenAI ID token.
// It is only ever read from a token whose signature has already verified.
type idTokenAuthClaims struct {
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	ChatGPTPlanType  string `json:"chatgpt_plan_type"`
}

// validateIDTokenClaims checks the claims of a token whose signature already
// verified and returns the identity to store. Browser flows require a nonce.
func validateIDTokenClaims(payload []byte, clientID, nonce string) (verifiedIdentity, error) {
	if clientID == "" || nonce == "" {
		return verifiedIdentity{}, errors.New("id token verification requires an expected client ID and nonce")
	}
	return validateOpenAIIDTokenClaims(payload, clientID, &nonce)
}

func validateOpenAIIDTokenClaims(payload []byte, clientID string, nonce *string) (verifiedIdentity, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var claims idTokenClaims
	if err := decoder.Decode(&claims); err != nil {
		return verifiedIdentity{}, errors.New("id token claims are malformed")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return verifiedIdentity{}, errors.New("id token claims are malformed")
	}
	if claims.Iss != openAIIssuer {
		return verifiedIdentity{}, errors.New("id token issuer is not recognized")
	}
	if claims.Sub == "" {
		return verifiedIdentity{}, errors.New("id token subject is missing")
	}
	audience, err := parseIDTokenAudience(claims.Aud)
	if err != nil || len(audience) == 0 {
		return verifiedIdentity{}, errors.New("id token audience is malformed")
	}
	matched := false
	for _, value := range audience {
		if value == clientID {
			matched = true
		}
	}
	if !matched {
		return verifiedIdentity{}, errors.New("id token audience does not match the client")
	}
	if len(audience) > 1 && claims.Azp == "" {
		return verifiedIdentity{}, errors.New("id token is missing the authorized party")
	}
	if claims.Azp != "" && claims.Azp != clientID {
		return verifiedIdentity{}, errors.New("id token authorized party does not match the client")
	}
	expiry, ok := int64Claim(claims.Exp)
	if !ok {
		return verifiedIdentity{}, errors.New("id token expiration is missing")
	}
	issuedAt, ok := int64Claim(claims.Iat)
	if !ok {
		return verifiedIdentity{}, errors.New("id token issued-at time is missing")
	}
	now := time.Now()
	if time.Unix(issuedAt, 0).After(now.Add(idTokenSkew)) || issuedAt > expiry {
		return verifiedIdentity{}, errors.New("id token issued-at time is invalid")
	}
	if now.After(time.Unix(expiry, 0).Add(idTokenSkew)) {
		return verifiedIdentity{}, errors.New("id token is expired")
	}
	if len(claims.Nbf) > 0 {
		notBefore, ok := int64Claim(claims.Nbf)
		if !ok {
			return verifiedIdentity{}, errors.New("id token not-before time is malformed")
		}
		if time.Unix(notBefore, 0).After(now.Add(idTokenSkew)) {
			return verifiedIdentity{}, errors.New("id token is not valid yet")
		}
	}
	if nonce != nil && (claims.Nonce == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(*nonce)) != 1) {
		return verifiedIdentity{}, errors.New("id token nonce does not match")
	}
	auth := parseIDTokenAuthClaims(claims.Auth)
	return verifiedIdentity{Subject: claims.Sub, Email: claims.Email, Name: claims.Name, WorkspaceID: auth.ChatGPTAccountID, PlanType: auth.ChatGPTPlanType}, nil
}

// parseIDTokenAuthClaims reads the workspace identity block of an already
// signature-verified ID token. A malformed block yields no identity instead of
// an error; the caller decides whether the missing workspace is acceptable.
func parseIDTokenAuthClaims(raw json.RawMessage) idTokenAuthClaims {
	var claims idTokenAuthClaims
	if len(raw) == 0 || json.Unmarshal(raw, &claims) != nil {
		return idTokenAuthClaims{}
	}
	claims.ChatGPTAccountID = strings.TrimSpace(claims.ChatGPTAccountID)
	claims.ChatGPTPlanType = strings.TrimSpace(claims.ChatGPTPlanType)
	return claims
}

// parseIDTokenAudience accepts the string or string-list form of aud.
func parseIDTokenAudience(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing audience")
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	return nil, errors.New("malformed audience")
}

// int64Claim decodes a claim that must be a bare JSON integer. Quoted,
// fractional, and exponent forms are rejected instead of coerced.
func int64Claim(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func decodeJWTSegment(segment string) ([]byte, error) {
	if segment == "" {
		return nil, errors.New("empty segment")
	}
	return jwtSegments.DecodeString(segment)
}

type jwksDocument struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// selectRSASigningKey returns the public key whose kid matches exactly. Keys
// that do not match, and matching keys that are not plausible RS256 signature
// keys, are skipped or rejected.
func selectRSASigningKey(jwks []byte, kid string) (*rsa.PublicKey, error) {
	var document jwksDocument
	if err := json.Unmarshal(jwks, &document); err != nil {
		return nil, errors.New("OpenAI signing keys are malformed")
	}
	matched := false
	for _, key := range document.Keys {
		if key.Kid != kid {
			continue
		}
		matched = true
		if public, err := rsaPublicKey(key); err == nil {
			return public, nil
		}
	}
	if matched {
		return nil, errors.New("OpenAI signing key is not usable")
	}
	return nil, errors.New("OpenAI signing key was not found")
}

func rsaPublicKey(key jwkKey) (*rsa.PublicKey, error) {
	if key.Kty != "RSA" {
		return nil, errors.New("key is not RSA")
	}
	if key.Use != "" && key.Use != "sig" {
		return nil, errors.New("key is not a signature key")
	}
	if key.Alg != "" && key.Alg != "RS256" {
		return nil, errors.New("key algorithm is not RS256")
	}
	modulusBytes, err := base64.RawURLEncoding.DecodeString(key.N)
	if err != nil || len(modulusBytes) == 0 {
		return nil, errors.New("key modulus is malformed")
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(key.E)
	if err != nil || len(exponentBytes) == 0 {
		return nil, errors.New("key exponent is malformed")
	}
	modulus := new(big.Int).SetBytes(modulusBytes)
	exponent := new(big.Int).SetBytes(exponentBytes)
	if modulus.BitLen() < 2048 {
		return nil, errors.New("key modulus is too small")
	}
	// The exponent must fit the platform's int and be a plausible odd value.
	if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > 1<<31-1 || exponent.Bit(0) == 0 {
		return nil, errors.New("key exponent is not usable")
	}
	return &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}, nil
}
