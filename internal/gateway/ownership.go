package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Account scopes. A shared account serves everyone's requests; a personal
// account serves only the user who connected it.
const (
	scopeGlobal   = "global"
	scopePersonal = "personal"
)

// Routing orders. The tier named first serves every request it can; the
// other tier takes over when the first has no usable account for the model.
const (
	routingPersonalFirst = "personal-first"
	routingGlobalFirst   = "global-first"
)

var errAccountOwned = errors.New("gateway: provider account is connected under another owner")

func accountScope(a storedAccount) string {
	if a.OwnerID != "" {
		return scopePersonal
	}
	return scopeGlobal
}

// accountVisible reports whether a user's requests may use an account and
// the user may see it. Nobody sees another user's personal accounts, admins
// included.
func accountVisible(a storedAccount, userID string) bool {
	return a.OwnerID == "" || a.OwnerID == userID
}

// accountManageable reports whether a user may reconnect, disable or remove
// an account: admins manage shared accounts, and owners their own.
func accountManageable(a storedAccount, user userRecord) bool {
	if a.OwnerID == "" {
		return user.admin()
	}
	return a.OwnerID == user.ID
}

// accountScope is the scope of the account that served the attempt, or ""
// when none did.
func (a *inferenceAttempt) accountScope() string {
	if a.account.ID == "" {
		return ""
	}
	return accountScope(a.account)
}

// routingOrder lists candidates in the order requests try them: the preferred
// tier first, then the other, each rotated so load spreads across its
// accounts.
func routingOrder(candidates []storedAccount, routing string, turn uint64) []storedAccount {
	first := scopePersonal
	if routing == routingGlobalFirst {
		first = scopeGlobal
	}
	var preferred, rest []storedAccount
	for _, a := range candidates {
		if accountScope(a) == first {
			preferred = append(preferred, a)
		} else {
			rest = append(rest, a)
		}
	}
	return append(rotateAccounts(preferred, turn), rotateAccounts(rest, turn)...)
}

func rotateAccounts(accounts []storedAccount, turn uint64) []storedAccount {
	if len(accounts) == 0 {
		return nil
	}
	start := int(turn % uint64(len(accounts))) //nolint:gosec // the remainder is below len(accounts)
	out := make([]storedAccount, 0, len(accounts))
	return append(append(out, accounts[start:]...), accounts[:start]...)
}

// validateOwners requires every personal account to belong to a known user.
func validateOwners(accounts []storedAccount, users []userRecord) error {
	known := make(map[string]struct{}, len(users))
	for _, user := range users {
		known[user.ID] = struct{}{}
	}
	for i, a := range accounts {
		if _, ok := known[a.OwnerID]; a.OwnerID != "" && !ok {
			return fmt.Errorf("gateway: account %d belongs to an unknown user", i)
		}
	}
	return nil
}

// settingsView holds the gateway-wide settings every user may read.
type settingsView struct {
	Routing string `json:"routing"`
}

func (s *server) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, settingsView{Routing: s.store.snapshot().Routing})
}

// putSettings changes the gateway-wide settings. Only admins reach it.
func (s *server) putSettings(w http.ResponseWriter, r *http.Request) {
	var input settingsView
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil || (input.Routing != routingPersonalFirst && input.Routing != routingGlobalFirst) {
		writeJSON(w, 400, map[string]string{"error": "Choose personal-first or global-first routing"})
		return
	}
	if err := s.store.update(func(d *diskState) error { d.Routing = input.Routing; return nil }); err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not save the settings"})
		return
	}
	writeJSON(w, 200, input)
}
