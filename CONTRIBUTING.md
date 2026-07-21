# Contributing to Yolorouter

Thanks for your interest in improving Yolorouter! This guide covers how to set
up your environment, the coding standards we enforce, and how to get a change
merged.

By participating you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md).

## Getting started

Requirements:

- **Go 1.26+**
- **Node.js 22.12+**

```bash
git clone https://github.com/yolorouter/yolorouter.git
cd yolorouter

# Rebuild frontend + backend, run migrations, and (re)start the dev server.
./scripts/dev.sh
```

Useful `./scripts/dev.sh` flags: `--backend`, `--frontend`, `--migrate`,
`--restart`.

## Project layout

```
cmd/yolorouter/     CLI entry point (serve, db:migrate, update, version)
internal/           Backend: handler → service → repository, gateway, middleware
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
