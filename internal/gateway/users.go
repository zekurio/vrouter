package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Roles a user can hold. The identity provider's vrouter_role claim decides
// the role at every sign-in; vrouter has no way to change it.
const (
	roleAdmin  = "admin"
	roleMember = "member"
)

var (
	errUserNotFound = errors.New("gateway: user not found")
	errUserDisabled = errors.New("gateway: user is disabled")
)

// userRecord is a person who signed in through the identity provider. The
// issuer and subject identify them; email and name follow the latest sign-in.
type userRecord struct {
	ID         string     `json:"id"`
	Issuer     string     `json:"issuer"`
	Subject    string     `json:"subject"`
	Email      string     `json:"email,omitempty"`
	Name       string     `json:"name,omitempty"`
	Picture    string     `json:"picture,omitempty"`
	Role       string     `json:"role"`
	CreatedAt  time.Time  `json:"createdAt"`
	SignedInAt time.Time  `json:"signedInAt"`
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
}

func (u userRecord) admin() bool { return u.Role == roleAdmin }

// label names the user in lists other people see.
func (u userRecord) label() string {
	switch {
	case u.Name != "":
		return u.Name
	case u.Email != "":
		return u.Email
	}
	return "Unnamed user"
}

func findUser(state *diskRegistry, id string) *userRecord {
	for i := range state.Users {
		if state.Users[i].ID == id {
			return &state.Users[i]
		}
	}
	return nil
}

// activeUser returns a user who exists and is not disabled.
func (s *server) activeUser(id string) (userRecord, bool) {
	state := s.registry.snapshot()
	user := findUser(&state, id)
	if user == nil || user.DisabledAt != nil {
		return userRecord{}, false
	}
	return *user, true
}

// userLabels names every user by ID, for lists that show owners.
func (s *server) userLabels() map[string]string {
	labels := map[string]string{}
	for _, user := range s.registry.snapshot().Users {
		labels[user.ID] = user.label()
	}
	return labels
}

func registryValidateUsers(users []userRecord) (map[string]struct{}, error) {
	seen := make(map[string]struct{}, len(users))
	identities := make(map[string]struct{}, len(users))
	for i := range users {
		user := &users[i]
		if !recordIDPattern.MatchString(user.ID) {
			return nil, fmt.Errorf("gateway: user %d has an invalid ID", i)
		}
		if _, duplicate := seen[user.ID]; duplicate {
			return nil, errors.New("gateway: user IDs must be unique")
		}
		seen[user.ID] = struct{}{}
		if user.Issuer == "" || user.Subject == "" || len(user.Issuer) > 512 || len(user.Subject) > 512 {
			return nil, fmt.Errorf("gateway: user %d has an invalid identity", i)
		}
		identity := user.Issuer + "\x00" + user.Subject
		if _, duplicate := identities[identity]; duplicate {
			return nil, errors.New("gateway: user identities must be unique")
		}
		identities[identity] = struct{}{}
		if user.Role != roleAdmin && user.Role != roleMember {
			return nil, fmt.Errorf("gateway: user %d has an invalid role", i)
		}
		if len(user.Email) > 320 || len(user.Name) > 256 || len(user.Picture) > maxPictureURL {
			return nil, fmt.Errorf("gateway: user %d has an oversized profile", i)
		}
	}
	return seen, nil
}

// oidcIdentity is what a verified sign-in says about a person.
type oidcIdentity struct {
	issuer, subject, email, name, picture string
}

const maxPictureURL = 2048

// pictureURL keeps a profile picture address the browser can load: an
// absolute http or https URL of bounded length. Anything else becomes "",
// and the dashboard shows initials instead.
func pictureURL(value string) string {
	u, err := url.Parse(value)
	if err != nil || len(value) > maxPictureURL || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return ""
	}
	return value
}

// signIn records a verified sign-in and returns the user. An administrator's
// sign-in adopts the keys no user owns, which only keys from before users
// existed can be.
func (s *server) signIn(ctx context.Context, identity oidcIdentity, role string) (userRecord, error) {
	var user userRecord
	now := time.Now().UTC()
	err := s.registry.update(func(state *diskRegistry) error {
		var record *userRecord
		for i := range state.Users {
			if state.Users[i].Issuer == identity.issuer && state.Users[i].Subject == identity.subject {
				record = &state.Users[i]
				break
			}
		}
		if record == nil {
			id, err := secureID()
			if err != nil {
				return err
			}
			state.Users = append(state.Users, userRecord{ID: id, Issuer: identity.issuer, Subject: identity.subject, CreatedAt: now})
			record = &state.Users[len(state.Users)-1]
		}
		if record.DisabledAt != nil {
			return errUserDisabled
		}
		record.Email = boundedText(identity.email, 320)
		record.Name = boundedText(identity.name, 256)
		record.Picture = identity.picture
		record.Role = role
		record.SignedInAt = now
		if role == roleAdmin {
			for i := range state.Keys {
				if state.Keys[i].UserID == "" {
					state.Keys[i].UserID = record.ID
				}
			}
		}
		user = *record
		return nil
	})
	if err != nil {
		return userRecord{}, err
	}
	if user.admin() {
		if err := s.store.data.usage.adoptUnowned(ctx, user.ID); err != nil {
			// The keys already moved. Their older history moves at the next
			// administrator sign-in.
			slog.Warn("usage history of unowned keys not adopted", "error", err)
		}
	}
	return user, nil
}

// userView is a user as the Users page shows them.
type userView struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	CreatedAt  time.Time  `json:"createdAt"`
	SignedInAt time.Time  `json:"signedInAt"`
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
	Keys       int        `json:"keys"`
}

func (s *server) listUsers(w http.ResponseWriter, _ *http.Request) {
	state := s.registry.snapshot()
	keys := map[string]int{}
	now := time.Now()
	for _, key := range state.Keys {
		if key.RevokedAt == nil && !key.expired(now) {
			keys[key.UserID]++
		}
	}
	users := make([]userView, 0, len(state.Users))
	for _, u := range state.Users {
		users = append(users, userView{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, CreatedAt: u.CreatedAt, SignedInAt: u.SignedInAt, DisabledAt: u.DisabledAt, Keys: keys[u.ID]})
	}
	sort.SliceStable(users, func(i, j int) bool {
		return strings.ToLower(users[i].Name+users[i].Email) < strings.ToLower(users[j].Name+users[j].Email)
	})
	writeJSON(w, 200, map[string]any{"users": users})
}

// patchUser disables or re-enables a user. Disabling ends their sessions and
// stops their keys; their personal accounts stay stored but unused.
func (s *server) patchUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Disabled *bool `json:"disabled"`
	}
	if decodeKeyInput(w, r, &input) != nil || input.Disabled == nil {
		writeJSON(w, 400, map[string]string{"error": "Choose whether the user is disabled"})
		return
	}
	id := r.PathValue("id")
	if id == currentUser(r).ID {
		writeJSON(w, 400, map[string]string{"error": "You can't disable yourself"})
		return
	}
	now := time.Now().UTC()
	var updated userRecord
	err := s.registry.update(func(state *diskRegistry) error {
		user := findUser(state, id)
		if user == nil {
			return errUserNotFound
		}
		user.DisabledAt = nil
		if *input.Disabled {
			user.DisabledAt = &now
		}
		updated = *user
		return nil
	})
	if errors.Is(err, errUserNotFound) {
		writeJSON(w, 404, map[string]string{"error": "User not found"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not save the user"})
		return
	}
	if *input.Disabled {
		if err := s.store.data.usage.endUserSessions(r.Context(), id); err != nil {
			writeJSON(w, 500, map[string]string{"error": "The user is disabled, but their sessions could not be ended"})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"user": userView{ID: updated.ID, Email: updated.Email, Name: updated.Name, Role: updated.Role, CreatedAt: updated.CreatedAt, SignedInAt: updated.SignedInAt, DisabledAt: updated.DisabledAt}})
}

type userContextKey struct{}

// currentUser is the signed-in user authorize attached to the request.
func currentUser(r *http.Request) userRecord {
	user, _ := r.Context().Value(userContextKey{}).(userRecord)
	return user
}

// sessionView is the signed-in user as the dashboard needs them.
func sessionView(u userRecord) map[string]any {
	return map[string]any{"user": map[string]string{"id": u.ID, "name": u.Name, "email": u.Email, "picture": pictureURL(u.Picture), "role": u.Role}}
}
