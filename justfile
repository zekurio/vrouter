# Build the embedded UI and Go binary.
build:
    pnpm --dir web install --frozen-lockfile
    pnpm --dir web build
    CGO_ENABLED=0 go build -trimpath -o bin/vrouter ./cmd/vrouter

# Go on :8080 and Vite on :5173. Restart after Go changes.
dev:
    #!/usr/bin/env bash
    set -euo pipefail
    test -d web/node_modules || pnpm --dir web install --frozen-lockfile
    test -f web/dist/index.html || pnpm --dir web build
    CGO_ENABLED=0 go build -trimpath -o bin/vrouter ./cmd/vrouter
    node scripts/run-local.mjs &
    backend_pid=$!
    trap 'kill "$backend_pid" 2>/dev/null || true; wait "$backend_pid" 2>/dev/null || true' EXIT
    pnpm --dir web dev

dev-web:
    pnpm --dir web dev
