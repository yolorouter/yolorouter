# Yolorouter — Frontend

The admin console for [Yolorouter](../README.md): a Vue 3 + TypeScript SPA
built with Vite and [naive-ui](https://www.naiveui.com/).

In production this app is compiled and embedded into the single Go binary via
`go:embed` (see [`../web`](../web)). You only need the steps below for frontend
development.

## Requirements

- Node.js >= 22.12

## Develop

```bash
npm ci
npm run dev      # Vite dev server with hot reload
```

Point the dev server at a running backend (see the repository root
[`README.md`](../README.md) → *Development* for the full-stack workflow via
`./scripts/dev.sh`).

## Build

```bash
npm run build    # type-check (vue-tsc) + production build into dist/
```

To produce a binary with the UI embedded, use the repository root:

```bash
make build-embed
```

## Conventions

- Components from `naive-ui` must be imported explicitly.
- Icons use `@lucide/vue`.
- See the repository [`CONTRIBUTING.md`](../CONTRIBUTING.md) before opening a PR.
