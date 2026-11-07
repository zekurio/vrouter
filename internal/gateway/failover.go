package gateway

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

const (
	// accountTroubleCooldown is how long an account that failed on its own
	// goes to the back of the routing order.
	accountTroubleCooldown = 30 * time.Second
	// accountTroubleMax bounds a cooldown a provider's Retry-After asks for.
	accountTroubleMax = 5 * time.Minute
)

// failoverStatus reports whether a provider status says the account failed
// rather than the request, so another account may serve it: a rate limit,
// refused credentials, a model this account cannot reach, a timeout, or a
// provider failure. Anthropic reports overload as 529.
func failoverStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

// noteTrouble sends an account that just failed on its own to the back of
// the routing order for a while. A 404 names a model, not a broken account.
// A 401 also marks the access token for a refresh.
func (s *server) noteTrouble(a storedAccount, resp *http.Response) {
	if resp != nil && (resp.StatusCode == http.StatusNotFound || !failoverStatus(resp.StatusCode)) {
		return
	}
	cooldown := accountTroubleCooldown
	if resp != nil {
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			cooldown = min(max(time.Duration(seconds)*time.Second, cooldown), accountTroubleMax)
		}
	}
	s.troubleMu.Lock()
	s.trouble[a.ID] = time.Now().Add(cooldown)
	// The token is known by its expiry, which a refresh changes. An API key
	// cannot refresh.
	if resp != nil && resp.StatusCode == http.StatusUnauthorized && a.AuthMode != "api_key" && a.RefreshToken != "" {
		s.rejected[a.ID] = a.ExpiresAt
	}
	s.troubleMu.Unlock()
}

func (s *server) clearTrouble(id string) {
	s.troubleMu.Lock()
	delete(s.trouble, id)
	s.troubleMu.Unlock()
}

// tokenRejected reports whether the provider refused the account's current
// access token, so the next request refreshes it instead of sending it again.
func (s *server) tokenRejected(a storedAccount) bool {
	s.troubleMu.Lock()
	defer s.troubleMu.Unlock()
	expiry, ok := s.rejected[a.ID]
	if ok && !expiry.Equal(a.ExpiresAt) {
		delete(s.rejected, a.ID)
		return false
	}
	return ok
}

// healthyFirst moves accounts that failed recently behind the others. Each
// group keeps its routing order.
func (s *server) healthyFirst(candidates []storedAccount) []storedAccount {
	now := time.Now()
	healthy := make([]storedAccount, 0, len(candidates))
	var troubled []storedAccount
	s.troubleMu.Lock()
	for _, a := range candidates {
		until, ok := s.trouble[a.ID]
		switch {
		case ok && now.Before(until):
			troubled = append(troubled, a)
		case ok:
			delete(s.trouble, a.ID)
			healthy = append(healthy, a)
		default:
			healthy = append(healthy, a)
		}
	}
	s.troubleMu.Unlock()
	return append(healthy, troubled...)
}

// forgetAccountState drops what vrouter learned about an account's previous
// connection. The caller holds refreshMu.
func (s *server) forgetAccountState(id string) {
	s.forgetCatalog(id)
	s.troubleMu.Lock()
	delete(s.trouble, id)
	delete(s.rejected, id)
	s.troubleMu.Unlock()
	delete(s.refreshFailures, id)
}

// forwardError is a failure kept while other accounts are tried. It is the
// response when none of them answers.
type forwardError struct {
	status     int
	body       any
	retryAfter string
}

func (e *forwardError) write(w http.ResponseWriter) {
	if e.retryAfter != "" {
		w.Header().Set("Retry-After", e.retryAfter)
	}
	writeJSON(w, e.status, e.body)
}

// connectionFailure describes a provider request that got no response.
func connectionFailure(err error) *forwardError {
	message := "Provider connection failed"
	if networkError, ok := errors.AsType[net.Error](err); ok && networkError.Timeout() {
		message = "Provider did not answer in time"
	}
	return &forwardError{status: http.StatusBadGateway, body: map[string]string{"error": message}}
}

// forwardPlan says how one kind of request reaches an account.
type forwardPlan struct {
	client *http.Client
	// request builds the provider request for an account. invalid is the
	// error when it cannot.
	request func(a storedAccount) (*http.Request, error)
	invalid string
	// deliver relays a successful response.
	deliver func(resp *http.Response, a storedAccount)
	// keep reads a failure that passes the request on, and closes the body.
	keep func(resp *http.Response, a storedAccount) *forwardError
	// fail answers the last account's failure. It returns the accounts to try
	// again, if any.
	fail func(resp *http.Response, a storedAccount) []storedAccount
}

// tryAccounts sends a request to each candidate in turn until one serves it.
// An account that fails on its own passes the request to the next one and
// goes to the back of the routing order for a while. A client that leaves
// cuts the attempt short. It returns what plan.fail returns.
func (s *server) tryAccounts(w http.ResponseWriter, r *http.Request, attempt *inferenceAttempt, candidates []storedAccount, plan forwardPlan) []storedAccount {
	var kept *forwardError
	for i, candidate := range s.healthyFirst(candidates) {
		last := i == len(candidates)-1
		a, err := s.accessAccount(r.Context(), candidate.ID)
		if r.Context().Err() != nil {
			attempt.cutShort(reasonClientClosed)
			return nil
		}
		if err != nil || !routableAuth(a) {
			continue
		}
		req, err := plan.request(a)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": plan.invalid})
			return nil
		}
		attempt.provider, attempt.account = a.Provider, a
		resp, err := plan.client.Do(req) //nolint:bodyclose // deliver, keep, and fail close it
		switch {
		case err != nil:
			if r.Context().Err() != nil {
				attempt.cutShort(reasonClientClosed)
				return nil
			}
			s.noteTrouble(a, nil)
			kept = connectionFailure(err)
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			s.clearTrouble(a.ID)
			plan.deliver(resp, a)
			return nil
		case failoverStatus(resp.StatusCode) && !last:
			s.noteTrouble(a, resp)
			kept = plan.keep(resp, a)
		default:
			s.noteTrouble(a, resp)
			return plan.fail(resp, a)
		}
	}
	if kept != nil {
		kept.write(w)
		return nil
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "No usable account remains. Reconnect or enable an account."})
	return nil
}
