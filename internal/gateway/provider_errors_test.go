package gateway

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func providerResponse(status int, body string, headers map[string]string) *http.Response {
	resp := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	for name, value := range headers {
		resp.Header.Set(name, value)
	}
	return resp
}

func providerErrorBody(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	body, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %#v", out)
	}
	return body
}

func TestProviderErrorJSONFields(t *testing.T) {
	resp := providerResponse(429, `{"error":{"type":"rate_limit_error","message":"Rate limit reached for gpt-5. Retry after 20s.","code":"rate_limit_exceeded"}}`, map[string]string{"x-request-id": "req_abc123"})
	out := readProviderError(resp, storedAccount{})
	body := providerErrorBody(t, out)
	if body["type"] != "rate_limit_error" || body["message"] != "Rate limit reached for gpt-5. Retry after 20s." || body["code"] != "rate_limit_exceeded" {
		t.Fatalf("unexpected error body: %#v", body)
	}
	if out["request_id"] != "req_abc123" {
		t.Fatalf("request id: %#v", out)
	}
	if len(body) != 3 {
		t.Fatalf("unstable error keys: %#v", body)
	}
}

func TestProviderErrorStringMessage(t *testing.T) {
	out := readProviderError(providerResponse(503, `{"error":"model is overloaded"}`, nil), storedAccount{})
	body := providerErrorBody(t, out)
	if body["message"] != "model is overloaded" || body["type"] != providerErrorType {
		t.Fatalf("unexpected body: %#v", body)
	}
	if _, exists := body["code"]; exists {
		t.Fatal("absent code returned")
	}
	if _, exists := out["request_id"]; exists {
		t.Fatal("absent request id returned")
	}
}

func TestProviderErrorTopLevelFields(t *testing.T) {
	resp := providerResponse(400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 200000 is greater than the maximum"}}`, map[string]string{"request-id": "req_456"})
	out := readProviderError(resp, storedAccount{})
	body := providerErrorBody(t, out)
	if body["type"] != "invalid_request_error" || body["message"] != "max_tokens: 200000 is greater than the maximum" {
		t.Fatalf("unexpected body: %#v", body)
	}
	if out["request_id"] != "req_456" {
		t.Fatalf("request id: %#v", out)
	}
}

func TestProviderErrorNumericCode(t *testing.T) {
	out := readProviderError(providerResponse(429, `{"message":"slow down","code":429}`, nil), storedAccount{})
	body := providerErrorBody(t, out)
	if body["message"] != "slow down" || body["code"] != "429" {
		t.Fatalf("unexpected body: %#v", body)
	}
}

func TestProviderErrorFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"html", 502, "<html><body><h1>502 Bad Gateway</h1></body></html>"},
		{"malformed", 500, `{"error":`},
		{"empty", 429, ""},
		{"plain text", 502, "upstream connection reset"},
		{"oversized", 500, `{"error":"` + strings.Repeat("a", providerErrorReadMax) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := readProviderError(providerResponse(tc.status, tc.body, nil), storedAccount{})
			body := providerErrorBody(t, out)
			if body["message"] != http.StatusText(tc.status) || body["type"] != providerErrorType {
				t.Fatalf("unexpected fallback: %#v", body)
			}
		})
	}
}

func TestProviderErrorNilBody(t *testing.T) {
	out := readProviderError(&http.Response{StatusCode: 500, Header: http.Header{}}, storedAccount{})
	body := providerErrorBody(t, out)
	if body["message"] != http.StatusText(500) || body["type"] != providerErrorType {
		t.Fatalf("unexpected body: %#v", body)
	}
}

func TestProviderErrorMessageBounded(t *testing.T) {
	long := strings.Repeat("x", providerErrorMessageMax+500)
	out := readProviderError(providerResponse(500, `{"error":{"message":"`+long+`"}}`, nil), storedAccount{})
	message, _ := providerErrorBody(t, out)["message"].(string)
	if runes := []rune(message); len(runes) != providerErrorMessageMax {
		t.Fatalf("message length: %d", len(runes))
	}
}

type writeCounter struct{ n int }

func (c *writeCounter) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }

func TestProviderErrorReadBounded(t *testing.T) {
	counter := &writeCounter{}
	stream := io.TeeReader(strings.NewReader(strings.Repeat("a", providerErrorReadMax*2)), counter)
	resp := &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(stream)}
	out := readProviderError(resp, storedAccount{})
	if counter.n > providerErrorReadMax+1 {
		t.Fatalf("read %d bytes, want at most %d", counter.n, providerErrorReadMax+1)
	}
	if providerErrorBody(t, out)["message"] != http.StatusText(500) {
		t.Fatal("garbage body used as a message")
	}
}

func TestProviderErrorRedactsCredentials(t *testing.T) {
	account := storedAccount{
		AccessToken:  "access token+1/2",
		RefreshToken: "refresh-token-ABCDEF",
		IDToken:      "id-token-GHIJKL",
		ClientSecret: "client-secret-MNOPQR",
	}
	message := "provider rejected " + account.AccessToken + " and " + account.RefreshToken +
		" id " + account.IDToken + " secret " + account.ClientSecret +
		" plus Bearer abcdef123456 and sk-live-1234567890 and sk_test_1234567890 and vr_key_1234567890" +
		" encoded " + url.QueryEscape(account.AccessToken) +
		" hex " + hex.EncodeToString([]byte(account.RefreshToken))
	raw, err := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
	if err != nil {
		t.Fatal(err)
	}
	out := readProviderError(providerResponse(401, string(raw), nil), account)
	text, _ := providerErrorBody(t, out)["message"].(string)
	for _, secret := range []string{
		account.AccessToken, account.RefreshToken, account.IDToken, account.ClientSecret,
		"abcdef123456", "sk-live-1234567890", "sk_test_1234567890", "vr_key_1234567890",
		url.QueryEscape(account.AccessToken), hex.EncodeToString([]byte(account.RefreshToken)),
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("secret leaked: %q in %q", secret, text)
		}
	}
	if !strings.Contains(text, providerRedacted) {
		t.Fatalf("no redaction marker: %q", text)
	}
	if !strings.Contains(text, "Bearer "+providerRedacted) {
		t.Fatalf("bearer token not masked: %q", text)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access token", account.RefreshToken, account.IDToken, account.ClientSecret} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("response leaked credential: %q", secret)
		}
	}
}

func TestProviderErrorRejectsUnsafeMessages(t *testing.T) {
	for _, message := range []string{
		"<div>internal error</div>",
		"goroutine 1 [running]:\nmain.main()\n\t/app/main.go:42 +0x1e",
		"java.lang.RuntimeException: boom\n\tat com.example.Main.main(Main.java:10)",
	} {
		raw, err := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
		if err != nil {
			t.Fatal(err)
		}
		out := readProviderError(providerResponse(500, string(raw), nil), storedAccount{})
		if providerErrorBody(t, out)["message"] != http.StatusText(500) {
			t.Fatalf("unsafe message returned: %#v", out)
		}
	}
}

func TestProviderErrorRequestIDValidated(t *testing.T) {
	for _, tc := range []struct {
		header, want string
	}{
		{"req_valid-1", "req_valid-1"},
		{" short ", "short"},
		{`"><script>`, ""},
		{strings.Repeat("a", 129), ""},
	} {
		out := readProviderError(providerResponse(500, `{"error":"boom"}`, map[string]string{"x-request-id": tc.header}), storedAccount{})
		if tc.want == "" {
			if _, exists := out["request_id"]; exists {
				t.Fatalf("unsafe request id accepted: %q", tc.header)
			}
			continue
		}
		if out["request_id"] != tc.want {
			t.Fatalf("request id %q: %#v", tc.header, out["request_id"])
		}
	}
	// Header request IDs stay available when the body cannot be used.
	out := readProviderError(providerResponse(502, "<html>bad gateway</html>", map[string]string{"request-id": "req_from_header"}), storedAccount{})
	if out["request_id"] != "req_from_header" {
		t.Fatalf("fallback request id: %#v", out)
	}
}

func TestProviderErrorOptionalFieldsDoNotEchoCredentials(t *testing.T) {
	cases := []struct {
		name    string
		account storedAccount
		body    string
		headers map[string]string
		check   func(t *testing.T, out map[string]any)
	}{
		{
			name:    "access token in type",
			account: storedAccount{AccessToken: "short-access"},
			body:    `{"error":{"type":"short-access","message":"boom"}}`,
			check: func(t *testing.T, out map[string]any) {
				if providerErrorBody(t, out)["type"] != providerErrorType {
					t.Fatalf("credential echoed as type: %#v", out)
				}
			},
		},
		{
			name:    "refresh token in code",
			account: storedAccount{RefreshToken: "short-refresh"},
			body:    `{"error":{"message":"boom","code":"short-refresh"}}`,
			check: func(t *testing.T, out map[string]any) {
				if _, exists := providerErrorBody(t, out)["code"]; exists {
					t.Fatalf("credential echoed as code: %#v", out)
				}
			},
		},
		{
			name:    "id token in request id",
			account: storedAccount{IDToken: "short-id"},
			body:    `{"error":"boom"}`,
			headers: map[string]string{"x-request-id": "short-id"},
			check: func(t *testing.T, out map[string]any) {
				if _, exists := out["request_id"]; exists {
					t.Fatalf("credential echoed as request id: %#v", out)
				}
			},
		},
		{
			name:    "client secret embedded in code",
			account: storedAccount{ClientSecret: "short-secret"},
			body:    `{"error":{"message":"boom","code":"wrap_short-secret_x"}}`,
			check: func(t *testing.T, out map[string]any) {
				if _, exists := providerErrorBody(t, out)["code"]; exists {
					t.Fatalf("embedded credential echoed as code: %#v", out)
				}
			},
		},
		{
			name:    "tainted header falls through to a safe header",
			account: storedAccount{AccessToken: "short-access"},
			body:    `{"error":"boom"}`,
			headers: map[string]string{"x-request-id": "short-access", "request-id": "req_safe-1"},
			check: func(t *testing.T, out map[string]any) {
				if out["request_id"] != "req_safe-1" {
					t.Fatalf("safe header not used: %#v", out)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := readProviderError(providerResponse(400, tc.body, tc.headers), tc.account)
			tc.check(t, out)
			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{tc.account.AccessToken, tc.account.RefreshToken, tc.account.IDToken, tc.account.ClientSecret} {
				if secret != "" && strings.Contains(string(encoded), secret) {
					t.Fatalf("credential %q leaked: %s", secret, encoded)
				}
			}
		})
	}
}
