# vrouter

A Go model gateway with a React management UI, built into one binary for Linux and macOS. It serves Claude and Codex accounts through OpenAI and Anthropic compatible endpoints, with a percentage quota per client key.

## Quick start

Install [Nix and devenv](https://devenv.sh/getting-started/) and run `devenv shell`. Or install Go 1.26+, Node 22.12+ or 24, pnpm, and just yourself. Only the build needs Node.

```sh
just build
export VROUTER_ADMIN_TOKEN='replace-with-a-private-admin-token'
./bin/vrouter
```

Open http://127.0.0.1:8080, enter the admin token, add provider accounts, and create API keys.

## Configuration

vrouter does not load `.env` files. Export variables or set them in your service manager.

| Variable | Purpose |
| --- | --- |
| `VROUTER_ADDR` | Listen address, default `127.0.0.1:8080` |
| `VROUTER_PUBLIC_URL` | Public origin, such as `https://vrouter.example.com`. Optional for local development |
| `VROUTER_DATA_DIR` | Persistent state, default `$XDG_STATE_HOME/vrouter` or `~/.local/state/vrouter` |
| `VROUTER_ADMIN_TOKEN` | Administrator token. Required for non-loopback listeners unless external authentication is on |
| `VROUTER_EXTERNAL_AUTH` | Set to `1` when an authentication proxy protects the dashboard and management API |
| `VROUTER_WINDOW_SKIP_PLANS` | Plans that never get an automatic 5-hour window trigger, default `codex:pro*` |

## Data

One `vrouter.json` file in the data directory holds accounts, model settings, client keys, quotas, and request history. vrouter writes it with restricted file permissions but does not encrypt it, so keep backups private.

Run one instance per data directory. Stop the server before you restore data or import provider credentials.

This testing build does not migrate older files. Use a fresh data directory or convert the data by hand.

## Remote hosting

Put vrouter behind an HTTPS reverse proxy and set `VROUTER_PUBLIC_URL` to the address users open.

### Dashboard access

Management has three modes.

- With neither variable set, management accepts loopback peers and loopback hosts only.
- With `VROUTER_ADMIN_TOKEN`, every management request needs the bearer token.
- With `VROUTER_EXTERNAL_AUTH=1`, an authentication proxy such as TinyAuth protects the dashboard and every `/api` route. Keep the backend private so clients cannot bypass the proxy.

vrouter does not read identity headers or keep user sessions, so everyone the proxy admits is a full administrator. Restrict its users or groups. If you set both variables, management still requires the token.

Route `/v1/*` straight to vrouter with no browser login. Those endpoints require a vrouter API key instead.

### Public URL

vrouter uses `VROUTER_PUBLIC_URL` for the client endpoints you copy from the UI and for provider sign-in return links. Browser management writes must come from that origin, so the reverse proxy has to preserve the Host header.

The value is an origin with an optional trailing slash. Subpaths are unsupported, and forwarded headers do not override it. Without the variable, the UI uses the browser's origin and return links use the request's checked Origin or Host.

### Provider sign-in

Provider accounts connect over OAuth, which is separate from dashboard access. When the app URL is remote, vrouter uses hosted flows and opens no localhost callback ports.

- Codex shows a device code. Enter it on the linked OpenAI page and vrouter finishes on its own. Enable device code login in your ChatGPT security settings or workspace permissions first.
- Claude shows an authorization code on its own website. Paste the complete `code#state` value into vrouter.

Local development keeps the automatic loopback callbacks. Providers fix their registered redirects, so the public URL is where vrouter lives, not a replacement callback.

## NixOS

`nix build` produces `result/bin/vrouter` with the UI embedded. The flake supports x86-64 and ARM64 Linux and Apple Silicon macOS. It provides `packages.<system>.default`, `packages.<system>.vrouter`, and `nixosModules.default`.

The flake is for packaging and deployment. Development still uses `devenv shell`. The flake's nixpkgs pin starts at the same revision as `devenv.lock`, and you can update it independently.

Add the input and import the module in the host configuration:

```nix
# flake.nix inputs
vrouter = {
  url = "github:zekurio/vrouter";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

```nix
{ inputs, config, ... }: {
  imports = [ inputs.vrouter.nixosModules.default ];
  services.vrouter = {
    enable = true;
    publicUrl = "https://vrouter.example.com";
    environmentFile = config.sops.secrets.vrouter_env.path;
  };
}
```

The environment file must contain `VROUTER_ADMIN_TOKEN=...`. Supply it through SOPS or another secret manager. Never put tokens in Nix expressions or store-backed files. To use an authentication proxy instead, set `externalAuth = true` and follow the rules under [Dashboard access](#dashboard-access).

The service listens on `127.0.0.1:8080` and opens no firewall ports. It runs as a dynamic systemd user and keeps state in `/var/lib/vrouter` with mode `0700`. The options are `host`, `port`, `package`, `publicUrl`, `externalAuth`, `windowSkipPlans`, and `environmentFile`.

Read logs with `journalctl -u vrouter`. Back up `/var/lib/private/vrouter`, which is systemd's backing directory for the state. Stop the service before you restore it or import credentials.

The frontend dependency hash lives in `nix/package.nix`. After changing `web/pnpm-lock.yaml`, replace the hash with `lib.fakeHash`, run `nix build`, and copy in the hash it reports. The fetcher includes all platforms, so one hash works for Linux and macOS.

## API keys and quotas

Create a key for each person or tool on the API keys page. vrouter shows the secret once and stores only its hash. You can rename, revoke, or delete a key.

Client keys cannot open management endpoints, and admin tokens cannot authorize inference. vrouter no longer reads `VROUTER_API_KEY` or the old `client-key` file.

### Percentage limits

Each key has optional Claude and Codex limits for the 5-hour and 7-day windows. Blank means unlimited. Zero blocks the provider. Once a key reaches either limit, further requests to that provider get HTTP 429. The request that crosses a limit can finish above it.

The two windows are independent. Each account's contribution expires at the reset time its provider reports.

vrouter measures a request by reading the account's provider-reported utilization before and after it. Each enabled subscription account counts as one equal share of its provider's pool. With four accounts, 20 percentage points on one account costs 5% of the pool. vrouter does not weight plans by token capacity. It captures the pool size when the request starts, and later pool changes do not rewrite existing charges.

These numbers are estimates. Providers round and delay their readings, and vrouter cannot tell gateway traffic from other use of the same account. Dedicate percentage-limited accounts to vrouter.

While any active key has a percentage quota for a provider, vrouter allows one request at a time in that gateway's pool for that provider. Overlapping calls get HTTP 429 with a short retry interval. Claude and Codex run independently.

### Uncertain usage

A missing measurement, an ambiguous request, or an unexpected reset during a request blocks the key's capped access to that provider. Saving the key's provider quotas acknowledges the gap and keeps the usage vrouter does know about. After a restart, vrouter treats unfinished requests as uncertain.

Provider accounts that use an API key cannot enforce percentage quotas. vrouter does not treat an unavailable provider window as unlimited.

### Expiry and history

A key can have an expiry. After it passes, vrouter refuses the key's calls with HTTP 401, though a response already running finishes. Move the expiry later or clear it to restore the key.

Keys have no lifetime request or token limit. vrouter persists a reservation before forwarding each request and settles token usage afterward. It keeps the latest 1,000 requests per gateway, and deleting or revoking a key does not erase them.

## Client requests

OpenAI-compatible clients use `https://your-host/v1` as the base URL. Anthropic clients that append `/v1/messages` use `https://your-host`. In Delta, select Responses and use the base URL with `/v1`.

vrouter serves `/v1/models`, `/v1/responses`, `/v1/messages`, and `/v1/chat/completions`. `/v1beta` is unsupported.

The model ID picks the provider and the endpoint picks the client protocol. When they differ, vrouter converts text, images, tools, tool results, and streaming events to the provider's format. It returns HTTP 400 for anything it cannot convert.

vrouter never moves a request from a subscription account to a paid API-key account.

### Parameters

Codex subscription accounts accept an output token cap. vrouter converts Messages `max_tokens` and Chat `max_completion_tokens` to Responses `max_output_tokens`.

Codex rejects `temperature` and `top_p`. vrouter validates them, drops them for Codex, and lists them in the `X-Vrouter-Ignored-Parameters` response header. Other accounts keep their sampling settings.

Stored Responses are unsupported. Set `store` to `false`.

Messages `metadata.user_id` has no equivalent on the other side. vrouter drops it and lists it in the same header.

### Thinking budgets

When a Messages request sets a fixed thinking budget for a Responses model, vrouter turns the budget into a reasoning effort.

| Budget tokens | Effort |
| --- | --- |
| up to 1,024 | `low` |
| up to 8,192 | `medium` |
| up to 24,576 | `high` |
| larger | `xhigh` |

The effort is a hint, not a thinking token limit, and the output token cap still applies. An explicit `output_config.effort` wins.

### Tools and reasoning

Conversion accepts cache hints and direct tool-call defaults. A failed tool result keeps its content and call ID and gains an error marker.

Clients must replay the complete assistant output when they send tool results. To keep signed reasoning intact across protocols, vrouter carries provider state in Responses `encrypted_content`, Messages `signature`, or the Chat extension `vrouter_reasoning`. The wrapper is base64. Send it back unchanged and stay on the same provider. Chat clients that drop `vrouter_reasoning` break reasoning with tool calls.

### Claude OAuth

Claude OAuth requests need the native Claude Code identity block. vrouter adds it ahead of the caller's system prompt and leaves the caller's blocks unchanged. This covers inference and automatic window starts. Paid API-key requests keep their original system prompt.

### Testing a model

Open a model in the UI to copy a request or run **Test this model** with a client key. The test calls the public inference endpoint and counts against that key. It shows the HTTP status, provider error, and reply, and passes only after a complete response. The key stays in memory until you close the dialog or switch gateways.

**Connected** on an account means vrouter has saved credentials. It does not prove inference works.

## Automatic usage resets

When a client request finds every account in its provider pool blocked, vrouter refreshes their usage and looks for included resets.

It spends a reset only on an account with an exhausted 7-day allowance. Among those, it picks the account whose weekly allowance resets furthest in the future. An exhausted 5-hour window alone never triggers a reset. vrouter skips accounts with no reported weekly reset time or no reset available.

Claude's Opus and Sonnet weekly limits count when they block the requested model. The grant vrouter selects must clear every blocking limit on that account.

vrouter confirms fresh usable quota before it retries the request. A pending attempt keeps its request ID across restarts, and vrouter spends no other account's reset while that outcome is unknown.

The provider decides which windows a reset clears. The weekly-only rule only controls when vrouter spends one. Resets happen on demand, never from the background 5-hour window check.

## Automatic 5-hour windows

Every minute, vrouter checks each enabled subscription account in every gateway for an active 5-hour window and sends a small request to the accounts without one. Accounts that are idle at the same check start together.

vrouter picks the smallest advertised model. That means Haiku for Claude, and nano then mini for Codex. It honors model exclusions and never substitutes a larger model. Catalogs do not expose prices, so this is a fixed preference and not a price comparison. Codex uses the lowest reasoning level the model advertises. Claude sends no thinking configuration and caps output at eight tokens.

vrouter does not recheck an account with an active window until its reported reset time. It skips plans without a 5-hour window and rechecks them hourly.

Codex Pro plans report a 5-hour entry without enforcing one, so vrouter also skips plans by name. `VROUTER_WINDOW_SKIP_PLANS` takes comma-separated `plan` or `provider:plan` entries and replaces the default `codex:pro*`. Matching ignores case and tests both the provider's plan value and the name shown on the account. A trailing `*` matches any ending, so the default covers every Codex Pro tier. Set the variable to `none` to trigger on every plan.

A trigger consumes both provider allowances where they apply. It is not charged to a client key and does not redeem resets. vrouter sends no trigger while another allowance on the account is exhausted, or while the provider is serving a percentage-limited request.

After a failed check, a rejected trigger, or a trigger whose new reset time vrouter cannot confirm, it waits five minutes before trying that account again. It persists the cooldown, so a restart cannot repeat a trigger sooner. vrouter logs starts and failures.

## Management API

`GET` and `POST /api/keys` list and create keys. `PATCH` and `DELETE /api/keys/{id}` change and remove them. A creation body looks like this:

```json
{
  "name": "Alice",
  "providerQuotas": {
    "claude": { "fiveHour": 25, "sevenDay": 25 },
    "codex": { "fiveHour": 45, "sevenDay": 45 }
  }
}
```

On an update, omitting `providerQuotas` keeps the existing limits. Supplying it replaces them and acknowledges uncertain percentage usage.

`expiresAt` takes a future RFC 3339 timestamp, or `null` for a key that never expires. Omitting it on an update keeps the current expiry.

`GET /api/telemetry` returns recent requests.

Select older gateways with the `X-Vrouter-Gateway` header. Inference always takes its gateway from the client key.

## Development

`devenv shell` enters the environment, and `devenv shell just build` runs one command in it. With direnv hooked into your shell, run `direnv allow` once and the environment activates when you enter the repo.

```sh
just dev                    # Go server on :8080, Vite on :5173
just build                  # Build UI and bin/vrouter
go test ./...               # Backend tests
pnpm --dir web build        # Type-check and build the UI
```

`just dev` uses the normal persistent data directory. Set `VROUTER_DATA_DIR` to a scratch directory for experiments. The UI reloads as you edit, but Go changes need a restart of `just dev`.

- `cmd/vrouter` has startup and import commands.
- `internal/gateway` has routing, provider adapters, authentication, storage, and management handlers.
- `web/src` has the React UI.
