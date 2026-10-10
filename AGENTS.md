# Working on vrouter

vrouter is a Go model gateway (`internal/gateway`) with a React UI (`web`) embedded in one binary. `README.md` covers setup.

## Commands

- Run tools through `devenv shell <command>`, or install Go 1.26+, Node 22.12+, pnpm and just yourself.
- `just build` builds the UI and then `bin/vrouter`. `just dev` runs Go on `:8080` and Vite on `:5173`. Restart it after Go changes.
- Run `just check` before you finish. It runs golangci-lint (gofumpt, goimports, go vet, staticcheck, gosec, and more), `tsc` (TypeScript 7, the Go-native compiler), oxlint, and oxfmt without writing files. `just fmt` formats Go and the UI. Lint rules live in `.golangci.yml` and `web/.oxlintrc.json`. Both are strict and treat warnings as errors. Fix findings instead of silencing them. When a finding is wrong, use a `//nolint:<linter> // reason` or `// oxlint-disable-next-line <rule> -- reason` comment on that one line.
- The repository has no automated tests. Don't add any.
- CI (`.github/workflows/ci.yml`) runs golangci-lint, the UI checks and build, and `nix flake check` on pushes to main and on pull requests. `nix flake check` fails when `web/pnpm-lock.yaml` changes without a matching hash in `nix/package.nix`.

## Things to know

- Go embeds `web/dist`, so build the UI before building a binary you want to run.
- `vrouter.json` in the data directory stores provider credentials in plain text. Set `VROUTER_DATA_DIR` to a scratch directory when you experiment, and keep secrets out of responses and logs.
- Go JSON types and the TypeScript types in `web/src` are kept in sync by hand. Change both.
- Don't mix subscription accounts and API-key accounts in one inference pool. Falling back from one to the other crosses a billing boundary.
