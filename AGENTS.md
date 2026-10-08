# Working on vrouter

vrouter is a Go model gateway with a React/TypeScript UI embedded in one binary. Start with `README.md` for setup and `.env.example` for access configuration.

## Commands

- Use `devenv shell`, or install Go 1.26+ and Node 22.12+ or 24 with pnpm and just. For one command, use `devenv shell <command>`. The environment is defined in `devenv.nix`, with inputs pinned in `devenv.lock`; `.envrc` supports automatic activation with direnv.
- `just build` installs frontend dependencies, type-checks/builds the UI, then builds `bin/vrouter` with CGO disabled.
- `just dev` runs Go on `:8080` and Vite on `:5173`. Only the UI hot-reloads; restart after Go changes. `just dev-web` starts only Vite.
- Check Go changes with `go vet ./...` and `just build`; check UI changes with `pnpm --dir web build`. Run `go test ./...` for backend tests. There are no frontend test/lint scripts. Format changed Go files with `gofmt` and changed UI files with Prettier.

## Where changes belong

- `cmd/vrouter/main.go` handles startup and credential import. `internal/gateway` contains the backend in one package.
- `server.go` registers management routes on both the authorized public mux and internal `mgmt` mux; `management.go` serves the dashboard; `tenancy.go` dispatches requests through gateway views of the shared store. Preserve local/token and external-auth paths when adding routes.
- `inference.go` handles inference; `providers.go`, `oauth.go`, and `claude_profile.go` contain provider-specific behavior. Provider account OAuth is separate from dashboard access, which uses a reverse proxy or an admin token.
- `web/src/api.ts` owns management requests; `Workspace.tsx` owns per-gateway UI state. Go JSON shapes and TypeScript types are maintained manually, so update both when changing contracts.

## Quirks to preserve

- Build the UI before compiling a runnable Go binary. `web/embed.go` embeds `web/dist`; the tracked `.gitkeep` lets Go compile without a UI, but startup requires `index.html`. `just dev` only builds missing UI assets, so its embedded UI can be stale. Generated assets and `bin` are ignored.
- `.env` is not loaded automatically. `just dev` uses the normal persistent data directory. Create client keys through management. Set `VROUTER_DATA_DIR` to a scratch directory for experiments.
- State contains unencrypted provider credentials. Storage uses private permissions, atomic writes, and process locks. Use the store APIs, keep secrets out of responses/logs, and stop the server before CLI imports into the same directory.
- `data.go` owns the shared `vrouter.json` file. Account and registry stores are scoped views of it. This testing build has no automatic migrations. All client keys use managed records; environment and old local keys are not read.
- Management selects a gateway with `X-Vrouter-Gateway`; inference selects it from the API key. Admin tokens and inference keys are distinct. Preserve the browser Host in `/api` and `/auth` proxies because management writes check origin.
- Switching gateways remounts `Workspace` and closes its API client. Closed clients reject stale reads but allow `DELETE` to clean up provider sign-in sessions. Preserve that exception.
- Live inference supports `/v1/models`, `/v1/responses`, `/v1/messages`, and `/v1/chat/completions`. Select the provider from the model before quota admission. Protocol adapters must preserve tool calls, streamed output, terminal events, and raw upstream usage. Reject features that cannot be converted. `/v1beta` is unsupported.
- Keep inference account pools within one auth mode. Falling back from subscription credentials to paid API keys would cross a billing boundary.
- `telemetry.go` reserves requests before forwarding and settles usage afterward. Keys have no lifetime request or token limits; an optional `expiresAt` refuses calls at authentication and again at reservation. Provider percentage quotas use separate 5-hour and weekly accounting in `key_percent.go`. Uncertain usage blocks percentage-limited keys, including unfinished reservations recovered after restart, until an explicit provider-quota update clears that flag; preserve these accounting rules.
