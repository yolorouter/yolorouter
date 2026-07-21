<div align="center">

# Yolorouter

**A self-hosted, OpenAI-compatible LLM gateway with multi-provider failover, key rotation, and a built-in admin console.**

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![CI](https://github.com/yolorouter/yolorouter/actions/workflows/ci.yml/badge.svg)](https://github.com/yolorouter/yolorouter/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/yolorouter/yolorouter)](https://goreportcard.com/report/github.com/yolorouter/yolorouter)
[![Release](https://img.shields.io/github/v/release/yolorouter/yolorouter?sort=semver)](https://github.com/yolorouter/yolorouter/releases)
[![Go](https://img.shields.io/badge/go-1.26+-00ADD8.svg)](go.mod)

English · [简体中文](README_zh.md)

</div>

---

Point your application at **one** endpoint and **one** API key. Yolorouter sits
between your apps and your upstream providers, so the messy parts — juggling
provider accounts, rotating rate-limited keys, failing over when an account
breaks, enforcing per-key budgets, and knowing what everything costs — live in
one place instead of scattered across every codebase.

It speaks the OpenAI Chat Completions API (streaming and function calling
included), so it's a drop-in replacement: change the base URL and the key,
nothing else.

Everything ships as a **single binary** with the web console embedded. No
Node runtime, no separate frontend deploy, no external services required —
SQLite works out of the box, PostgreSQL when you want it.

## Why Yolorouter

- **Drop-in OpenAI compatibility** — `POST /v1/chat/completions`, streaming SSE, and `tools` / `tool_choice` function calling pass straight through.
- **Multi-provider failover** — map one public model name (e.g. `smart`) to an ordered list of provider candidates. When one is down, requests fail over to the next — transparently, without the caller ever seeing a different model name.
- **Upstream key rotation** — give each provider a pool of upstream keys. Rate-limited, unauthorized, or quota-exhausted keys are skipped automatically; the request retries the next key before failing over.
- **Model aliasing** — callers request a stable public name; each provider candidate maps it to whatever model id that provider actually expects.
- **Per-key access control** — every issued key carries a model allowlist, request-rate / concurrency limits, a cumulative budget cap, and an optional expiry. Revoke instantly.
- **Streaming done right** — key rotation and failover happen *before* the first byte reaches the client; once streaming starts, the provider is locked in. Content from two providers is never stitched into one response.
- **Observability built in** — a dashboard, usage & cost analytics (by model / provider / time / caller), and full request logs with the complete per-attempt routing trace. Export any view to CSV.
- **Bilingual admin console** — English and 简体中文, switchable anywhere, before or after login.
- **Self-updating** — the binary can check for and apply new releases.

## Screenshots

<!-- Drop real screenshots into docs/screenshots/ and uncomment:
<p align="center">
  <img src="docs/screenshots/dashboard.png" width="49%" alt="Dashboard" />
  <img src="docs/screenshots/request-log-detail.png" width="49%" alt="Request log detail" />
</p>
<p align="center">
  <img src="docs/screenshots/model-routing.png" width="49%" alt="Model routing" />
  <img src="docs/screenshots/analytics.png" width="49%" alt="Usage & cost analytics" />
</p>
-->

_Screenshots coming soon._

## Quick start

### Run a release binary

Download the archive for your platform from the
[latest release](https://github.com/yolorouter/yolorouter/releases), extract it, then:

```bash
./yolorouter serve
```

On first run it generates `configs/config.yaml` (including a random AES-256
master key used to encrypt stored upstream keys), applies database migrations,
and serves the console on <http://localhost:8080>. Open it, create the first
admin account, and follow the setup flow: add a provider and an upstream key,
create a model with its provider candidates, then issue an API key.

### Call it

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-yr-your-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "smart",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

`model` is the public name you configured — Yolorouter picks the provider,
substitutes the real upstream model id, and returns an OpenAI-compatible
response with the public model name preserved. Add `"stream": true` for SSE,
or a `tools` array for function calling.

## Configuration

Configuration lives in `configs/config.yaml`, auto-generated on first run. You
rarely need to edit it by hand.

```yaml
server:
  port: 8080
database:
  driver: sqlite          # sqlite | postgres
  sqlite_path: ../data/yolorouter.db
  # host/port/user/password/dbname/sslmode apply when driver: postgres
security:
  provider_master_key: "" # base64 AES-256 key; auto-generated when blank
update:
  enabled: true           # set false to disable the update-check API and CLI
  github_repo: ""          # "owner/repo" override for update checks
```

See [`configs/config.example.yaml`](configs/config.example.yaml) for the full
annotated reference. If the config file already exists, `provider_master_key`
must be a real key — it is only auto-filled on the initial generate path.

CLI subcommands:

```bash
./yolorouter serve         # start the HTTP server
./yolorouter db:migrate    # apply migrations
./yolorouter update        # self-update to the latest release
./yolorouter --version
```

## Build from source

Requirements: **Go 1.26+** and **Node.js 22.12+**.

```bash
# Backend only — serves a placeholder page instead of the console
make build          # -> ./bin/yolorouter

# Full binary with the web console embedded
make build-embed    # -> ./bin/yolorouter (frontend built + embedded)
```

## Development

```bash
./scripts/dev.sh              # rebuild frontend + backend, migrate, (re)start
./scripts/dev.sh --backend    # backend only
./scripts/dev.sh --frontend   # frontend only

make test                     # go test ./...
make vet                      # go vet (plain + -tags release)
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full workflow and coding standards.

## Architecture

```
┌────────────┐    OpenAI-compatible     ┌─────────────────────────────┐    ┌────────────┐
│  your app  │ ───────────────────────▶ │          Yolorouter          │ ─▶ │ provider A │
└────────────┘   /v1/chat/completions   │                              │ ─▶ │ provider B │
                                        │  auth · limits · budget      │    │    ...     │
┌────────────┐        admin UI          │  model alias · candidate     │    └────────────┘
│  operator  │ ───────────────────────▶ │  routing · key rotation      │
└────────────┘   embedded Vue console   │  failover · logging · cost   │
                                        └──────────────┬───────────────┘
                                                       │
                                                 SQLite / PostgreSQL
```

- **Backend** — Go ([Gin](https://gin-gonic.com/) + [GORM](https://gorm.io/)), migrations via [goose](https://github.com/pressly/goose). Layered handler → service → repository.
- **Frontend** — Vue 3 + TypeScript + [naive-ui](https://www.naiveui.com/), built with Vite and embedded into the binary via `go:embed`.
- **Storage** — SQLite (pure-Go, zero-config) or PostgreSQL. Upstream keys are encrypted at rest with AES-256.

## Status & scope

Yolorouter is at **v0.1** — the core loop (configure providers → route with
failover → observe usage and cost) is complete and tested. It targets the
OpenAI Chat Completions API.

Out of scope for now: non-OpenAI request formats (Claude / Gemini), image
understanding, and circuit-breaker state machines. Cost figures are displayed
in a single currency in v0.1. See the roadmap for what's next.

## Contributing

Issues and pull requests are welcome. Please read
[CONTRIBUTING.md](CONTRIBUTING.md) and our
[Code of Conduct](CODE_OF_CONDUCT.md) first. To report a security issue, see
[SECURITY.md](SECURITY.md).

## License

Licensed under the [Apache License 2.0](LICENSE).
