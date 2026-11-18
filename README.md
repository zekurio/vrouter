# vrouter

A Go model gateway with a React management UI, built into one binary for Linux and macOS. It serves Claude and Codex accounts through OpenAI and Anthropic compatible endpoints. One admin key opens the dashboard.

## Quick start

Install [Nix and devenv](https://devenv.sh/getting-started/) and run `devenv shell`. Or install Go 1.26+, Node 22.12+ or 24, pnpm, and just yourself. Only the build needs Node.

Build and start vrouter:

```sh
just build
./bin/vrouter
```

vrouter prints an admin key when it starts. Open http://localhost:8080 and sign in with it. Add provider accounts on the Accounts page and create API keys on the Keys page.

The printed key changes at every start. To keep one, set `VROUTER_ADMIN_KEY` yourself:

```sh
export VROUTER_ADMIN_KEY="$(openssl rand -base64 32)"
```

## Configuration

vrouter does not load `.env` files. Export variables or set them in your service manager.

| Variable | Purpose |
| --- | --- |
| `VROUTER_ADDR` | Listen address, default `127.0.0.1:8080` |
| `VROUTER_ADMIN_KEY` | The key that opens the dashboard and the management API, 16 to 512 characters. Unset, vrouter makes a new key at every start and prints it |
| `VROUTER_ADMIN_KEY_FILE` | A file that holds the admin key, in place of `VROUTER_ADMIN_KEY` |
| `VROUTER_PUBLIC_URL` | The origin people open, such as `https://vrouter.example.com`. Set it behind a reverse proxy. Unset, vrouter trusts the origin each browser names |
| `VROUTER_DATA_DIR` | Persistent state, default `$XDG_STATE_HOME/vrouter` or `~/.local/state/vrouter` |
| `VROUTER_WINDOW_SKIP_PLANS` | Plans that never get an automatic 5-hour window trigger, default `codex:pro*` |
| `VROUTER_PRICE_CATALOG` | Where the Usage page's list prices come from. Empty reads [models.dev](https://models.dev), `off` uses only the prices built into vrouter, and any other value is the URL of a catalog in the models.dev format |

## Admin key

Whoever holds the admin key manages everything: provider accounts, model settings, client keys, and usage. vrouter has no users or roles.

The dashboard asks for the key once and keeps the sign-in in a cookie for 30 days. The cookie is derived from the key, so it survives a restart and stops working when the key changes. Changing the key is how you sign every browser out. Scripts send the key as `Authorization: Bearer <key>`.

Sign-in has no attempt limit, so use a long random key. vrouter refuses one shorter than 16 characters.

## Accounts and routing

Every account serves every client key. Requests rotate across the accounts of a provider pool. An account that just failed moves to the back of the order for 30 seconds, or for as long as its rate limit's `Retry-After` asks, up to five minutes. An account fails on its own with a rate limit, refused credentials, a provider outage or overload, or a dropped connection. When a provider refuses an access token that has not expired yet, vrouter refreshes it before the account's next request.

vrouter keeps each account's model catalog for a minute and then reloads it in the background. When a provider fails to list its models, the last list it sent keeps serving for up to six hours, unless the provider refused the account's credentials. A failed token refresh is retried after 30 seconds, and the old token serves until it expires.

Each provider account connects to vrouter once. Signing in to an account that is already connected replaces its stored sign-in.

## Data

`vrouter.json` in the data directory holds accounts, model settings, and client keys. `usage.sqlite` holds usage totals, in-flight accounting, the latest requests, and 30 days of account quota reports. Both use restricted file permissions and are unencrypted, so keep backups private.

Run one instance per data directory. Stop the server before you restore data or import provider credentials.

Data from a build with users is upgraded on the first start. Users are dropped, every key keeps working, and personal accounts become ordinary accounts that serve every key. The old file stays beside the new one as `vrouter.v2.json`, because older builds cannot read the new layout. Usage history is merged into one set of totals, and the request log keeps its newest 1,000 entries.

Data from before gateways were removed is upgraded the same way, with `vrouter.v1.json` as the copy. vrouter refuses to start on data with gateways besides the default one.

Delete the copy once the upgrade works, since it holds the same unencrypted credentials.

The earlier move of usage accounting to SQLite kept configuration and started fresh usage history. Older configuration files with the removed `chatgpt_registration`, `client_secret`, or retired model-policy fields are still unsupported; use a fresh data directory for those builds.

To import existing provider logins, stop vrouter and pass native Codex or Claude CLI credential files:

```sh
./bin/vrouter import ~/.codex/auth.json ~/.claude/.credentials.json
```

The import skips files already saved in the store. It does not replace credentials that vrouter has refreshed. It accepts native CLI files only.

## Remote hosting

Put vrouter behind an HTTPS reverse proxy and set `VROUTER_PUBLIC_URL` to the address you open.

### Dashboard access

vrouter handles sign-in itself, so the reverse proxy needs no authentication of its own. Route everything to vrouter. `/v1/*` requests need a vrouter API key, and `/api` routes need the admin key or the dashboard's cookie.

The session cookie is HTTP-only and `SameSite=Strict`. It is `Secure` when the public URL uses `https`. vrouter refuses management writes whose `Origin` is not the public URL.

### Public URL

vrouter uses `VROUTER_PUBLIC_URL` for the client endpoints you copy from the UI and for provider sign-in return links. Browser management writes must come from that origin, so the reverse proxy has to preserve the Host header.

The value is an origin with an optional trailing slash. Subpaths are unsupported, and forwarded headers do not override it.

The dashboard's pages have addresses of their own, such as `/usage` and `/accounts`. vrouter serves the dashboard for them, so a proxy needs no rewrite rules.

### Provider sign-in

Provider accounts connect over OAuth, which is separate from dashboard access. When the public URL is not on localhost, vrouter uses hosted flows and opens no localhost callback ports.

- Codex shows a device code. Enter it on the linked OpenAI page and vrouter finishes on its own. Enable device code login in your ChatGPT security settings or workspace permissions first.
- Claude shows an authorization code on its own website. Paste the complete `code#state` value into vrouter.

With a localhost public URL, or none, vrouter keeps the automatic loopback callbacks. Providers fix their registered redirects, so the public URL is where vrouter lives, not a replacement callback.

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
    adminKeyFile = config.sops.secrets.vrouter_admin_key.path;
  };
}
```

The module passes `adminKeyFile` to the service as a systemd credential, so the file can stay readable by root alone. Supply it through SOPS or another secret manager. Never put secrets in Nix expressions or store-backed files. Without `adminKeyFile`, vrouter prints a new admin key to the journal at every start.

The service listens on `127.0.0.1:8080` and opens no firewall ports. It runs as a dynamic systemd user and keeps state in `/var/lib/vrouter` with mode `0700`. The options are `host`, `port`, `package`, `publicUrl`, `adminKeyFile`, `windowSkipPlans`, and `environmentFile`. The environment file is optional and can set any other variable from [Configuration](#configuration).

Read logs with `journalctl -u vrouter`. Back up `/var/lib/private/vrouter`, which is systemd's backing directory for the state. Stop the service before you restore it or import credentials.

The frontend dependency hash lives in `nix/package.nix`. After changing `web/pnpm-lock.yaml`, replace the hash with `lib.fakeHash`, run `nix build`, and copy in the hash it reports. The fetcher includes all platforms, so one hash works for Linux and macOS.

## API keys

Create keys on the API keys page, one for each tool. vrouter shows the secret once and stores only its hash. You can rename, revoke, or delete a key.

Client keys cannot open management endpoints, and the admin key cannot authorize inference.

### Expiry and history

A key can have an expiry. After it passes, vrouter refuses the key's calls with HTTP 401, though a response already running finishes. Move the expiry later or clear it to restore the key.

Keys have no usage limits. vrouter counts each authenticated inference attempt when it starts and adds measured tokens when a successful response finishes. Deleting or revoking a key does not erase aggregate usage.

## Usage

Usage lives in `usage.sqlite` in the data directory. Credentials and configuration stay in `vrouter.json`. SQLite uses WAL mode and durable transactions; inference accounting does not rewrite the configuration file. The first run with SQLite starts fresh usage history and discards the old JSON request log and key counters while preserving accounts, keys, and settings.

The Usage page shows what your requests would cost at public API list prices: the total, the cost over time, and the split by token type, speed tier, model, and key. The totals cover every request since the database was created. They survive restarts and never decrease.

Prices come from [models.dev](https://models.dev). vrouter downloads its catalog at startup and then twice a day, asks again within the hour when a request uses a model it has no price for, and keeps a copy in `usage.sqlite` for restarts without network. Models the catalog lacks fall back to prices built into vrouter. A request is priced when it finishes. A model that later gets a price also prices its earlier unpriced usage, though cache writes in that usage count at the input rate and long-prompt surcharges do not apply. Set `VROUTER_PRICE_CATALOG=off` to keep vrouter from contacting models.dev.

vrouter keeps running totals for each five-minute period by provider, model, key, and speed tier, and retains them permanently. Days and hours on the page follow the browser's time zone.

The Requests view lists the latest 1,000 requests with their timing, model, key, account, outcome, HTTP status, and token counts. Streamed requests show the time to first output. Requests that were cut short say why: the output limit, a content filter, the provider stopping, the client disconnecting, the provider stream dropping, or a process restart. Older requests leave the list but stay in the totals. Prompts, completions, and credentials are never stored. A failed request keeps a short error summary after the same redaction applied to the error the client receives. A request left running after a process restart is recorded as cut short, with unknown token usage. A request refused before vrouter could read it whole, such as one over 32 MiB or one the client abandoned mid-upload, still counts toward its model when the model appears in the first 64 KiB of its body. A request without a usage report adds no tokens or cost, and a request on a model without a list price counts as unpriced.

Back up the entire data directory with the service stopped, including `usage.sqlite` and any `-wal`/`-shm` files. Copying only the live database file can miss committed WAL data.

## Client requests

OpenAI-compatible clients use `https://your-host/v1` as the base URL. Anthropic clients that append `/v1/messages` use `https://your-host`. In Delta, select Responses and use the base URL with `/v1`.

vrouter serves `/v1/models`, `/v1/responses`, `/v1/messages`, `/v1/chat/completions`, `/v1/images/generations`, and `/v1/images/edits`. `/v1beta` is unsupported.

The model ID picks the provider and the endpoint picks the client protocol. When they differ, vrouter converts text, images, tools, tool results, and streaming events to the provider's format. It returns HTTP 400 for anything it cannot convert.

vrouter never moves a request from a subscription account to a paid API-key account.

### Pi with Claude subscriptions

Add a custom provider to `~/.pi/agent/models.json`, using a vrouter client key and a Claude model available from `/v1/models`:

```json
{
  "providers": {
    "vrouter": {
      "baseUrl": "http://localhost:8080",
      "api": "anthropic-messages",
      "apiKey": "${VROUTER_API_KEY}",
      "models": [
        {
          "id": "claude-sonnet-4-6",
          "reasoning": true,
          "input": ["text", "image"],
          "contextWindow": 200000,
          "maxTokens": 64000
        }
      ]
    }
  }
}
```

Set `VROUTER_API_KEY` in Pi's environment, replace the URL and model as needed, then select the model with `/model`. The base URL has no `/v1` suffix because the Anthropic client appends `/v1/messages`. Use the selected model's actual context and output limits when changing the example.

Claude OAuth requests use a Claude Code header profile and signed billing block. vrouter moves third-party system instructions into conversation system messages on newer Claude models, or system reminders in the first user message on Claude 3 and 4. Custom tools get deterministic MCP aliases; responses restore the original names, including streamed tool calls. Tool arguments, caller cache markers, and thinking signatures are preserved. Native Claude Code requests with their own identity and billing blocks keep their system layout and tool names. API-key accounts do not receive this adaptation.

Session identity comes from `X-Claude-Code-Session-Id`, `X-Session-Id`, `Session-Id`, `session_id`, or an OpenAI `prompt_cache_key`, then Claude's `metadata.user_id` session field. Without a session identifier, vrouter derives one from the client key, caller identity, system instructions and first user message. Clients that compact history should send a stable session identifier. Tool aliases stay stable across account rotation and restarts; vrouter adds no changing date text to the conversation.

Anthropic may reject third-party subscription requests or require extra usage. HTTP 200 alone does not establish which allowance was charged.

### Parameters

Codex subscription accounts accept an output token cap. vrouter converts Messages `max_tokens` and Chat `max_completion_tokens` to Responses `max_output_tokens`.

Codex rejects `temperature` and `top_p`. vrouter validates them, drops them for Codex, and lists them in the `X-Vrouter-Ignored-Parameters` response header. Other accounts keep their sampling settings.

Stored Responses are unsupported. Set `store` to `false`.

Messages `metadata.user_id` has no equivalent on the other side. vrouter drops it and lists it in the same header.

Claude caches a prompt only when the request asks for it. vrouter asks for automatic caching on every Responses or Chat request it converts for Claude. Messages requests keep their own `cache_control`.

Responses and Chat clients may send `prompt_cache_key`. vrouter forwards it to Codex and drops it for Claude. A Codex subscription picks the cache from a `session-id` header instead, so vrouter sends the key there too. When a request has no key, vrouter derives one from the model, the instructions and the first user message, so every turn of a conversation shares it.

### Fast mode

Responses and Chat requests with `service_tier: "fast"` or `"priority"` become `speed: "fast"` when routed to Claude. Messages requests with `speed: "fast"` become `service_tier: "priority"` when routed to OpenAI or Codex. OpenAI accepts `fast` and `priority` as equivalent names. Explicit standard speed converts between Messages `speed: "standard"` and OpenAI `service_tier: "default"`. Chat-to-Responses conversion preserves the service tier.

vrouter adds Claude's `fast-mode-2026-02-01` beta header on fast requests. The selected model and account must support fast mode. Other OpenAI tiers without a Claude equivalent return HTTP 400 with an explanation. Provider rejections keep their sanitized message, error type, code, request ID and `Retry-After`, with fast-mode context added to the message.

Codex fast mode draws down subscription allowance faster. Claude fast mode uses paid usage credits and separate rate limits, so vrouter sends those requests without checking normal subscription windows or spending subscription usage resets. The account quota bars still show normal subscription allowance. Account retries stay within the same authentication mode and retain the requested speed.

Accounting uses the tier the provider reports serving, from Claude's `usage.speed` or OpenAI's response `service_tier`. When the response names no tier, the request's tier is recorded instead. The gateway prices fast requests at twice the list rate and OpenAI `ultrafast` requests at six times. It calculates cost per request before aggregation, including long-context thresholds and cache reads/writes, and stores the estimate as integer nanodollars. Later price changes do not recalculate historical costs. A fast request that Claude Opus 4.6 runs at standard speed, or that OpenAI downgrades to `default`, is priced at the standard rate.

Claude Code checks fast-mode availability directly with Anthropic, outside the configured base URL. When using a vrouter key through `ANTHROPIC_AUTH_TOKEN`, set `CLAUDE_CODE_SKIP_FAST_MODE_ORG_CHECK=1` so the request can reach vrouter. This only skips the client check; the selected upstream account still needs access and credits. See [Claude Code fast mode behind gateways](https://code.claude.com/docs/en/fast-mode#use-fast-mode-behind-proxies-and-llm-gateways) and [Codex fast mode](https://developers.openai.com/codex/speed/).

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

### Images

`/v1/images/generations` and `/v1/images/edits` follow the OpenAI Images API and run on Codex and OpenAI accounts. Subscription accounts use the ChatGPT image endpoint that the Codex CLI calls, and API-key accounts use the OpenAI API. Ask for an image model such as `gpt-image-2`. Image models are not listed in `/v1/models`.

Edits take JSON with `images` as a list of `{"image_url": ...}` or `{"file_id": ...}` entries, or the multipart form that the OpenAI SDKs send, with `image`, `image[]` and `mask` files in PNG, JPEG or WebP. vrouter turns uploaded files into data URLs. An image request may be up to 32 MiB.

Images always come back as `b64_json`. vrouter rejects `response_format: "url"` and `stream: true`. For Codex subscription accounts it drops `user` and lists it in `X-Vrouter-Ignored-Parameters`.

ChatGPT plans limit image generation separately, and vrouter does not track that limit. An account that answers HTTP 429, or fails on its own in another way, passes the request to the next one, and image requests never spend a usage reset. The usage page prices image tokens with the model's models.dev entry, which lists only the text input rate, so edits that send images come out a little low. A model without an entry counts as unpriced.

### Claude OAuth

Claude OAuth requests need the native Claude Code identity block. vrouter adds it ahead of the caller's system prompt and leaves the caller's blocks unchanged. This covers inference and automatic window starts. Paid API-key requests keep their original system prompt.

### Testing a model

Open a model in the UI to copy a request or run **Test this model** with a client key. The test calls the public inference endpoint and counts against that key. It shows the HTTP status, provider error, and reply, and passes only after a complete response. The key stays in memory until you close the dialog.

**Connected** on an account means vrouter has saved credentials. It does not prove inference works.

## Pool charts

The Overview charts each provider pool's allowance, summed across the accounts that report one. It follows the weekly window, or an account's first window when it has no weekly one. The solid line shows what providers reported over the last window. The dashed line projects the next window from a spending rate you pick above the pools: the last 3 hours, the last day, or the average since each window reset. The projection splits that rate evenly across the accounts with allowance left, the way requests rotate, and refills each account when its window resets. Until there is enough history for the rate you picked, it uses the average since the reset.

vrouter records a provider's report whenever it reads an account's usage. That happens while requests route through the pool and while the dashboard is open. Codex usage is read at most once a minute. Claude's usage endpoint rate limits a token that asks that often, so vrouter reads it at most every five minutes, and once more before and after it spends a reset. It takes the 5-hour, weekly and Fable windows from the rate-limit headers of each Claude response in between. After a refused read it waits five minutes, or longer when Claude says so. The dashboard keeps the last values meanwhile and marks an account only when they are more than 15 minutes old. A gap in the recorded line is time nobody looked, and the chart draws it straight. Usage spent outside vrouter during a gap shows up at the next report. vrouter keeps 30 days of reports in `usage.sqlite` and drops an account's history when you remove the account.

## Usage resets

The Accounts page lists the resets each subscription account holds under its name: Claude's 5-hour, weekly, full and Fable resets, and Codex reset credits. Open the list to use one. vrouter asks twice, because a reset cannot be undone. The provider decides whether it applies, and vrouter shows its answer.

A Fable reset gives back the weekly usage the Fable models spent and refills the 5-hour limit. Claude accepts it before the account reaches a limit. Other resets usually work only at a limit they refill, and the list says so.

### Automatic resets

When a client request finds every account in its provider pool blocked, vrouter refreshes their usage and looks for included resets.

It spends a reset only on an account with an exhausted 7-day allowance. Among those, it picks the account whose weekly allowance resets furthest in the future. An exhausted 5-hour window alone never triggers a reset. vrouter skips accounts with no reported weekly reset time or no reset available.

Claude's Opus, Sonnet and Fable weekly limits count when they block the requested model. The grant vrouter selects must clear every blocking limit on that account. When the Fable limit is the only weekly limit in the way, vrouter spends a Fable reset before a weekly or full one. It never spends a Fable reset on an exhausted weekly limit, because that only returns the Fable share and may leave the account blocked.

vrouter confirms fresh usable quota before it retries the request. A pending attempt keeps its request ID across restarts, and vrouter spends no other account's reset while that outcome is unknown.

The provider decides which windows a reset clears. The weekly-only rule only controls when vrouter spends one. Resets happen on demand, never from the background 5-hour window check.

## Automatic 5-hour windows

Every minute, vrouter checks each enabled subscription account for an active 5-hour window and sends a small request to the accounts without one. Accounts that are idle at the same check start together.

vrouter picks the smallest advertised model. That means Haiku for Claude, and nano then mini for Codex. It honors model exclusions and never substitutes a larger model. Catalogs do not expose prices, so this is a fixed preference and not a price comparison. Codex uses the lowest reasoning level the model advertises. Claude sends no thinking configuration and caps output at eight tokens.

vrouter does not recheck an account with an active window until its reported reset time. It skips plans without a 5-hour window and rechecks them hourly.

Codex Pro plans report a 5-hour entry without enforcing one, so vrouter also skips plans by name. `VROUTER_WINDOW_SKIP_PLANS` takes comma-separated `plan` or `provider:plan` entries and replaces the default `codex:pro*`. Matching ignores case and tests both the provider's plan value and the name shown on the account. A trailing `*` matches any ending, so the default covers every Codex Pro tier. Set the variable to `none` to trigger on every plan.

A trigger consumes both provider allowances where they apply. It is not charged to a client key and does not redeem resets. vrouter sends no trigger while another allowance on the account is exhausted.

After a failed check, a rejected trigger, or a trigger whose new reset time vrouter cannot confirm, it waits five minutes before trying that account again. It persists the cooldown, so a restart cannot repeat a trigger sooner. vrouter logs starts and failures.

## Management API

Management requests need the admin key as `Authorization: Bearer <key>`, or the dashboard's session cookie. A write that carries an `Origin` header must come from the public URL, and a write authorized by the cookie must carry one.

```sh
curl -H "Authorization: Bearer $VROUTER_ADMIN_KEY" https://your-host/api/keys
```

`GET` and `POST /api/keys` list and create keys. `PATCH` and `DELETE /api/keys/{id}` change and remove them. A creation body looks like this:

```json
{
  "name": "Alice",
  "expiresAt": "2027-01-01T00:00:00Z"
}
```

`expiresAt` takes a future RFC 3339 timestamp, or `null` for a key that never expires. Omitting it on an update keeps the current expiry.

`GET /api/usage?tz=<IANA time zone>` returns the all-time totals, the breakdowns by provider, model, and key, usage per day and per recent hour in that time zone, and the latest 1,000 requests. Without a known zone it uses UTC.

`GET /api/quota-history?window=weekly&days=7` returns how the named quota windows changed on each account, over 1 to 30 days, seven by default. Repeat `window` for up to eight windows. Each run holds the remaining percentage, the reset time, and when vrouter first and last saw that report, all times in Unix milliseconds.

`POST /api/accounts/{id}/resets` uses one reset on an account. For Claude the body names the grant, `{"grantId": "..."}`, with an ID from the account's `resets` list in `GET /api/state`. For Codex send `{}`. It answers 200 once the provider confirms, 409 with the provider's reason when it refuses, and 502 when the outcome is unknown. Repeating an unconfirmed request reuses its request ID.

`GET /api/auth` says whether the request is signed in. `POST /api/auth/session` with `{"key": "..."}` sets the session cookie, and `DELETE` clears it.

## Development

`devenv shell` enters the environment, and `devenv shell just build` runs one command in it. With direnv hooked into your shell, run `direnv allow` once and the environment activates when you enter the repo.

```sh
just dev                    # Go server on :8080, Vite on :5173
just build                  # Build UI and bin/vrouter
just check                  # golangci-lint, tsc, oxlint, and oxfmt without writing
just fmt                    # Format Go (gofumpt, goimports) and the UI (oxfmt)
just lint                   # golangci-lint and oxlint
```

CI runs the same checks, plus `nix flake check`, on every pull request. The linters are strict, and warnings count as errors.

`just dev` uses the normal persistent data directory. Set `VROUTER_DATA_DIR` to a scratch directory for experiments. The UI reloads as you edit, but Go changes need a restart of `just dev`.

Sign-in works through Vite too. Leave `VROUTER_PUBLIC_URL` unset, or set it to `http://localhost:5173`.

- `cmd/vrouter` has startup and import commands.
- `internal/gateway` has routing, provider adapters, authentication, storage, and management handlers.
- `web/src` has the React UI.
