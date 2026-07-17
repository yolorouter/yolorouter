.PHONY: build build-embed build-release frontend embed-frontend test test-release test-embed vet vet-embed dev migrate

# Design doc §2.1: --version must print a real, non-"dev" version in
# production builds, injected via -ldflags rather than hand-editing the
# var version = "dev" default in commands.go for each release.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Plain build: no -tags embed, so it never requires web/dist/ to contain
# anything (see web/embed_stub.go) — web/dist/ is 100% gitignored with no
# tracked exceptions. Serves web/placeholder.html for every request.
build:
	go build -o ./bin/yolorouter-ce ./cmd/yolorouter-ce

# Removes frontend/dist first so a misconfigured build (e.g. an outDir
# typo in vite.config that leaves npm run build writing somewhere else
# entirely) can't leave a stale, previously-successful build sitting there
# — without this, embed-frontend's index.html check below would pass
# against leftover output from a prior run instead of catching the break.
frontend:
	rm -rf frontend/dist
	cd frontend && npm ci && npm run build

# Clears everything in web/dist/ (all gitignored — there is nothing tracked
# in there to protect) and repopulates it from a real frontend build. Asserts
# index.html landed: router.New()'s startup check catches this at serve time
# too, but failing here means build-embed/build-release never even produce a
# binary carrying a broken embedded frontend in the first place.
embed-frontend: frontend
	rm -rf web/dist
	mkdir -p web/dist
	cp -r frontend/dist/. web/dist/
	test -f web/dist/index.html || { echo "embed-frontend: web/dist/index.html missing after frontend build" >&2; exit 1; }

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

# web/embed_real.go (//go:build embed) only compiles under -tags embed —
# neither `test` nor `test-release` above ever touches it, so a break
# there (e.g. a typo in the "all:dist" pattern) would only surface at
# `make build-embed`/`build-release` time. Depends on embed-frontend since
# -tags embed requires web/dist/ to actually contain a build.
test-embed: embed-frontend
	go test -tags embed ./... -v

vet:
	go vet ./...
	go vet -tags release ./...

# See test-embed above for why this needs its own target.
vet-embed: embed-frontend
	go vet -tags embed ./...

migrate: build
	./bin/yolorouter-ce db:migrate

dev:
	./scripts/dev.sh
