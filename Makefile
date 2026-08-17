.PHONY: build build-embed build-release build-windows build-windows-check build-macos frontend embed-frontend test test-release test-embed vet vet-embed gates dev migrate

# Release-build metadata injected via -ldflags into internal/version (the
# package both the `--version` CLI flag and the system-info API read from).
# A plain `go build` / `make build` leaves the "dev"/"unknown" defaults, so
# only build-release (and goreleaser) carry real values.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDTIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# The canonical "owner/repo" GitHub release source, baked into release builds
# as version.DefaultGitHubRepo. Empty in local builds (the update feature then
# relies on config.update.github_repo, or is disabled). The public release
# workflow injects the real value (see .github/workflows/release.yml).
DEFAULT_GITHUB_REPO ?=
VERSION_PKG := github.com/yolorouter/yolorouter/internal/version

# Plain build: no -tags embed, so it never requires web/dist/ to contain
# anything (see web/embed_stub.go) — web/dist/ is 100% gitignored with no
# tracked exceptions. Serves web/placeholder.html for every request.
build:
	go build -o ./bin/yolorouter ./cmd/yolorouter

# Cross-compile check: the project must always build for windows so the
# platform-split instance lock / stop / config-permission paths never regress.
# No embed needed (and so no npm) — this only verifies compilation, which is
# what makes it cheap enough to run on its own. Produces no artifact; use
# build-windows for that.
build-windows-check:
	GOOS=windows GOARCH=amd64 go build ./...

# Cross-compile runnable Windows binaries for both architectures goreleaser
# publishes, with the real frontend embedded — enough to test the whole flow
# on an actual Windows machine (first-run config generation, migrations,
# console, gateway). Requires embed-frontend first, like build-macos.
build-windows: embed-frontend
	GOOS=windows GOARCH=amd64 go build -tags embed -o ./bin/yolorouter-windows-amd64.exe ./cmd/yolorouter
	GOOS=windows GOARCH=arm64 go build -tags embed -o ./bin/yolorouter-windows-arm64.exe ./cmd/yolorouter

# Cross-compile macOS binaries for both Intel and Apple Silicon, with the
# real frontend embedded. Requires embed-frontend (npm build + copy to
# web/dist/) first.
build-macos: embed-frontend
	GOOS=darwin GOARCH=amd64 go build -tags embed -o ./bin/yolorouter-darwin-amd64 ./cmd/yolorouter
	GOOS=darwin GOARCH=arm64 go build -tags embed -o ./bin/yolorouter-darwin-arm64 ./cmd/yolorouter

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
	go build -tags embed -o ./bin/yolorouter ./cmd/yolorouter

# RELEASE_TAG is the exact git tag at HEAD (empty if HEAD isn't tagged).
# Evaluated at make-parse time so every recipe line sees the same value — a
# recipe-local `TAG=$$(...)` would be lost on the next line because make runs
# each recipe line in a fresh shell. git-describe strings (v1.2.3-dirty /
# v1.2.3-4-gabc) are rejected: they're semver prereleases ranked below the
# tag, which would let the updater downgrade a newer build to the older tag.
RELEASE_TAG := $(shell git describe --tags --exact-match HEAD 2>/dev/null)

build-release: embed-frontend
	@if [ -z "$(RELEASE_TAG)" ]; then \
		echo "ERROR: build-release requires HEAD to be an exact git tag (e.g. v0.1.0)" >&2; \
		echo "       git-describe strings (v1.2.3-dirty / v1.2.3-4-gabc) are semver prereleases," >&2; \
		echo "       ranked below their release, which would let the updater downgrade a" >&2; \
		echo "       newer build to the older tag. Tag first: git tag v0.1.0" >&2; \
		echo "       (release publishing uses goreleaser, not this target)" >&2; \
		exit 1; \
	fi
	@if ! printf '%s' "$(RELEASE_TAG)" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$$'; then \
		echo "ERROR: build-release tag '$(RELEASE_TAG)' must be canonical release semver (v1.2.3, no leading zeros, no prerelease)" >&2; \
		echo "       currentUpdatable rejects non-semver and prerelease versions; a non-canonical" >&2; \
		echo "       tag would build a binary the update checker can never compare." >&2; \
		exit 1; \
	fi
	@if ! git diff-index --quiet HEAD --; then \
		echo "ERROR: build-release requires a clean worktree (no uncommitted changes)" >&2; \
		echo "       git describe --tags --exact-match ignores dirty state, so a tagged HEAD" >&2; \
		echo "       with modified sources still ships as a clean vX.Y.Z, defeating the" >&2; \
		echo "       downgrade/provenance guard. Commit or stash first." >&2; \
		exit 1; \
	fi
	@if [ -n "$$(git ls-files --others --exclude-standard)" ]; then \
		echo "ERROR: build-release requires a clean worktree (no untracked source files)" >&2; \
		echo "       untracked .go / frontend sources are built but invisible to git" >&2; \
		echo "       diff-index, so a tagged HEAD with untracked files still ships as a clean" >&2; \
		echo "       vX.Y.Z. Commit or remove them first." >&2; \
		exit 1; \
	fi
	go build -tags release,embed -ldflags "-X $(VERSION_PKG).Version=$(RELEASE_TAG) -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).BuildTime=$(BUILDTIME) -X $(VERSION_PKG).DefaultGitHubRepo=$(DEFAULT_GITHUB_REPO)" -o ./bin/yolorouter ./cmd/yolorouter

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

# Structural gates that keep the gateway kernel decoupled from the
# capabilities plugged into it. Three layers, each covering what the others
# cannot:
#
#   - depguard (via golangci-lint) enforces the import directions: capability
#     packages stay off gin and the kernel, the kernel stays off concrete
#     capability/modality packages, and the fact package stays stdlib-only.
#     Rules and rationale live in .golangci.yml.
#   - internal/gates holds source-level checks no off-the-shelf linter
#     expresses: the fact vocabulary's routing/accounting split, record
#     consumers accounting for every field, and the candidate loop's
#     per-iteration state resets.
#   - The named -run lines run the behavioural halves that live next to the
#     code they exercise. `go test -run` exits 0 when a pattern matches
#     nothing, so these names alone cannot detect their own disappearance —
#     TestBehaviouralGateTestsStillExist inside internal/gates asserts each
#     name still exists, and fails loudly when one is renamed away.
gates:
	golangci-lint run --enable-only depguard ./...
	go test ./internal/gates/... -count=1 -v
	go test ./internal/decision/ -run '^(TestDecisionTableIsComplete|TestCombineIsCommutative|TestCombineIsAssociative|TestCombineNeverProducesUndefined|TestAVerdictIsRememberedOnlyByTheFactThatSuppliedIt)$$' -count=1 -v
	go test ./internal/gateway/ -run '^(TestObserversCannotReachEachOtherOrTheAuditBody|TestEveryCountSurvivesTheDeliveryHop|TestAVerdictDoesNotOutliveTheKeyThatEarnedIt|TestFoldUpstreamDecision|TestKernelBaselineTimelineEntries|TestReportKernelFactStampsProvenanceAndFolds|TestSettlementNotesCarryKernelProvenance)$$' -count=1 -v
	go test ./internal/fact/ -run '^TestValidateRejectsImpossibleDeliveries$$' -count=1 -v

migrate: build
	./bin/yolorouter db:migrate

dev:
	./scripts/dev.sh
