package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCancelledCatalogRequestIsNotCached(t *testing.T) {
	s := testServer(t, Config{})
	account := storedAccount{ID: "a", Provider: "codex", AuthMode: "api_key", AccessToken: "secret"}
	if err := s.store.update(func(d *diskState) error {
		d.Accounts = append(d.Accounts, account)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"m"}]}`))}, nil
	})}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.accountModels(cancelled, account); err == nil {
		t.Fatal("cancelled request succeeded")
	}
	models, err := s.accountModels(context.Background(), account)
	if err != nil || len(models) != 1 {
		t.Fatalf("models = %v, err = %v; a cancelled caller poisoned the cache", models, err)
	}
}
