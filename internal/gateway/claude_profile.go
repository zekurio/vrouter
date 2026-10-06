package gateway

import (
	"context"
	"strings"
	"time"
)

const claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"

type claudeProfile struct {
	Account struct {
		UUID   string `json:"uuid"`
		HasPro bool   `json:"has_claude_pro"`
		HasMax bool   `json:"has_claude_max"`
	} `json:"account"`
	Organization struct {
		Type string `json:"organization_type"`
		Tier string `json:"rate_limit_tier"`
	} `json:"organization"`
}

func (p claudeProfile) plan() string {
	switch strings.ToLower(p.Organization.Tier) {
	case "default_claude_max_5x":
		return "Max 5x"
	case "default_claude_max_20x":
		return "Max 20x"
	}
	switch strings.ToLower(p.Organization.Type) {
	case "claude_pro":
		return "Pro"
	case "claude_max":
		return "Max"
	case "claude_team":
		return "Team"
	case "claude_enterprise":
		return "Enterprise"
	}
	if p.Account.HasMax {
		return "Max"
	}
	if p.Account.HasPro {
		return "Pro"
	}
	return ""
}

// Claude's usage response has no subscription type. Fetch it independently so
// a profile failure never hides valid quota windows. The caller coalesces and
// caches this alongside the usage request.
func (s *server) claudePlan(ctx context.Context, a storedAccount) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var profile claudeProfile
	if s.providerJSON(ctx, a, claudeProfileURL, &profile) != nil {
		return ""
	}
	if a.AccountID != "" && profile.Account.UUID != a.AccountID {
		return ""
	}
	plan := profile.plan()
	if plan == "" {
		return ""
	}
	if plan == a.Plan {
		return plan
	}
	// Keep the last reported plan for disabled accounts and temporary profile
	// failures. Serialize with token refresh so an older account snapshot cannot
	// overwrite it, and only update the same saved identity.
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	_ = s.store.update(func(d *diskState) error {
		for i := range d.Accounts {
			current := &d.Accounts[i]
			if current.ID == a.ID && current.Provider == "claude" && current.AuthMode == "oauth" && current.AccountID == a.AccountID {
				current.Plan = plan
				break
			}
		}
		return nil
	})
	return plan
}
