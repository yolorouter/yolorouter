# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0]

Initial release. The core loop is complete: configure providers, route with
failover, and observe usage and cost.

### Added

- OpenAI-compatible gateway: `POST /v1/chat/completions` with streaming (SSE) and function calling (`tools` / `tool_choice` / `parallel_tool_calls`).
- Multi-provider routing with ordered failover, keeping the public model name stable to the caller.
- Upstream API key pools with automatic rotation on rate-limit / auth-failure / quota-exhaustion.
- Model aliasing (public model name → per-provider model id) with per-candidate capability flags (streaming, function calling).
- API key management: model allowlist, request-rate and concurrency limits, cumulative budget cap, expiry, and instant revocation. Full key shown once on creation.
- Admin console: dashboard, usage & cost analytics (by model / provider / time / caller), and request logs with the full per-attempt routing trace. CSV export.
- First-run setup: create the initial admin, guided provider / model / key configuration.
- Bilingual admin UI (English / 简体中文).
- Single binary with the web console embedded via `go:embed`; SQLite or PostgreSQL storage; upstream keys encrypted at rest (AES-256).
- Self-update via the `update` command and update-check API.

[Unreleased]: https://github.com/yolorouter/yolorouter/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/yolorouter/yolorouter/releases/tag/v0.1.0
