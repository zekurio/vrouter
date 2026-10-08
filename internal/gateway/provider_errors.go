package gateway

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// providerErrorReadMax bounds how much of a failed provider body is read.
	providerErrorReadMax = 64 << 10
	// providerErrorMessageMax bounds the message sent to the client.
	providerErrorMessageMax = 4096
	providerRedacted        = "[redacted]"
	providerErrorType       = "provider_error"
	providerGenericMessage  = "Provider request failed"
)

var (
	providerTagPattern        = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	providerRequestIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._:+/=@-]{1,128}$`)
	providerStackFramePattern = regexp.MustCompile(`(?im)^\s*at [A-Za-z0-9_.$<>/\\-]+\(`)
	providerGoFramePattern    = regexp.MustCompile(`\.go:\d+`)
	// providerTokenPatterns mask token-shaped strings that are not tied to one
	// stored account, such as a key quoted in a provider message.
	providerTokenPatterns = []struct {
		pattern     *regexp.Regexp
		replacement string
	}{
		{regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=%-]{6,}`), `${1} ` + providerRedacted},
		{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{4,}`), "sk-" + providerRedacted},
		{regexp.MustCompile(`\bsk_[A-Za-z0-9_-]{4,}`), "sk_" + providerRedacted},
		{regexp.MustCompile(`\bvr_[A-Za-z0-9_-]{4,}`), "vr_" + providerRedacted},
	}
)

// readProviderError turns a failed provider response into the JSON document
// sent to the client, shaped as
//
//	{"error":{"type":...,"message":...,"code":...},"request_id":...}
//
// where code and request_id appear only when known. The caller invokes this
// before closing resp.Body and writes the result with the provider status.
//
// The body is read to at most 64 KiB and parsed as JSON only. The accepted
// shapes are {"error":{"type","message","code"}}, {"error":"message"}, and
// the same fields at the top level. Anything else - HTML, malformed JSON,
// oversized bodies - falls back to the HTTP status text, so a raw body or
// stack trace is never echoed. The message is capped at 4096 characters.
//
// Credentials stored on the account, token-shaped Bearer/sk-/sk_/vr_ strings,
// and common encoded forms of both are replaced before the message leaves
// this function. The request ID is taken from a small set of response headers
// and validated. Nothing is logged.
func readProviderError(resp *http.Response, a storedAccount) map[string]any {
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	fallback := http.StatusText(status)
	if fallback == "" {
		fallback = providerGenericMessage
	}
	message, typeName, code := "", "", ""
	if resp != nil && resp.Body != nil {
		rawMessage, rawType, rawCode := providerErrorFields(resp.Body)
		typeName = providerErrorTag(rawType, true, a)
		code = providerErrorTag(rawCode, false, a)
		if sanitized, ok := sanitizeProviderMessage(rawMessage, a); ok {
			message = sanitized
		}
	}
	if message == "" {
		message = fallback
	}
	if typeName == "" {
		typeName = providerErrorType
	}
	body := map[string]any{"type": typeName, "message": message}
	if code != "" {
		body["code"] = code
	}
	out := map[string]any{"error": body}
	if id := providerRequestID(resp, a); id != "" {
		out["request_id"] = id
	}
	return out
}

// providerErrorFields extracts the error fields from a bounded, JSON-only
// body. It returns empty strings when nothing usable parses.
func providerErrorFields(reader io.Reader) (message, typeName, code string) {
	raw, err := io.ReadAll(io.LimitReader(reader, providerErrorReadMax+1))
	if err != nil || len(raw) == 0 || len(raw) > providerErrorReadMax {
		return "", "", ""
	}
	var top struct {
		Error   json.RawMessage `json:"error"`
		Type    json.RawMessage `json:"type"`
		Message json.RawMessage `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(raw, &top) != nil {
		return "", "", ""
	}
	if len(top.Error) > 0 && string(top.Error) != "null" {
		var text string
		if json.Unmarshal(top.Error, &text) == nil {
			return strings.TrimSpace(text), "", ""
		}
		var nested struct {
			Type    json.RawMessage `json:"type"`
			Message json.RawMessage `json:"message"`
			Code    json.RawMessage `json:"code"`
		}
		if json.Unmarshal(top.Error, &nested) != nil {
			return "", "", ""
		}
		return providerRawString(nested.Message), providerRawString(nested.Type), providerRawCode(nested.Code)
	}
	return providerRawString(top.Message), providerRawString(top.Type), providerRawCode(top.Code)
}

func providerRawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return strings.TrimSpace(text)
}

// providerRawCode accepts a string or a number code. Other types are dropped.
func providerRawCode(raw json.RawMessage) string {
	if text := providerRawString(raw); text != "" {
		return text
	}
	var number json.Number
	if len(raw) == 0 || json.Unmarshal(raw, &number) != nil {
		return ""
	}
	return number.String()
}

// providerErrorTag keeps a provider type or code only when it is a short,
// printable token that does not echo a stored credential. The generic type
// "error" is replaced by provider_error.
func providerErrorTag(raw string, isType bool, a storedAccount) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !providerTagPattern.MatchString(raw) {
		return ""
	}
	if providerFieldContainsSecret(raw, a) {
		return ""
	}
	if isType && strings.EqualFold(raw, "error") {
		return ""
	}
	return raw
}

// sanitizeProviderMessage trims, flattens, redacts, and caps a provider
// message. The boolean is false when the text is empty or looks like HTML or
// a stack trace, in which case the caller uses the status fallback.
func sanitizeProviderMessage(raw string, a storedAccount) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || providerMessageUnsafe(raw) {
		return "", false
	}
	message := strings.Join(strings.Fields(raw), " ")
	if message == "" {
		return "", false
	}
	message = redactProviderSecrets(message, a)
	if runes := []rune(message); len(runes) > providerErrorMessageMax {
		message = string(runes[:providerErrorMessageMax])
	}
	if message == "" {
		return "", false
	}
	return message, true
}

// providerMessageUnsafe reports whether text must not be echoed: HTML,
// stack-trace shapes, or C0 control characters other than whitespace.
func providerMessageUnsafe(raw string) bool {
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<html") ||
		strings.Contains(lower, "<body") || strings.Contains(lower, "<script") ||
		strings.Contains(lower, "<div") || strings.Contains(lower, "</") {
		return true
	}
	if strings.Contains(lower, "traceback (most recent call last)") ||
		strings.Contains(lower, "goroutine ") || strings.Contains(lower, "panic:") ||
		providerStackFramePattern.MatchString(raw) || providerGoFramePattern.MatchString(raw) {
		return true
	}
	return strings.ContainsFunc(raw, func(r rune) bool {
		return r < 0x20 && r != '\t' && r != '\n' && r != '\r'
	})
}

// redactProviderSecrets replaces every stored credential and token-shaped
// string. Encoded forms of a stored credential are replaced too, so a message
// that quotes a URL, a JSON string, or a base64 re-encoding cannot leak it.
func redactProviderSecrets(message string, a storedAccount) string {
	for _, variant := range providerSecretVariants(a) {
		message = strings.ReplaceAll(message, variant, providerRedacted)
	}
	for _, pattern := range providerTokenPatterns {
		message = pattern.pattern.ReplaceAllString(message, pattern.replacement)
	}
	return message
}

// providerFieldContainsSecret reports whether a short metadata value would
// echo a stored credential. Such values are dropped instead of sent, because
// type, code, and request_id have no safe redacted form.
func providerFieldContainsSecret(value string, a storedAccount) bool {
	for _, variant := range providerSecretVariants(a) {
		if strings.Contains(value, variant) {
			return true
		}
	}
	return false
}

// providerSecretVariants returns the raw credentials plus the encodings a
// message is most likely to quote, longest first so a shorter variant cannot
// split a longer match.
func providerSecretVariants(a storedAccount) []string {
	variants := map[string]struct{}{}
	for _, secret := range []string{a.AccessToken, a.RefreshToken, a.IDToken, a.ClientSecret} {
		addProviderSecretVariants(variants, secret)
	}
	ordered := make([]string, 0, len(variants))
	for variant := range variants {
		ordered = append(ordered, variant)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	return ordered
}

// addProviderSecretVariants registers the raw credential and the encodings a
// message is most likely to quote: URL percent-encoding, JSON string escapes,
// hex, and the four base64 alphabets.
func addProviderSecretVariants(variants map[string]struct{}, secret string) {
	if secret == "" {
		return
	}
	quoted := strconv.Quote(secret)
	if len(quoted) >= 2 {
		quoted = quoted[1 : len(quoted)-1]
	}
	for _, variant := range []string{
		secret,
		strings.TrimSpace(secret),
		url.QueryEscape(secret),
		url.PathEscape(secret),
		quoted,
		hex.EncodeToString([]byte(secret)),
		base64.StdEncoding.EncodeToString([]byte(secret)),
		base64.RawStdEncoding.EncodeToString([]byte(secret)),
		base64.URLEncoding.EncodeToString([]byte(secret)),
		base64.RawURLEncoding.EncodeToString([]byte(secret)),
	} {
		if variant != "" {
			variants[variant] = struct{}{}
		}
	}
}

// providerRequestID returns a validated request ID from the response headers.
// A value that would echo a stored credential is skipped, and body-derived
// IDs are never used.
func providerRequestID(resp *http.Response, a storedAccount) string {
	if resp == nil {
		return ""
	}
	for _, name := range []string{"X-Request-Id", "Request-Id", "X-Amzn-Requestid", "X-Ms-Request-Id"} {
		value := strings.TrimSpace(resp.Header.Get(name))
		if providerRequestIDPattern.MatchString(value) && !providerFieldContainsSecret(value, a) {
			return value
		}
	}
	return ""
}
