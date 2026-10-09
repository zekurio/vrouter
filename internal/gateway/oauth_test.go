package gateway

import "testing"

func TestOAuthReconnectRequiresSameClientAndIdentity(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			fresh := storedAccount{Provider: provider, AccountID: "account", Subject: "user", Email: "test@example.com"}
			if provider == "codex" {
				fresh.AuthMode, fresh.ClientID = "codex", codexNativeClientID
			} else {
				fresh.AuthMode, fresh.ClientID = "oauth", claudeClientID
			}
			if !oauthSameAccount(fresh, fresh) || oauthReconnectAllowed(fresh, fresh) != nil {
				t.Fatal("matching account cannot reconnect")
			}
			for _, client := range []string{"", "another-client"} {
				old := fresh
				old.ClientID = client
				if oauthSameAccount(old, fresh) || oauthReconnectAllowed(old, fresh) == nil {
					t.Fatal("account with a different client can reconnect")
				}
			}
			old := fresh
			old.AccountID = "another-account"
			if oauthSameAccount(old, fresh) || oauthReconnectAllowed(old, fresh) == nil {
				t.Fatal("account UUID mismatch was hidden by a matching email")
			}
			if provider == "codex" {
				old = fresh
				old.Subject = "another-user"
				if oauthSameAccount(old, fresh) || oauthReconnectAllowed(old, fresh) == nil {
					t.Fatal("another workspace member can replace the account")
				}
			}
		})
	}
}
