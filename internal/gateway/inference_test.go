package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeInferenceSelectsEntitledAccountAndStripsCredentials(t *testing.T) {
	s := oauthServer(t)
	nativeSeed(t, s, nativeAccount("wrong-model"), nativeAccount("eligible"))
	var sent atomic.Int32
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			if r.Header.Get("Authorization") == "Bearer provider-eligible" {
				return oauthReply(200, nativeModels("wanted")), nil
			}
			return oauthReply(200, nativeModels("other")), nil
		}
		sent.Add(1)
		if r.URL.String() != "https://chatgpt.com/backend-api/codex/responses" || r.Header.Get("Authorization") != "Bearer provider-eligible" {
			t.Error("wrong provider destination or account")
		}
		for _, header := range []string{"Cookie", "X-Api-Key", "X-Management-Key", "ChatGPT-Account-ID", "OpenAI-Organization"} {
			if r.Header.Get(header) != "" {
				t.Errorf("forwarded client header %s", header)
			}
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["store"] != false || body["stream"] != true {
			t.Error("wrong subscription request flags")
		}
		if _, ok := body["input"].([]any); !ok {
			t.Error("string input not normalized")
		}
		return nativeSSE("data: done\n\n"), nil
	}))
	for _, payload := range []string{`{"model":"wanted"}`, `{"model":"wanted","stream":false}`, `{"model":"wanted","stream":true,"store":true}`} {
		if w := nativeCall(s, "POST", "/v1/responses", "client", payload); w.Code != 400 {
			t.Fatalf("accepted invalid flags %s", payload)
		}
	}
	r := httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"wanted","input":"hello","stream":true}`))
	r.Header.Set("Authorization", "Bearer client")
	r.Header.Set("X-Api-Key", "client")
	r.Header.Set("Cookie", "admin=secret")
	r.Header.Set("X-Management-Key", "private")
	r.Header.Set("ChatGPT-Account-ID", "malicious")
	r.Header.Set("OpenAI-Organization", "malicious")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || sent.Load() != 1 {
		t.Fatalf("inference %d %s", w.Code, w.Body)
	}
}
func TestNativeClaudeMessagesAndCatalog(t *testing.T) {
	s := oauthServer(t)
	a := nativeAccount("claude")
	a.Provider = "claude"
	a.AuthMode = "oauth"
	nativeSeed(t, s, a)
	nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer provider-claude" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") == "" {
			t.Error("missing Claude authentication")
		}
		if r.Method == "GET" {
			if r.URL.Path == "/api/oauth/usage" {
				return oauthReply(200, map[string]any{"five_hour": map[string]any{"utilization": 20}}), nil
			}
			if r.URL.String() == claudeProfileURL {
				return oauthReply(200, map[string]any{}), nil
			}
			if r.URL.Host != "api.anthropic.com" || r.URL.Path != "/v1/models" {
				t.Error("wrong Claude catalog")
			}
			return oauthReply(200, map[string]any{"data": []map[string]string{{"id": "claude-model", "display_name": "Claude model"}}}), nil
		}
		if r.URL.String() != "https://api.anthropic.com/v1/messages" {
			t.Error("wrong messages endpoint")
		}
		return oauthReply(200, map[string]any{"type": "message", "content": []any{}}), nil
	}))
	w := nativeCall(s, "GET", "/v1/models", "client", "")
	if !strings.Contains(w.Body.String(), "claude-model") {
		t.Fatal(w.Body)
	}
	w = nativeCall(s, "POST", "/v1/messages", "client", `{"model":"claude-model","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
}
func TestNativeBoundedFailoverAndNoPaidFallback(t *testing.T) {
	for _, status := range []int{429, 503, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := oauthServer(t)
			a, b, paid := nativeAccount("a"), nativeAccount("b"), nativeAccount("paid")
			paid.AuthMode = "api_key"
			nativeSeed(t, s, a, b, paid)
			var sent atomic.Int32
			nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" {
					if r.Header.Get("Authorization") == "Bearer provider-paid" {
						return oauthReply(200, map[string]any{"data": []map[string]string{{"id": "model"}}}), nil
					}
					return oauthReply(200, nativeModels("model")), nil
				}
				if r.Header.Get("Authorization") == "Bearer provider-paid" {
					t.Error("fell back to paid account")
				}
				sent.Add(1)
				response := oauthReply(status, map[string]string{"private": "provider diagnostics"})
				response.Header.Set("Location", "https://attacker.invalid")
				return response, nil
			}))
			w := nativeCall(s, "POST", "/v1/responses", "client", `{"model":"model","stream":true}`)
			wantCalls := int32(2)
			wantStatus := status
			if status == 302 {
				wantCalls = 1
				wantStatus = 502
			}
			if sent.Load() != wantCalls || w.Code != wantStatus || strings.Contains(w.Body.String(), "diagnostics") || w.Header().Get("Location") != "" {
				t.Fatalf("failover %d calls %d: %s", w.Code, sent.Load(), w.Body)
			}
		})
	}
}
func TestNativeStreamingFlushTerminalErrorAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal error", true: "disconnect"}[cancel], func(t *testing.T) {
			s := oauthServer(t)
			nativeSeed(t, s, nativeAccount("a"), nativeAccount("b"))
			release := make(chan struct{})
			cancelled := make(chan struct{})
			var calls atomic.Int32
			nativeMock(s, oauthTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" {
					return oauthReply(200, nativeModels("model")), nil
				}
				calls.Add(1)
				reader, writer := io.Pipe()
				go func() {
					defer writer.Close()
					io.WriteString(writer, "data: first\n\n")
					select {
					case <-release:
						io.WriteString(writer, "data: {\"type\":\"response.failed\"}\n\n")
					case <-r.Context().Done():
						close(cancelled)
					}
				}()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
			}))
			host := httptest.NewServer(s)
			defer host.Close()
			req, _ := http.NewRequest("POST", host.URL+"/v1/responses", strings.NewReader(`{"model":"model","stream":true}`))
			req.Header.Set("Authorization", "Bearer client")
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				close(release)
				t.Fatal(err)
			}
			reader := bufio.NewReader(resp.Body)
			first, err := reader.ReadString('\n')
			if err != nil || first != "data: first\n" {
				close(release)
				resp.Body.Close()
				t.Fatalf("stream buffered: %q %v", first, err)
			}
			if cancel {
				resp.Body.Close()
				select {
				case <-cancelled:
				case <-time.After(time.Second):
					close(release)
					t.Fatal("provider request did not cancel")
				}
				close(release)
			} else {
				close(release)
				rest, err := io.ReadAll(reader)
				resp.Body.Close()
				if err != nil || !strings.Contains(string(rest), "response.failed") {
					t.Fatal("terminal provider error lost")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("request retried after streaming began")
			}
		})
	}
}
