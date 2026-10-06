package gateway

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	identityTestKid      = "identity-test-key"
	identityTestClientID = "oaiapp_identity_test"
	identityTestNonce    = "identity-test-nonce"
	identityTestSubject  = "subject-identity-test"
	identityTestEmail    = "person@example.com"
	identityTestName     = "Example Person"
)

// identityFixture holds an RSA key pair and a matching one-key JWKS document.
// Every test builds its own so shared state cannot mask a failure.
type identityFixture struct {
	key  *rsa.PrivateKey
	kid  string
	jwks []byte
}

func newIdentityFixture(t *testing.T) identityFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test RSA key: %v", err)
	}
	fixture := identityFixture{key: key, kid: identityTestKid}
	fixture.jwks = identityJWKSDocument(identityJWK(&key.PublicKey, fixture.kid))
	return fixture
}

func identityJWK(pub *rsa.PublicKey, kid string) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func identityJWKSDocument(keys ...map[string]any) []byte {
	document, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		panic(err) // only built-in types are passed
	}
	return document
}

func identityHeader(kid, alg string) map[string]any {
	header := map[string]any{"typ": "JWT", "kid": kid}
	if alg != "" {
		header["alg"] = alg
	}
	return header
}

func identityClaims(clientID, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":   openAIIssuer,
		"sub":   identityTestSubject,
		"aud":   clientID,
		"nonce": nonce,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"email": identityTestEmail,
		"name":  identityTestName,
	}
}

func identitySign(t *testing.T, key *rsa.PrivateKey, header, claims any) string {
	t.Helper()
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal test header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal test claims: %v", err)
	}
	return identitySignRaw(t, key, headerJSON, claimsJSON)
}

// identitySignRaw signs the exact encoded segments, so tests can craft
// malformed payloads that still carry a valid signature.
func identitySignRaw(t *testing.T, key *rsa.PrivateKey, headerJSON, claimsJSON []byte) string {
	t.Helper()
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func identityValidToken(t *testing.T, fixture identityFixture) string {
	t.Helper()
	return identitySign(t, fixture.key, identityHeader(fixture.kid, "RS256"), identityClaims(identityTestClientID, identityTestNonce))
}

type identityRoundTrip func(*http.Request) (*http.Response, error)

func (f identityRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func identityResponse(request *http.Request, status int, body []byte) *http.Response {
	recorder := httptest.NewRecorder()
	recorder.WriteHeader(status)
	_, _ = recorder.Write(body)
	response := recorder.Result()
	response.Request = request
	return response
}

func identityJWKSClient(jwks []byte) *http.Client {
	return &http.Client{Transport: identityRoundTrip(func(request *http.Request) (*http.Response, error) {
		return identityResponse(request, http.StatusOK, jwks), nil
	})}
}

func identityServer(client *http.Client) *server {
	return &server{client: client}
}

// identityRequireError fails when verification unexpectedly succeeded or when
// the error message leaks the token or a claim value.
func identityRequireError(t *testing.T, err error, token string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected ID token verification to fail")
	}
	for _, secret := range []string{token, identityTestNonce, identityTestSubject, identityTestEmail, identityTestName} {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatalf("verification error leaks %q: %v", secret, err)
		}
	}
}

func TestVerifyIDTokenValid(t *testing.T) {
	fixture := newIdentityFixture(t)
	var requests []string
	client := &http.Client{Transport: identityRoundTrip(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.Method+" "+request.URL.String())
		return identityResponse(request, http.StatusOK, fixture.jwks), nil
	})}
	token := identityValidToken(t, fixture)
	identity, err := identityServer(client).verifyIDToken(context.Background(), token, identityTestClientID, identityTestNonce)
	if err != nil {
		t.Fatalf("valid ID token rejected: %v", err)
	}
	want := verifiedIdentity{Subject: identityTestSubject, Email: identityTestEmail, Name: identityTestName}
	if identity != want {
		t.Fatalf("identity = %+v, want %+v", identity, want)
	}
	if len(requests) != 1 || requests[0] != "GET "+openAIJWKSURL {
		t.Fatalf("JWKS requests = %v, want one GET to %s", requests, openAIJWKSURL)
	}
}

func TestVerifyIDTokenRequiresExpectedClientAndNonce(t *testing.T) {
	fixture := newIdentityFixture(t)
	token := identityValidToken(t, fixture)
	verifier := identityServer(identityJWKSClient(fixture.jwks))
	for _, test := range []struct {
		name     string
		clientID string
		nonce    string
	}{
		{"no client ID", "", identityTestNonce},
		{"no nonce", identityTestClientID, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := verifier.verifyIDToken(context.Background(), token, test.clientID, test.nonce); err == nil {
				t.Fatal("verification without expected values succeeded")
			}
		})
	}
}

func TestVerifyIDTokenAcceptsMatchingKeyAndOmittedMetadata(t *testing.T) {
	fixture := newIdentityFixture(t)
	unrelated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate unrelated RSA key: %v", err)
	}
	// JWK metadata may omit use/alg; the signed JWT must still name RS256.
	matching := identityJWK(&fixture.key.PublicKey, fixture.kid)
	delete(matching, "use")
	delete(matching, "alg")
	document := identityJWKSDocument(identityJWK(&unrelated.PublicKey, "unrelated-key"), matching)
	token := identitySign(t, fixture.key, identityHeader(fixture.kid, "RS256"), identityClaims(identityTestClientID, identityTestNonce))
	identity, err := identityServer(identityJWKSClient(document)).verifyIDToken(context.Background(), token, identityTestClientID, identityTestNonce)
	if err != nil {
		t.Fatalf("token with omitted JWK metadata rejected: %v", err)
	}
	if identity.Subject != identityTestSubject {
		t.Fatalf("subject = %q, want %q", identity.Subject, identityTestSubject)
	}
}

func TestVerifyIDTokenRejectsBadSignatureOrKey(t *testing.T) {
	fixture := newIdentityFixture(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other RSA key: %v", err)
	}
	tests := []struct {
		name  string
		token func(t *testing.T) string
		jwks  []byte
	}{
		{
			"signature from another key with the right kid",
			func(t *testing.T) string {
				return identitySign(t, other, identityHeader(fixture.kid, "RS256"), identityClaims(identityTestClientID, identityTestNonce))
			},
			fixture.jwks,
		},
		{
			"unknown key ID",
			func(t *testing.T) string {
				return identitySign(t, fixture.key, identityHeader("other-kid", "RS256"), identityClaims(identityTestClientID, identityTestNonce))
			},
			fixture.jwks,
		},
		{
			"claims swapped after signing",
			func(t *testing.T) string {
				parts := strings.Split(identityValidToken(t, fixture), ".")
				claims := identityClaims(identityTestClientID, identityTestNonce)
				claims["email"] = "attacker@example.com"
				payload, err := json.Marshal(claims)
				if err != nil {
					t.Fatalf("marshal claims: %v", err)
				}
				return parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]
			},
			fixture.jwks,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token := test.token(t)
			_, err := identityServer(identityJWKSClient(test.jwks)).verifyIDToken(context.Background(), token, identityTestClientID, identityTestNonce)
			identityRequireError(t, err, token)
		})
	}
}

func TestVerifyIDTokenRejectsAlgorithmConfusion(t *testing.T) {
	fixture := newIdentityFixture(t)
	claimsJSON, err := json.Marshal(identityClaims(identityTestClientID, identityTestNonce))
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	headerJSON, err := json.Marshal(identityHeader(fixture.kid, "none"))
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON) + "."

	hmacHeader, err := json.Marshal(identityHeader(fixture.kid, "HS256"))
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	hmacInput := base64.RawURLEncoding.EncodeToString(hmacHeader) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	mac := hmac.New(sha256.New, fixture.jwks)
	mac.Write([]byte(hmacInput))
	hmacSigned := hmacInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	verifier := identityServer(identityJWKSClient(fixture.jwks))
	for _, test := range []struct {
		name  string
		token string
	}{
		{"alg none", unsigned},
		{"missing alg", identitySign(t, fixture.key, identityHeader(fixture.kid, ""), identityClaims(identityTestClientID, identityTestNonce))},
		{"alg HS256 with the public keys as the HMAC secret", hmacSigned},
		{"alg RS512", identitySign(t, fixture.key, identityHeader(fixture.kid, "RS512"), identityClaims(identityTestClientID, identityTestNonce))},
		{"alg ES256", identitySign(t, fixture.key, identityHeader(fixture.kid, "ES256"), identityClaims(identityTestClientID, identityTestNonce))},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := verifier.verifyIDToken(context.Background(), test.token, identityTestClientID, identityTestNonce)
			identityRequireError(t, err, test.token)
		})
	}
}

func TestVerifyIDTokenClaimValidation(t *testing.T) {
	fixture := newIdentityFixture(t)
	verifier := identityServer(identityJWKSClient(fixture.jwks))
	tests := []struct {
		name    string
		mutate  func(claims map[string]any)
		wantErr bool
	}{
		{"audience list includes client with azp", func(claims map[string]any) {
			claims["aud"] = []string{"other-client", identityTestClientID}
			claims["azp"] = identityTestClientID
		}, false},
		{"single audience with azp", func(claims map[string]any) {
			claims["azp"] = identityTestClientID
		}, false},
		{"audience list without azp", func(claims map[string]any) {
			claims["aud"] = []string{"other-client", identityTestClientID}
		}, true},
		{"audience list with wrong azp", func(claims map[string]any) {
			claims["aud"] = []string{"other-client", identityTestClientID}
			claims["azp"] = "someone-else"
		}, true},
		{"wrong audience", func(claims map[string]any) { claims["aud"] = "other-client" }, true},
		{"audience of the wrong type", func(claims map[string]any) { claims["aud"] = 42 }, true},
		{"empty audience list", func(claims map[string]any) { claims["aud"] = []string{} }, true},
		{"missing audience", func(claims map[string]any) { delete(claims, "aud") }, true},
		{"wrong issuer", func(claims map[string]any) { claims["iss"] = "https://login.example.com" }, true},
		{"missing issuer", func(claims map[string]any) { delete(claims, "iss") }, true},
		{"missing subject", func(claims map[string]any) { delete(claims, "sub") }, true},
		{"empty subject", func(claims map[string]any) { claims["sub"] = "" }, true},
		{"subject of the wrong type", func(claims map[string]any) { claims["sub"] = 7 }, true},
		{"wrong nonce", func(claims map[string]any) { claims["nonce"] = "a-different-nonce" }, true},
		{"missing nonce", func(claims map[string]any) { delete(claims, "nonce") }, true},
		{"nonce of the wrong type", func(claims map[string]any) { claims["nonce"] = 1234 }, true},
		{"expired token", func(claims map[string]any) { claims["exp"] = time.Now().Add(-time.Hour).Unix() }, true},
		{"expiration within skew", func(claims map[string]any) {
			claims["iat"] = time.Now().Add(-time.Hour).Unix()
			claims["exp"] = time.Now().Add(-3 * time.Second).Unix()
		}, false},
		{"missing expiration", func(claims map[string]any) { delete(claims, "exp") }, true},
		{"quoted expiration", func(claims map[string]any) { claims["exp"] = "1750000000" }, true},
		{"fractional expiration", func(claims map[string]any) { claims["exp"] = float64(1750000000) + 0.5 }, true},
		{"exponent expiration", func(claims map[string]any) { claims["exp"] = 1e30 }, true},
		{"boolean expiration", func(claims map[string]any) { claims["exp"] = true }, true},
		{"future issued-at", func(claims map[string]any) { claims["iat"] = time.Now().Add(time.Hour).Unix() }, true},
		{"missing issued-at", func(claims map[string]any) { delete(claims, "iat") }, true},
		{"fractional issued-at", func(claims map[string]any) { claims["iat"] = float64(time.Now().Unix()) + 0.5 }, true},
		{"not valid before in the future", func(claims map[string]any) { claims["nbf"] = time.Now().Add(time.Hour).Unix() }, true},
		{"not valid before within skew", func(claims map[string]any) { claims["nbf"] = time.Now().Add(3 * time.Second).Unix() }, false},
		{"malformed not valid before", func(claims map[string]any) { claims["nbf"] = "soon" }, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := identityClaims(identityTestClientID, identityTestNonce)
			test.mutate(claims)
			token := identitySign(t, fixture.key, identityHeader(fixture.kid, "RS256"), claims)
			_, err := verifier.verifyIDToken(context.Background(), token, identityTestClientID, identityTestNonce)
			if test.wantErr {
				identityRequireError(t, err, token)
			} else if err != nil {
				t.Fatalf("valid claims rejected: %v", err)
			}
		})
	}
}

func TestVerifyIDTokenRejectsMalformedTokens(t *testing.T) {
	fixture := newIdentityFixture(t)
	verifier := identityServer(identityJWKSClient(fixture.jwks))
	validToken := identityValidToken(t, fixture)
	parts := strings.Split(validToken, ".")
	claimsJSON, err := json.Marshal(identityClaims(identityTestClientID, identityTestNonce))
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	headerJSON, err := json.Marshal(identityHeader(fixture.kid, "RS256"))
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	notJSON := []byte("not json")
	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"two segments", parts[0] + "." + parts[1]},
		{"four segments", validToken + ".extra"},
		{"empty signature segment", parts[0] + "." + parts[1] + "."},
		{"invalid base64", "!!!.!!!.!!!"},
		{"oversized", strings.Repeat("a", maxIDTokenBytes+1)},
		{"header is not JSON", base64.RawURLEncoding.EncodeToString(notJSON) + "." + parts[1] + "." + parts[2]},
		{"claims are not JSON", identitySignRaw(t, fixture.key, headerJSON, notJSON)},
		{"claims have trailing data", identitySignRaw(t, fixture.key, headerJSON, append(claimsJSON, []byte(" {}")...))},
		{"padded base64 header", base64.URLEncoding.EncodeToString(headerJSON) + "." + parts[1] + "." + parts[2]},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := verifier.verifyIDToken(context.Background(), test.token, identityTestClientID, identityTestNonce)
			identityRequireError(t, err, test.token)
		})
	}
}

func TestVerifyIDTokenRejectsUnusableSigningKeys(t *testing.T) {
	fixture := newIdentityFixture(t)
	token := identityValidToken(t, fixture)
	smallModulus := identityJWK(&fixture.key.PublicKey, fixture.kid)
	smallModulus["n"] = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 128)) // 1024 bits
	exponentOne := identityJWK(&fixture.key.PublicKey, fixture.kid)
	exponentOne["e"] = base64.RawURLEncoding.EncodeToString(big.NewInt(1).Bytes())
	evenExponent := identityJWK(&fixture.key.PublicKey, fixture.kid)
	evenExponent["e"] = base64.RawURLEncoding.EncodeToString(big.NewInt(4).Bytes())
	malformedModulus := identityJWK(&fixture.key.PublicKey, fixture.kid)
	malformedModulus["n"] = "!!!"
	malformedExponent := identityJWK(&fixture.key.PublicKey, fixture.kid)
	malformedExponent["e"] = "!!!"
	noModulus := identityJWK(&fixture.key.PublicKey, fixture.kid)
	delete(noModulus, "n")
	wrongKeyType := identityJWK(&fixture.key.PublicKey, fixture.kid)
	wrongKeyType["kty"] = "EC"
	encryptionUse := identityJWK(&fixture.key.PublicKey, fixture.kid)
	encryptionUse["use"] = "enc"
	otherAlgorithm := identityJWK(&fixture.key.PublicKey, fixture.kid)
	otherAlgorithm["alg"] = "RS512"
	hmacAlgorithm := identityJWK(&fixture.key.PublicKey, fixture.kid)
	hmacAlgorithm["alg"] = "HS256"
	tests := []struct {
		name string
		jwks []byte
	}{
		{"wrong key ID", identityJWKSDocument(identityJWK(&fixture.key.PublicKey, "other-kid"))},
		{"empty key set", identityJWKSDocument()},
		{"malformed document", []byte("{not json")},
		{"key type is EC", identityJWKSDocument(wrongKeyType)},
		{"key use is encryption", identityJWKSDocument(encryptionUse)},
		{"key algorithm is RS512", identityJWKSDocument(otherAlgorithm)},
		{"key algorithm is HS256", identityJWKSDocument(hmacAlgorithm)},
		{"modulus below 2048 bits", identityJWKSDocument(smallModulus)},
		{"exponent one", identityJWKSDocument(exponentOne)},
		{"even exponent", identityJWKSDocument(evenExponent)},
		{"malformed modulus", identityJWKSDocument(malformedModulus)},
		{"malformed exponent", identityJWKSDocument(malformedExponent)},
		{"missing modulus", identityJWKSDocument(noModulus)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := identityServer(identityJWKSClient(test.jwks)).verifyIDToken(context.Background(), token, identityTestClientID, identityTestNonce)
			identityRequireError(t, err, token)
		})
	}
}

func TestVerifyIDTokenRejectsKeySetFailures(t *testing.T) {
	fixture := newIdentityFixture(t)
	token := identityValidToken(t, fixture)
	redirect := func(request *http.Request) (*http.Response, error) {
		response := identityResponse(request, http.StatusFound, nil)
		response.Header.Set("Location", "https://attacker.example/jwks.json")
		return response, nil
	}
	redirectedRequests := 0
	tests := []struct {
		name   string
		client *http.Client
	}{
		{"non-200 status", &http.Client{Transport: identityRoundTrip(func(request *http.Request) (*http.Response, error) {
			return identityResponse(request, http.StatusInternalServerError, nil), nil
		})}},
		{"redirect not followed", &http.Client{
			Transport:     identityRoundTrip(redirect),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}},
		{"redirect followed to another host", &http.Client{Transport: identityRoundTrip(func(request *http.Request) (*http.Response, error) {
			redirectedRequests++
			if request.URL.String() == openAIJWKSURL {
				return redirect(request)
			}
			return identityResponse(request, http.StatusOK, fixture.jwks), nil
		})}},
		{"oversized body", &http.Client{Transport: identityRoundTrip(func(request *http.Request) (*http.Response, error) {
			return identityResponse(request, http.StatusOK, bytes.Repeat([]byte{'x'}, maxJWKSBytes+1)), nil
		})}},
		{"transport error", &http.Client{Transport: identityRoundTrip(func(request *http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := identityServer(test.client).verifyIDToken(context.Background(), token, identityTestClientID, identityTestNonce)
			identityRequireError(t, err, token)
		})
	}
	if redirectedRequests != 2 {
		t.Fatalf("redirected JWKS requests = %d, want 2", redirectedRequests)
	}
}

func TestVerifyIDTokenHonorsCanceledContext(t *testing.T) {
	fixture := newIdentityFixture(t)
	token := identityValidToken(t, fixture)
	client := &http.Client{Transport: identityRoundTrip(func(request *http.Request) (*http.Response, error) {
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		return identityResponse(request, http.StatusOK, fixture.jwks), nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := identityServer(client).verifyIDToken(ctx, token, identityTestClientID, identityTestNonce)
	identityRequireError(t, err, token)
}
