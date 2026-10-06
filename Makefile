.PHONY: build demo test dev dev-web

build:
	npm ci --prefix web
	npm run build --prefix web
	CGO_ENABLED=0 go build -trimpath -o bin/vrouter ./cmd/vrouter

demo: build
	VROUTER_DEMO=1 ./bin/vrouter

test:
	go test -race ./...
	go vet ./...
	npm run build --prefix web

# Single local server on :8080 plus Vite with hot reload on :5173.
dev:
	test -d web/node_modules || npm ci --prefix web
	test -f web/dist/index.html || npm run build --prefix web
	CGO_ENABLED=0 go build -trimpath -o bin/vrouter ./cmd/vrouter
	trap 'kill 0' EXIT; node scripts/run-local.mjs & npm run dev --prefix web

dev-web:
	npm run dev --prefix web
