.PHONY: build build-embed build-release frontend embed-frontend test test-release vet dev migrate

# Design doc §2.1: --version must print a real, non-"dev" version in
# production builds, injected via -ldflags rather than hand-editing the
# var version = "dev" default in commands.go for each release.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Plain build: no -tags embed, so it never requires web/dist/ to contain
# anything (see web/embed_stub.go) — web/dist/ is 100% gitignored with no
# tracked exceptions. Serves web/placeholder.html for every request.
build:
	go build -o ./bin/yolorouter-ce ./cmd/yolorouter-ce

frontend:
	cd frontend && npm ci && npm run build

# Clears everything in web/dist/ (all gitignored — there is nothing tracked
# in there to protect) and repopulates it from a real frontend build.
embed-frontend: frontend
	rm -rf web/dist
	mkdir -p web/dist
	cp -r frontend/dist/. web/dist/

# Same binary as `build`, but with -tags embed so it actually embeds and
# serves the real frontend build (see web/embed_real.go) — useful for
# testing the full go:embed pipeline locally without also wanting
# build-release's other effects (db:reset disabled, version injection).
build-embed: embed-frontend
	go build -tags embed -o ./bin/yolorouter-ce ./cmd/yolorouter-ce

build-release: embed-frontend
	go build -tags release,embed -ldflags "-X main.version=$(VERSION)" -o ./bin/yolorouter-ce ./cmd/yolorouter-ce

test:
	go test ./... -v

# db_reset_release.go (and its _test.go, both //go:build release) only
# compile under -tags release — the plain `test` target above silently
# never runs them, so a regression there wouldn't be caught by the usual
# test command.
test-release:
	go test -tags release ./... -v

vet:
	go vet ./...
	go vet -tags release ./...

migrate: build
	./bin/yolorouter-ce db:migrate

dev:
	./scripts/dev.sh
