# Contributing to Yolorouter

English · [简体中文](CONTRIBUTING_zh.md)

Thanks for your interest in improving Yolorouter! This guide covers how to set
up your environment, the coding standards we enforce, and how to get a change
merged.

By participating you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md).

## Getting started

Requirements:

- **Go 1.25.7+**
- **Node.js 22.12+**

```bash
git clone https://github.com/yolorouter/yolorouter.git
cd yolorouter

# Rebuild frontend + backend, run migrations, and (re)start the dev server.
./scripts/dev.sh
```

```powershell
# Same thing on Windows (PowerShell).
.\scripts\dev.ps1
```

Useful flags: `--backend`, `--frontend`, `--migrate`, `--restart` (`-Backend`,
`-Frontend`, `-Migrate`, `-Restart` on PowerShell). Run the script with `--help`
(`-Help`) for the full list; output language follows your locale and can be
forced with `YOLO_LANG=zh|en`.

### Local development and debugging

`./scripts/dev.sh` leaves a server running on `http://localhost:8080` and writes
its log to `logs/server.log` — that log is the first place to look when
something misbehaves. Configuration lives in `configs/config.yaml` and the
SQLite database in `data/yolorouter.db`, both generated on first run; delete the
database file to start over from a clean slate.

The tightest loops per side of the codebase:

- **Backend** — edit Go code, then `./scripts/dev.sh --backend` (rebuild +
  restart, skips the frontend). To run under a debugger or with extra flags,
  the same entry point works directly: `go run ./cmd/yolorouter serve`.
- **Frontend** — don't rebuild at all. `cd frontend && npm run dev` serves the
  console with hot reload on port 5173 and proxies `/healthz`, `/api`, and
  `/v1` to the backend (override the target with `VITE_BACKEND_TARGET`). The
  dev server listens on all interfaces so you can open it from a phone to test
  the mobile layout.

For request-level debugging — protocol conversion, provider failover, billing —
open the request log's detail page in the console: every relay stores the full
client request body, the upstream request body, and the upstream response body,
alongside the per-attempt routing chain (which provider and key each attempt
hit, and why it moved on).

### Building and testing

```bash
make build          # backend only -> ./bin/yolorouter
make build-embed    # full binary with the console embedded

# Cross-compilation (frontend embedded)
make build-macos            # -> ./bin/yolorouter-darwin-{amd64,arm64}
make build-windows          # -> ./bin/yolorouter-windows-{amd64,arm64}.exe
make build-windows-check    # fast compile check, no frontend build, no binary

make test           # go test ./...
make test-embed     # tests with the embedded-frontend build tag
make vet            # go vet (plain and -tags release)
```

## Project layout

```
cmd/yolorouter/     CLI entry point (serve, db:migrate, update, version)
internal/           Backend: handler → service → repository, middleware
  gateway/          The relay kernel: admission, candidate selection, dispatch,
                    delivery, settlement. Holds no feature logic of its own.
  capability/       One package per thing the gateway does to a request beyond
                    relaying it — rate limiting, content inspection, system
                    prompt injection, request logging, input compression,
                    output-ceiling clamping. Each registers itself with the
                    kernel at assembly (internal/router/capabilities.go) and
                    never imports it.
  decision/         The table that says what each observation costs a request:
                    whether to retry, what the caller sees, what is billed.
                    Capabilities report; this decides.
  fact/             The vocabulary capabilities report in — routing verdicts
                    and accounting records. Adding a record type here is how a
                    capability gets something onto the audit row.
  gates/            Structural checks that run as tests. They enforce rules a
                    linter cannot: error codes must be registered, the decision
                    table must be exhaustive, comments must not name symbols
                    that no longer exist. A pull request that trips one fails
                    here rather than in review — run `make gates` locally.
  loopback/         The process-level secret and header names for gateway
                    self-calls; imported by both the kernel and the
                    capability that calls back in, so neither imports the other.
  protocols/        Wire formats (OpenAI chat, Anthropic, Gemini, Responses)
                    and the intermediate form they convert through.
  selfupdate/       In-place binary upgrade mechanics (release lookup,
                    checksum verification, atomic replacement), shared by the
                    `update` CLI command and the admin update endpoint.
pkg/                Reusable packages (crypto, database, response, ...)
migrations/         goose migrations (sqlite/ and postgres/)
web/                go:embed of the built frontend
frontend/           Vue 3 + TypeScript admin console (Vite)
```

## Coding standards

### Go

- `gofmt` is mandatory. Run `gofmt -w` (or your editor's format-on-save) before committing.
- Code, comments, and string literals are written in **English**.
- Lint must pass:

  ```bash
  golangci-lint run      # config in .golangci.yml
  ```

- Tests and vet must pass, including build-tagged variants:

  ```bash
  make test              # go test ./...
  make vet               # go vet, plain + -tags release
  make test-release      # -tags release
  make test-embed        # -tags embed (requires a frontend build)
  ```

### Frontend (Vue / TypeScript)

- `naive-ui` components must be imported explicitly (no global auto-import).
- Icons use `@lucide/vue`.
- The build type-checks with `vue-tsc`; a red type-check fails CI:

  ```bash
  cd frontend && npm run build
  ```

## Commits & pull requests

- Use clear, conventional-style commit subjects where practical (e.g. `feat(gateway): ...`, `fix(auth): ...`).
- Keep PRs focused; one logical change per PR is easiest to review.
- Fill in the pull request template — what changed, why, and how you verified it.
- Ensure CI is green (test, lint, and the embedded build) before requesting review.
- New behavior should come with tests.

## Reporting bugs & requesting features

Open an issue using the appropriate template. For bugs, include your version
(`./yolorouter --version`), OS/arch, database driver, and clear reproduction
steps. For security issues, **do not** open a public issue — see
[SECURITY.md](SECURITY.md).

## License

By contributing, you agree that your contributions will be licensed under the
[Apache License 2.0](LICENSE).
