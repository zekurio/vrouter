# vrouter

A small Go router with an embedded React management app. vrouter stores provider connections, refreshes credentials, discovers models, selects accounts, and forwards streaming requests itself. It runs as one binary and no longer needs CLIProxyAPI.

The backend is written in Go. Node is needed only to build the UI. Linux and macOS are supported; credential storage uses a Unix file lock to prevent concurrent refreshes from multiple processes.

## Build and run

Requires Go 1.26+, Node 22.12+ or Node 24, and npm. On Nix, run `nix develop` first.

```sh
make build
export VROUTER_API_KEY='your-client-secret'
./bin/vrouter
```

Open http://127.0.0.1:8080 and add an account. Use a different `VROUTER_ADMIN_TOKEN` if you want the workspace to require a token. The UI holds the admin token in memory, so reloading signs you out.

The local launcher creates or reuses a private client key and starts only vrouter:

```sh
node scripts/run-local.mjs
```

| Variable | Purpose |
| --- | --- |
| `VROUTER_ADDR` | Listen address; defaults to `127.0.0.1:8080` |
| `VROUTER_DATA_DIR` | Private state directory; defaults to `$XDG_STATE_HOME/vrouter` or `~/.local/state/vrouter` |
| `VROUTER_API_KEY` | Optional legacy inference key for the default gateway; additional keys are provisioned in the UI |
| `VROUTER_ADMIN_TOKEN` | Separate administrator token; a non-loopback listener requires this or configured OIDC providers |
| `VROUTER_PUBLIC_URL` | External origin for OIDC callbacks, for example `https://router.example.com` |
| `VROUTER_OAUTH_PROVIDERS` | JSON array of OIDC login providers and claim-to-role mappings |
| `VROUTER_DEMO` | Set to `1` for sample data with inference and account changes disabled |

The server does not load `.env` automatically. Export variables or configure your service manager. Keep a remote deployment behind HTTPS. Tokenless management checks both the peer address and Host header for loopback access; do not expose it through a proxy that rewrites both to local addresses.

## Users and gateways

Users sign in through an OpenID Connect provider and create gateways in the UI. Each gateway has its own connected provider accounts, model settings, API keys and request history. A `user` can manage only gateways they own. An `admin` can manage every gateway. Share inference access by creating a gateway API key; this does not grant gateway management access.

Without OIDC configuration, local access and the existing administrator token continue to work. Existing accounts and model settings remain in the default gateway. Turning on OIDC does not give ordinary users access to that gateway. The administrator token remains an administrator credential when OIDC is enabled.

Login providers are separate from the Codex and Claude connections that supply model access. Configure login providers at startup:

```sh
export VROUTER_PUBLIC_URL='https://router.example.com'
export ROUTER_OIDC_SECRET='your-provider-client-secret'
export VROUTER_OAUTH_PROVIDERS='[
  {
    "id": "company",
    "name": "Company sign-in",
    "issuer": "https://identity.example.com/realms/company",
    "clientId": "vrouter",
    "clientSecretEnv": "ROUTER_OIDC_SECRET",
    "scopes": ["openid", "profile", "email", "groups"],
    "roleMappings": [
      {"claim": "groups", "value": "router-admins", "role": "admin"},
      {"claim": "groups", "value": "router-users", "role": "user"}
    ]
  }
]'
```

Register `https://router.example.com/auth/callback/company` as the client's redirect URI. The issuer must support OIDC discovery. Choose scopes supported by your provider and configure it to include the mapped claims in ID tokens. `clientSecretEnv` names an environment variable containing the secret. Keep secret values out of configuration checked into source control.

Mappings match a string claim or a member of a string array exactly. A dot-separated claim path supports nested claims, for example `realm_access.roles`. At least one mapping must match for login to succeed. There is no implicit default role; an admin match takes precedence over a user match. Identity follows the verified issuer and subject, so two users with the same email address do not share ownership.

`VROUTER_PUBLIC_URL` must use HTTPS, except for a local HTTP development origin. Sign-in uses a browser-bound state value, PKCE and nonce verification. Session cookies are HttpOnly. Login sessions are held in memory, so a restart requires users to sign in again. Provider and role configuration changes take effect after a restart.

For a NixOS service, define the provider array with `builtins.toJSON` and load secrets at runtime with an environment file. This example assumes the built binary is installed at `/opt/vrouter/bin/vrouter`:

```nix
systemd.services.vrouter = {
  description = "vrouter model gateway";
  wantedBy = [ "multi-user.target" ];
  after = [ "network-online.target" ];
  wants = [ "network-online.target" ];
  environment = {
    VROUTER_ADDR = "127.0.0.1:8080";
    VROUTER_DATA_DIR = "/var/lib/vrouter";
    VROUTER_PUBLIC_URL = "https://router.example.com";
    VROUTER_OAUTH_PROVIDERS = builtins.toJSON [ {
      id = "company";
      name = "Company sign-in";
      issuer = "https://identity.example.com/realms/company";
      clientId = "vrouter";
      clientSecretEnv = "ROUTER_OIDC_SECRET";
      scopes = [ "openid" "profile" "email" "groups" ];
      roleMappings = [
        { claim = "groups"; value = "router-admins"; role = "admin"; }
        { claim = "groups"; value = "router-users"; role = "user"; }
      ];
    } ];
  };
  serviceConfig = {
    ExecStart = "/opt/vrouter/bin/vrouter";
    DynamicUser = true;
    StateDirectory = "vrouter";
    StateDirectoryMode = "0700";
    EnvironmentFile = "/run/secrets/vrouter.env";
    Restart = "on-failure";
    UMask = "0077";
  };
};
```

Supply `ROUTER_OIDC_SECRET=...` in the runtime secret file. Put an HTTPS reverse proxy in front of the service and forward `/auth`, `/api` and `/v1` as well as the UI. Preserve the browser's Host header. The public URL determines the callback origin; forwarded headers do not determine it.

## Shared keys and usage

Select a gateway, then open API keys to create a named key. The full secret is returned once. Only its hash and display prefix are retained. Keys can have a lifetime request limit, a lifetime token limit, both, or neither. Zero means unlimited.

A supported inference POST consumes one request when it is admitted, including attempts that later fail. Admission is persisted before forwarding and serialized across concurrent calls. Catalog reads do not consume this allowance. A request cap is enforced before the next request is forwarded.

Token limits use usage reported by the provider after the response. The request that reaches a limit can exceed it. A token-limited key permits only one inference request at a time; concurrent requests receive HTTP 429. If a forwarded request ends without usable accounting, further requests on that token-limited key are blocked rather than treating the unknown usage as zero. Change its token limit or replace the key to acknowledge the missing accounting and resume access. A restart treats unfinished requests the same way. Keys without token limits keep working, with a warning that their recorded token total is incomplete. These limits are not a hard monetary spending cap.

Revocation and deletion block new requests immediately. A response already in progress can finish. Deleting a key does not remove earlier telemetry. The legacy `VROUTER_API_KEY` is controlled through the environment rather than the key-management UI.

The request history shows the requested and routed model, provider, selected account, key, HTTP status, duration and reported tokens. Streaming Responses and Messages usage is recorded as events arrive. A stream can return HTTP 200 and still end with an error or incomplete outcome; inspect the outcome as well as the status. Missing usage appears as unknown.

Request history retains the latest 1,000 records per gateway. The registry has a 64 MiB storage limit; writes that exceed it fail rather than saving a file the next startup cannot read. Gateway deletion and a telemetry cleanup UI are not yet available. History totals cover retained records; key usage counters are lifetime totals. Telemetry excludes prompts, generated content, cookies and credentials. Input and output token counts follow provider reporting, and cached input is reported separately without adding it twice to total tokens.

Management integrations use `GET/POST /api/gateways`, `GET/POST /api/keys`, `PATCH/DELETE /api/keys/{id}` and `GET /api/telemetry`. Send `X-Vrouter-Gateway` for gateway-scoped management endpoints, including existing account, OAuth and model-setting routes. OIDC deployments require an explicit selection. Inference always selects its gateway from the API key, regardless of that header.

## Connect accounts

**Codex:** Add a Codex account for native sign-in and account allowance tracking. This uses the Codex OAuth client and backend directly, with a loopback callback on port 1455. State, PKCE, and a signed ID token bind the connection to its verified user and workspace. Reconnect renews that same identity.

`POST /api/oauth/codex` starts a new connection with an empty body or `{}`. Send `{ "accountId": "saved-account-id" }` to reconnect a native account. An explicit `"authMode": "codex"` is also accepted.

Experimental ChatGPT sign-in has been removed because its credentials cannot query the usage needed for routing and rotation. Saved experimental connections remain visible for removal, but cannot reconnect, refresh, discover models or route requests. Add the account through Codex sign-in; existing credentials are never silently converted.

**Claude:** The Claude connection uses authorization code exchange with PKCE and a localhost callback on port 54545. Provider acceptance of imported sessions and Claude OAuth must be checked with your own account.

When the browser runs on another machine, paste the complete callback address into the connection dialog. vrouter validates its exact address and state and never fetches a pasted URL. Sessions expire after five minutes; cancellation closes their listeners. Tokens are exchanged and stored on the server, not in browser storage.

The default gateway keeps its existing `state.json`. Additional gateways use `gateways/<id>/state.json`. Ownership, hashed keys, quota counters and recent telemetry are stored in `registry.json`. These files use mode `0600`, beneath mode `0700` directories. Writes replace the file atomically. A process lock prevents two vrouter instances from using the same credential store. Credentials are protected by file permissions, not encrypted at rest. Keep backups private.

Removing an account deletes its local credentials and stops new routing to it. It does not revoke the app at the provider.

## Client API

Use `http://127.0.0.1:8080/v1` as the base URL. Authenticate with a key created in the API keys page, sent as a bearer token or `x-api-key`. The key selects its gateway. `VROUTER_API_KEY` remains available for the legacy default gateway. Admin tokens and provider tokens are separate from inference keys.

- `GET /v1/models` lists enabled models advertised by connected accounts, with local aliases applied.
- `POST /v1/responses` routes Codex accounts.
- `POST /v1/messages` routes Claude accounts.

Codex requests require `stream: true`. vrouter adds `store: false` when omitted and rejects `store: true`. It converts string input to a user-message array. Other request fields and streaming events retain their provider meaning.

```sh
curl http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer $VROUTER_API_KEY"

# Replace MODEL_ID with an ID from the catalog.
curl --no-buffer http://127.0.0.1:8080/v1/responses \
  -H "Authorization: Bearer $VROUTER_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"MODEL_ID","input":[{"role":"user","content":"Hello"}],"stream":true,"store":false}'
```

vrouter selects only enabled accounts that advertise the requested model. Selection rotates across eligible accounts. A provider's HTTP 429 or 503 can try another account before a response starts, within the same authentication type. It never switches silently from a subscription credential to a paid API key, retries a network-ambiguous request, or retries after streaming begins. Clients must handle terminal SSE errors and interrupted streams themselves.

This base intentionally supports the two native inference protocols above. Chat Completions, Gemini routes, WebSockets, and cross-provider protocol translation are not implemented. Clients previously relying on CLIProxyAPI for those features must change their endpoint or keep a separate router for them.

## Move an existing installation

Stop the old gateway before importing. Stop CLIProxyAPI before either process refreshes a copied credential; copies of rotating refresh tokens cannot safely be used by both processes.

```sh
make build
node scripts/import-local-auth.mjs
node scripts/run-local.mjs
```

The importer prefers the existing CLIProxyAPI credential files under the old local `auth` directory. To explicitly import the CLI credential files instead, use `node scripts/import-local-auth.mjs --cli`. To choose individual files:

```sh
VROUTER_DATA_DIR=/your/private/directory ./bin/vrouter import /path/to/credential.json
```

Import is explicit. It does not alter the source files, contact a provider, or overwrite an already imported connection. Imported Codex tokens use the native Codex endpoint. New login is preferable to sharing a CLI's rotating refresh token.

The launcher can reuse the old local `connection.json` client key. It never starts the old engine. CLIProxyAPI's YAML configuration, aliases, wildcard rules and unsupported providers are not imported. Set model aliases and exclusions again in vrouter's Models page. Old files remain available for rollback; a token refreshed by vrouter may make an old copied token unusable.

## Management and model settings

`/api/state` reports the native store, account status, model catalog and configuration checks without returning credentials. Catalogs are fetched per account and cached for one minute; failed fetches are cached briefly. A listed model indicates provider advertisement, not a verified inference request.

Account controls persist enable/disable and removal directly. `GET /api/model-settings` returns original IDs and a revision. `PUT /api/model-settings` accepts `{revision, models: [{id, provider, enabled, alias, context}]}`. Aliases replace public IDs; disabled and renamed models remain editable. Validation rejects collisions and stale revisions. The same local policy governs catalog listing and inference.

Context overrides are positive whole token counts, up to 2,147,483,647. Omit `context` to preserve an existing override or send `0` to restore the provider value. The Models page saves overrides with the other model settings; clearing a context field restores automatic metadata. Settings return the effective `context`, `defaultContext` from the provider and any `contextOverride`. Overrides persist across restarts and appear as `context_window` in `/v1/models`. They describe the advertised limit and do not increase a provider's actual allowance. Each slug has a copy button beside it.

Claude discovery reads `/v1/models`, including its `max_input_tokens` and `max_tokens` limits. Codex discovery reads `chatgpt.com/backend-api/codex/models` with client version `0.160.1` and includes models with `visibility: "list"`. The provider gates newer models by client version, so this compatibility version needs updating alongside native client support. Model availability still depends on the connected account.

Native Codex and Claude accounts use direct usage probes, cached for up to one minute and refreshed when a reported window resets. Routing checks usage even with the dashboard closed and skips accounts whose relevant allowance is exhausted. The visible dashboard refreshes every minute and when you return to its tab. Missing or failed usage stays unknown and does not by itself disable an account. Dashboard refreshes only read usage and never redeem a reset or send inference.

When the whole eligible subscription pool is exhausted, vrouter checks fresh usage for every account before redeeming an available usage reset. It chooses the eligible account with the most remaining resets, verifies that the reset restored allowance, then retries the request. Claude grants must be usable, unexpired, next in the provider's order, and cover every known blocking window for the requested model. A definitive rejection can try the next eligible account. An ambiguous result stops further redemptions until it is reconciled. No reset is spent while another account has usable or unknown allowance. Paid API keys stay outside this subscription fallback.

Reset attempts are serialized and their request IDs are saved in `state.json` before sending, so concurrent requests and process restarts reuse an unresolved attempt. After a timeout, retries use that same ID and wait at least one minute. A confirmed reset must be observed as usable before vrouter can spend another reset on that account. If no reset restores access, the request returns HTTP 429; later requests check usage again. Requests are never held until a future reset, and streams are never replayed after output starts.

The Accounts page shows available reset counts only when the provider reports them. Codex uses the native [reset-credit protocol](https://github.com/openai/codex/blob/0b863c69f50335acd92164aab971cb58d298c2fe/codex-rs/backend-client/src/client/rate_limit_resets.rs). Claude uses the usage-grant and redemption protocol verified in Claude Code 2.1.291. Like T3 Code, its usage reads and reset claims send the Claude CLI client header required for reset eligibility. An ineligible response leaves the reset count unknown. Paused, unusable and expired grants do not count. These redeem existing resets, without buying credits or enabling paid overage. Provider eligibility still applies. Known general windows and Claude Opus/Sonnet windows affect routing; review, Cowork and unrecognized scoped windows are displayed without assuming they block the requested model.

Claude reports its subscription separately at `/api/oauth/profile`. vrouter reads that profile alongside usage, displays the reported plan and Max multiplier when available, and keeps the last known plan for disabled accounts or temporary profile failures. A failed profile request does not hide valid usage windows.

## Development

```sh
make dev
make build
```

`make dev` starts the native server on port 8080 and Vite on port 5173. Rebuild the UI before rebuilding a release binary. `make demo` builds and starts an isolated fixture view.

`cmd/vrouter` owns startup and import commands. `internal/gateway` holds routing, provider adapters, OAuth, storage and management handlers. `web/src` contains the React app. Gateway ownership, login, API key limits and request accounting are implemented in the gateway package. Protocol translation is not implemented.

## Assets

Provider marks retain their upstream MIT notice in `web/public/brands/LICENSE`. React, Lucide and DM Sans retain their dependency licenses. vrouter no longer invokes or vendors CLIProxyAPI.
