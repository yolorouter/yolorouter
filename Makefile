.PHONY: build build-release frontend embed-frontend test test-release vet dev migrate

# Design doc §2.1: --version must print a real, non-"dev" version in
# production builds, injected via -ldflags rather than hand-editing the
# var version = "dev" default in commands.go for each release.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -o ./bin/yolorouter-ce ./cmd/yolorouter-ce

# build-release must embed the real frontend build, not whatever placeholder
# or stale copy currently sits in internal/web/dist/ — a plain
# `go build -tags release` here would silently ship whatever's already
# there (design doc §7: npm run build -> copy into internal/web/dist ->
# go build is one chained pipeline for the production target).
frontend:
	cd frontend && npm ci && npm run build

embed-frontend: frontend
	rm -rf internal/web/dist
	mkdir -p internal/web/dist
	cp -r frontend/dist/. internal/web/dist/

build-release: embed-frontend
	go build -tags release -ldflags "-X main.version=$(VERSION)" -o ./bin/yolorouter-ce ./cmd/yolorouter-ce

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
