# vrouter

A Go model gateway with an embedded React management UI. Runs as one binary on Linux or macOS.

## Hosting

Install [Nix and devenv](https://devenv.sh/getting-started/), then run `devenv shell`. The environment provides Go 1.26, Node 24, pnpm, just, gopls, and librsvg. You can also install Go 1.26+, Node 22.12+ or Node 24, pnpm, and just manually.

```sh
just build
export VROUTER_ADMIN_TOKEN='replace-with-a-private-admin-token'
./bin/vrouter
```

Open http://127.0.0.1:8080, enter the admin token, then add provider accounts and create API keys in the UI. Node is only needed for the build.

| Variable | Purpose |
| --- | --- |
| `VROUTER_ADDR` | Listen address, default `127.0.0.1:8080` |
| `VROUTER_PUBLIC_URL` | Public HTTP(S) origin, such as `https://vrouter.example.com`; optional for local development |
| `VROUTER_DATA_DIR` | Persistent state, default `$XDG_STATE_HOME/vrouter` or `~/.local/state/vrouter` |
| `VROUTER_ADMIN_TOKEN` | Optional administrator token; required for non-loopback listeners unless external authentication is enabled |
| `VROUTER_EXTERNAL_AUTH` | Set to `1` when an authentication proxy protects the dashboard and management API |
| `VROUTER_WINDOW_SKIP_PLANS` | Plans that never get an automatic 5-hour window trigger, default `codex:pro*` |

Export variables or set them in your service manager. The server does not load `.env` files automatically.

For remote hosting, put vrouter behind an HTTPS reverse proxy. With TinyAuth or another authentication proxy, set `VROUTER_EXTERNAL_AUTH=1` and protect the dashboard and every `/api` route. Keep the backend private so clients cannot bypass the proxy. Preserve the browser's Host header. vrouter does not trust identity headers or maintain user sessions.

Route `/v1/*` directly to vrouter without browser authentication. These endpoints still require a vrouter API key. The optional `VROUTER_ADMIN_TOKEN` continues to require a bearer token on management requests, even with external authentication enabled. Without either option, management accepts loopback peers and loopback hosts only.

Provider account connections use OAuth. Dashboard access uses the authentication proxy or admin token.

Persist the data directory and keep its backups private. Credentials are stored with restricted file permissions, without encryption at rest. Run only one instance per data directory.

Accounts, model settings, client keys, quotas, and request history share one `vrouter.json` file. This testing build does not migrate older files. Use a fresh data directory or convert the data by hand. Stop the server before restoring data or importing provider credentials.

Set `VROUTER_PUBLIC_URL` to the HTTPS address users open. vrouter uses it for copied client endpoints and provider sign-in return links. Browser management writes must use that origin, and the reverse proxy must preserve Host. The setting accepts an origin with an optional trailing slash; hosting under a subpath is unsupported. Forwarded headers do not override it. Without it, the UI uses the browser's origin and provider return links use the request's checked Origin or Host.

When the app URL is remote, provider sign-in uses hosted flows without opening localhost callback ports. Codex shows a device code; enter it on the linked OpenAI page and vrouter finishes automatically. Enable device code login in your ChatGPT security settings or workspace permissions first. Claude shows an authorization code on its own website; paste the complete `code#state` value into vrouter. Local development keeps the automatic loopback callbacks. Provider-registered redirects are fixed; the public URL is the address of vrouter, not a replacement provider callback.

## NixOS

`nix build` produces `result/bin/vrouter` with the UI embedded. The flake supports x86-64 and ARM64 Linux and Apple Silicon macOS, and provides `packages.<system>.default`, `packages.<system>.vrouter`, and `nixosModules.default`. Development continues to use `devenv shell`; the flake is for packaging and deployment. Its nixpkgs pin starts at the same revision as `devenv.lock` and can be updated independently.

Add the flake input and import its module in the host configuration:

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

The runtime environment file must contain `VROUTER_ADMIN_TOKEN=...`. Supply it through SOPS or another secret manager; never put tokens in Nix expressions or store-backed files. Alternatively, set `externalAuth = true` and put an authentication proxy in front of the dashboard and all `/api` routes. Every user admitted by that proxy has full administrator access. Restrict its users or groups explicitly. Inference under `/v1/*` uses separate vrouter API keys and must bypass browser login.

The service defaults to `127.0.0.1:8080`, opens no firewall ports, and stores state under `/var/lib/vrouter` using a dedicated dynamic systemd user and mode `0700`. `host`, `port`, `package`, `publicUrl`, `externalAuth`, `windowSkipPlans`, and `environmentFile` are configurable. Logs are available with `journalctl -u vrouter`. Back up `/var/lib/private/vrouter`, systemd's backing directory for the state, and stop the service before restoring it or importing credentials.

The frontend dependency hash lives in `nix/package.nix`. After changing `web/pnpm-lock.yaml`, replace it with `lib.fakeHash`, run `nix build`, and use the reported actual hash. The fetcher includes all platforms so the same hash works for Linux and macOS.

## API keys and quotas

Create a key for each person or tool in the API keys page. The secret is shown once; only its hash is stored. Keys can be renamed, revoked, or deleted. Client keys cannot open management endpoints.

All client keys use these managed records. The server no longer reads `VROUTER_API_KEY` or the old `client-key` file. Dashboard admin tokens remain separate and cannot authorize inference.

Each key has optional Claude and Codex percentage limits, separately for the 5-hour and 7-day windows. Blank means unlimited; zero blocks requests to that provider. Reaching either limit blocks subsequent requests to that provider with HTTP 429. Each account's contribution expires at its provider-reported reset time; the two windows are independent. The request that crosses a limit can finish above it.

Percentages are measured from changes in provider-reported account utilization before and after a request. Each enabled subscription account contributes one equal share of its provider's pool. For four accounts, a measured 20 percentage points on one account consumes 5% of the pool. Different subscription plans are not weighted by token capacity. Pool size is captured when the request starts; changing the pool does not rewrite existing charges.

These are observed estimates: provider readings can be rounded or delayed, and use of an account outside vrouter cannot be separated from gateway traffic. Percentage-limited accounts should be dedicated to vrouter. While any active key has a percentage quota for a provider, vrouter allows one request at a time in that gateway's provider pool; overlapping calls get HTTP 429 with a short retry interval. Claude and Codex can run independently.

A missing measurement, an ambiguous request, or an unexpected reset during a request blocks the affected key's capped provider access. Saving its provider quotas explicitly acknowledges the gap without clearing known usage. Restart recovery treats unfinished requests as uncertain. API-key provider credentials cannot enforce subscription-percentage quotas, and an unavailable provider window is not treated as unlimited.

A key can also have an expiry. After that time vrouter refuses its calls with HTTP 401; a response that is already running finishes. Moving the expiry later or clearing it restores the key. Keys have no lifetime request or token limit. Request reservations are persisted before forwarding; token usage settles afterward. Deleting or revoking a key does not erase request history. History retains the latest 1,000 requests per gateway.

## Client requests

Use `https://your-host/v1` as the base URL for OpenAI-compatible clients. For Anthropic clients that add `/v1/messages`, use `https://your-host`. In Delta, select Responses and use the base URL with `/v1`.

vrouter serves `/v1/models`, `/v1/responses`, `/v1/messages`, and `/v1/chat/completions`. The model ID selects the provider. The endpoint selects the client protocol. When they differ, vrouter converts text, images, tools, tool results, and streaming events to the provider's format. It returns HTTP 400 for features it cannot convert. `/v1beta` is not supported.

Provider limits still apply. Codex subscription accounts accept output token caps. vrouter converts Messages `max_tokens` and Chat `max_completion_tokens` to Responses `max_output_tokens`. Codex does not accept `temperature` or `top_p`. vrouter checks these values, omits them for Codex, and lists them in the `X-Vrouter-Ignored-Parameters` response header. Other accounts keep their sampling settings. Stored Responses remain unsupported; set `store` to `false`. vrouter does not move a request from a subscription account to a paid API-key account.

When Messages requests use a fixed thinking budget with a Responses model, vrouter converts the budget to a reasoning effort: up to 1,024 tokens becomes `low`, up to 8,192 becomes `medium`, up to 24,576 becomes `high`, and larger budgets become `xhigh`. This is an effort hint, not a separate thinking token limit. An explicit `output_config.effort` takes priority. The output token cap still applies. Cache hints and direct tool-call defaults are accepted during conversion. Failed tool results keep their content and call ID, with an error marker. Messages `metadata.user_id` has no matching client tag in this adapter. Conversion omits it and lists it in the same response header.

Claude OAuth requests need the native Claude Code identity block. vrouter adds it before the caller's system prompt and keeps the caller's prompt blocks unchanged. This applies to inference and automatic window starts. Paid API-key requests keep their original system prompt.

Clients must replay the complete assistant output when they send tool results. To preserve signed reasoning across protocols, vrouter carries provider state in Responses `encrypted_content`, Messages `signature`, or the Chat extension `vrouter_reasoning`. The wrapper uses base64 encoding. Keep it unchanged and continue with the same provider. Chat clients must preserve `vrouter_reasoning` for reasoning and tool calls to work together.

Open a model in the UI to copy a request or run **Test this model** with a client key. The test uses the public inference endpoint and counts against that key. It shows the HTTP status, provider error, and reply. It passes only after a complete response. The key stays in memory until you close the dialog or switch gateways. An account marked **Connected** has saved credentials; this does not prove that inference works.

## Automatic 5-hour windows

vrouter checks every minute whether each enabled subscription account in every gateway has an active 5-hour window, and sends a small request to the ones that do not. Accounts that are idle at the same check start together. vrouter prefers advertised Haiku models for Claude and nano, then mini models for Codex, honors model exclusions, and refuses to substitute a larger model. Catalogs do not expose prices, so these are explicit small-model preferences rather than a live price comparison. Codex uses the lowest reasoning level advertised by that model; Claude sends no thinking configuration and caps output at eight tokens.

An account with an active window is not checked again until its provider-reported reset time. Plans without a 5-hour window are skipped and rechecked hourly. Codex Pro plans report a 5-hour entry without enforcing one, so plans are also skipped by name: `VROUTER_WINDOW_SKIP_PLANS` takes comma-separated `plan` or `provider:plan` entries, matched without regard to case against the provider's plan value or the name shown on the account, and replaces the default `codex:pro*`. A trailing `*` matches any ending, so the default covers every Codex Pro tier. Set it to `none` to trigger on every plan. Triggers consume both provider allowances where applicable, are not charged to a client key, and do not redeem resets. No trigger is sent while another allowance of the account is exhausted or while the provider is serving a percentage-limited request. After a failed check, a rejected trigger, or a trigger whose new reset time is not confirmed, vrouter waits five minutes before trying that account again; the cooldown is persisted, so a restart cannot repeat a trigger sooner. Starts and failures are logged.

## Management API

`GET/POST /api/keys` and `PATCH/DELETE /api/keys/{id}` manage keys. For example, a creation body can include `{"name":"Alice","providerQuotas":{"claude":{"fiveHour":25,"sevenDay":25},"codex":{"fiveHour":45,"sevenDay":45}}}`. Omitting `providerQuotas` on an update preserves the existing settings; supplying it replaces them and acknowledges uncertain percentage usage. `expiresAt` takes an RFC 3339 timestamp in the future, or `null` for a key that never expires; omitting it on an update keeps the current expiry. `GET /api/telemetry` returns recent requests. Older gateways can be selected with `X-Vrouter-Gateway`; inference always selects its gateway from the client key.

## Development

Run `devenv shell` to enter the development environment, or `devenv shell just build` to run a single command. With direnv installed and hooked into your shell, run `direnv allow` once to activate the environment automatically when entering the repo.

```sh
just dev                    # Go server on :8080, Vite on :5173
just build                  # Build UI and bin/vrouter
go test ./...               # Backend tests
pnpm --dir web build        # Type-check and build the UI
```

`just dev` uses the normal persistent data directory. Create client keys on the Keys page. Set `VROUTER_DATA_DIR` to a scratch directory for experiments. The UI reloads as you edit; restart `just dev` after Go changes.

- `cmd/vrouter`: startup and import commands.
- `internal/gateway`: routing, provider adapters, authentication, storage and management handlers.
- `web/src`: React UI.
