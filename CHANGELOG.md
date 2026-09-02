# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- DashScope dialect detection covers Model Studio workspace domains
  (`{workspaceId}.{region}.maas.aliyuncs.com`), with the matching docs
  updates.

- Image editing. `POST /v1/images/edits` accepts the OpenAI multipart
  upload (reference images, mask, prompt) and forwards it to
  OpenAI-compatible providers with only the model field rewritten, the
  re-encode cached across failover attempts. DashScope providers are served
  through the native multimodal-generation dialect with the uploaded images
  carried as base64 data URIs; that dialect has no mask field, so a masked
  ask is refused per candidate rather than silently dropped. Settlement
  reuses the per-image rules, and the audit row renders the upload's shape
  — file parts become size notes — instead of its pixels.

- Image streaming. `stream=true` on the images endpoints is served as
  named-event SSE passthrough for `gpt-image-*` models (partial_image and
  completed events, on both the generation and the edit route); any other
  model keeps the 400. Usage is read from the completed events, and an
  incomplete delivery — the upstream's own error event, a stream that never
  completes an image, a broken read — bills nothing.

- Edit-shaped models are probed with a reference image attached (multipart
  on OpenAI-compatible bases, a data-URI content item in the native
  dialect), so importing e.g. qwen-image-edit no longer measures the edit
  family's own input rule as a probe failure.

### Changed

- The price-catalog refresh pipeline moved to its own data repository
  ([yolorouter/price-catalog](https://github.com/yolorouter/price-catalog));
  this repo no longer takes daily price-refresh commits, and the embedded
  seed is refreshed once per release instead of daily.

### Fixed

- Cache prices in the price catalog now deserialize: the JSON keys were
  camelCase while the Go reader expects snake_case, so `cache_write` /
  `cache_read` prefill suggestions were silently empty on both the embedded
  seed and the live-refreshed catalog.

## [0.2.1] - 2026-09-01

### Added

- Image generation is a first-class routing target. Providers expose
  image models behind `/v1/images/generations`, spoken either as the
  OpenAI images shape or as the DashScope native multimodal-generation
  dialect, chosen per provider; the credential probe speaks the same
  dialect as delivery, so a passing key means a deliverable image.
  Image models bill per delivered image against a quality/size tier
  table instead of per token (migrations 00039-00040); each request log
  carries the settlement snapshot — axes, delivered count, unit price —
  that priced it, so per-image costs stay auditable after tier edits.
  Output-modality declarations flow through model create, edit, batch
  create and bulk import, gate which endpoints a model's pool may use,
  and a bulk import refuses rows contradicting the modality a model
  already declared, surfacing the skip and its reason in the import
  progress view. The provider detail page prices image-billed mappings
  per delivered image and badges unpriced rows by billing mode.

## [0.2.0] - 2026-08-28

### Added

- Providers and their keys can now be deleted. Deleting a provider removes
  its keys and model candidates in one transaction while request history is
  fully retained — reports keep the traffic under the deleted provider's id
  with an empty name, and log details still show the recorded provider/key
  names. The confirmation dialog previews the blast radius (keys, candidates,
  affected models, models left with no routable source) and requires retyping
  the provider's name. Keys can be deleted at any time, with a warning when
  removing the provider's last usable key. Migration 00036 lifts the foreign
  key that request history held on providers; on SQLite this rebuilds the
  request_logs table, which transiently needs free disk on the order of that
  table's size.

## [0.1.9] - 2026-08-26

### Added

- Request logs capture the exact prices each request was settled at
  (input / output / cache read / cache write, migration 00033), and the
  log detail page shows the snapshot — historical costs stay auditable
  after catalog price changes.

- The dashboard gains an admin-only verified cache economics row for the
  selected window: hit rate, net saving, read saving and write premium,
  computed only over providers whose traffic and pricing actually support
  caching, with a disclosure of the providers excluded for missing cache
  metering or prices (new `/api/analytics/cache-stats`; partial indexes in
  migration 00034, created concurrently on PostgreSQL).

- Cache hit rate and net cache saving appear as columns in every analytics
  report dimension and in the cost drilldown tables; CSV exports append the
  two settled amounts after the existing columns. A shared net-saving card
  joins the analytics overview row and the cost pages' overview rows, and
  every cache figure renders an em-dash — never a fabricated 0% or ¥0.00 —
  until the window has actually recorded cache metering.

- Terminal output compression covers more runners: pytest, vitest / jest
  and npm / pnpm install logs fold their passing checks and noise lines
  into count markers while failures, errors and summaries stay verbatim,
  and the go test / pytest fold markers now carry the slowest folded
  duration.

- The setup administrator can edit any other account's display name and
  email from the Users page (both were write-once: set at creation or
  snapshotted from the identity provider's first login) — directory
  information only, so the target's sessions are untouched.

### Changed

- The cost optimization page's Concise Output card now reports the
  period's total saved cost and output tokens for the selected window
  (previously a per-million-token projection).
- The New Model dialog now defaults the scheduling mode to Balanced
  (spread by caller key) each time it opens. Failover is still selectable,
  and the API default for requests that omit the field stays `failover`.
- The Zhipu GLM preset group in the New Model dialog now lists `glm-5.3`
  and `glm-5-turbo` first; all previously listed models remain selectable.

### Fixed

- Provider key tests over the Gemini and Responses protocols now certify
  a key only against a real success body: an upstream answering 200 with
  a placeholder or empty response no longer marks the key as verified.
  Results recorded before this change may show a prompt to re-test.

## [0.1.8] - 2026-08-25

### Added

- Admins can provision local username/password accounts straight from the
  Users page — the new "Add user" dialog creates an enabled member that
  signs in with the ordinary login form, with an optional display name and
  a hand-typed or randomly generated initial password (the admin hands it
  to the user out of band; only the bcrypt hash is stored). The
  first-run-setup account is now marked as the bootstrap account (new
  `is_bootstrap` flag, migration 00032): it stays the one account that can
  never be disabled or demoted — the OAuth-failure escape hatch — while
  every other local account is fully manageable (promote, demote,
  disable) like any externally-provisioned one. Previously the local
  password account was unique by schema and admins had no way to create
  one. The create dialog also takes an optional, informational email
  (recorded for the directory; no mail is ever sent), and the bootstrap
  administrator can reset another local account's password from the same
  page — promoted admins manage roles and status but never credentials,
  and a reset ends every live session of the target.

- The API Keys page now shows where to point clients at: the gateway base
  URL in both its OpenAI-compatible (`{base}/v1`) and Anthropic-compatible
  forms, plus a copy-ready example request. The address is resolved
  server-side — the configured `server.external_url` when set, derived from
  the request otherwise — so it stays right on reverse-proxied, LAN and
  container deployments where the console's own origin is not the gateway's.
  The create-key dialog repeats it on the one-time plaintext step with the
  fresh key already filled into the example, and CC-Switch profile exports
  now carry the same resolved address instead of the console's own origin.
  Previously the address the keys were for appeared nowhere in the console,
  and an exported profile could point somewhere the gateway does not answer.

- Model mappings can be imported from a provider in bulk. The provider page
  lists what the upstream offers and imports the selection in one
  transaction, resolving prices for the whole batch in a single pass
  (previously recorded prices first, the built-in catalog second).
  Imported mappings arrive disabled and unverified, so nothing routes until
  it has been probed. Model names may now be slash-namespaced
  (`vendor/model`), and `GET /v1/models/{model}` resolves those ids.

- Imported mappings are verified by a background probe queue, and a mapping
  that passes is enabled automatically if it was armed for that at import
  time (migrations 00027-00030). The queue survives restarts by re-deriving
  itself from the armed rows, and an explicit disable always wins over a
  probe still in flight — including one started by a different instance or
  an older binary.

- Models can choose how they pick among their providers
  (`scheduling_mode`, migration 00031). The default, `failover`, behaves
  exactly as before. `balanced` spreads caller API keys across the
  providers of one model and keeps each key on the provider it landed on,
  so a key's traffic stays warm on one upstream instead of hopping; a
  provider that turns out to be a dead end is quarantined briefly and the
  key rebinds to whatever actually served it.

- The cost-optimization page now estimates what Concise Output saves, as an
  amount per million output tokens: the traffic-weighted output price of
  the window's priced traffic times a measured coefficient. Savings are
  shown as two cards, one per switch — input compression's measured
  roll-up, and this projection alongside its coefficient, priced volume and
  pricing coverage. The coefficient is the median of a paired on/off
  benchmark across five models, published in full — method and all 150 raw
  measurement pairs — in `docs/concise-output-benchmark.md` and its Chinese
  edition, so the number can be checked rather than taken on faith.

- SQLite databases are snapshotted automatically before a startup migration,
  and the migration is refused outright if the snapshot cannot be written.
  Snapshots are named after the schema version they were taken at, refreshed
  on every attempt, and pruned to the newest five only after a migration
  fully succeeds. PostgreSQL deployments are not auto-backed up — the
  official image carries no dump tooling — so startup warns to run
  `db:backup` only when an upgrade is actually pending.

### Fixed

- Update traffic no longer gives up when a configured proxy is rate-limited.
  A configured proxy used to be the only route; direct GitHub now trails it
  as a fallback. This matters for the mirror the installer writes by
  default: it answers from shared egress addresses whose GitHub quota every
  deployment behind it spends, so it can be rate-limited into 403 while the
  deployment's own path to GitHub is perfectly healthy — and the console
  then reported "check failed" indefinitely with nothing an operator could
  see or fix. The fallback is cheap to the proxy in front of it, which
  keeps three quarters of the walk budget.

- The per-account usage report no longer invents an account. Requests
  rejected before they resolved to an API key were grouped into a synthetic
  row of their own; that row is not an account and is now dropped from the
  report and its CSV export alike. The overview cards above still count that
  traffic, and a note on the table says so.

- Copy buttons work on plain-HTTP deployments. Two of them called the
  clipboard API directly, which browsers leave undefined outside a secure
  context — the typical self-hosted LAN setup — so those copies silently
  failed where the fallback would have worked.

- CC-Switch profile exports name each key by its id rather than its display
  prefix, which two distinct keys can share.

- Turning on Concise Output no longer runs the two built-in prompt examples
  together in English (`...technical detail.Prefer standard library...`).

### Changed

- The Concise Output coefficient is 12.6%, re-measured. The first benchmark
  installed only one of the two sentences the switch actually writes, so it
  described a prompt no deployment uses. Re-running it against the real one
  reverses the headline finding: every model measured now shows a positive
  median, where a reasoning model previously measured negative. Both
  editions of the published benchmark are regenerated from the raw pairs and
  now state plainly which prompt and which language were measured.

## [0.1.7] - 2026-08-19

### Added

- Update traffic — the release lookup, both asset downloads, and the About
  page's version check — now tries GitHub directly and automatically retries
  through the built-in mirror, so deployments behind a slow or blocked GitHub
  upgrade without hand-editing `update.github_proxy`. An explicitly
  configured proxy stays the sole route, and its credentials are redacted
  wherever an error could echo them. A stall watchdog turns hopeless
  transfers into prompt fallbacks instead of burned-out timeouts: header or
  body silence kills a dead route, and a rate projection abandons a transfer
  that provably cannot deliver the announced size in the time the walk has
  left — routes it kills get one projection-free retry after every
  alternative fails.

### Changed

- A provider's key pool is now a capacity pool: requests rotate their
  starting key round-robin, so load spreads across the pool instead of
  piling onto the first key until it fails. A plain 429 benches the key it
  hit for the upstream's `Retry-After` window (honoured, clamped to 1s–10m;
  fallback `gateway.key_rate_limit_cooldown`, default 60s): later requests
  walk healthier keys first, and the bench is a demotion, never an
  exclusion — an all-cooling pool still dispatches, soonest-to-recover
  first, and a benched key that serves 2xx comes off the bench. Unauthorized
  and quota-exhausted keys keep the existing persistent retest path; the
  rotation and bench state is in-process and resets on restart.
- The time-dimension analytics report runs as one grouped query per report
  instead of one query per bucket — a 90-day day window went from up to 90
  database round-trips (720 for a 30-day hour window) to one.

### Fixed

- SSE streams framed with CRLF line endings (`\r\n\r\n`) were silently
  dropped whole by the chat and Gemini stream decoders, which waited for
  `\n\n` — a shape with no two consecutive newlines in it, so no frame ever
  completed. Framing now accepts `\n\n`, `\n\r\n`, and `\r\n\r\n` alike.
- SSE `data:` frames that omit the optional space after the colon (as
  Aliyun's Anthropic-compatible endpoint sends them) are accepted by the
  Claude, chat, and Gemini decoders. Previously every frame of such a
  stream was dropped and a fully delivered, fully billed completion
  settled as `stream_ended_unannounced` with zero usage recorded.
- A candidate on a switched-off provider no longer enters the routing
  chain. It used to be walked and skipped at walk time, spending a probe
  of the request budget and writing a "provider disabled" attempt row for
  every disabled provider sorted ahead of a live one — enough of them
  could spend the whole budget before the request reached the provider
  that would have served it. When no routable candidate remains the caller
  now gets the switched-off-configuration answer (503 "no enabled route")
  instead of the 502 that reports an upstream outage nobody had.

## [0.1.6] - 2026-08-16

### Added

- Multi-user accounts. The single-admin console grows into a team system
  with two roles: admins keep the full console, members get a self-service
  view (overview, usage, costs, and their own API keys) that is enforced
  server-side — every query a member makes is pinned to their own account,
  provider/upstream details never appear in member responses, and reaching
  for another account's key is indistinguishable from a key that does not
  exist.
- External login via any OAuth2/OIDC provider (authorization-code + PKCE).
  Providers are configured in the console (with one-click OIDC discovery);
  accounts are created automatically on first login. The original password
  account stays as the local escape hatch.
- User directory for admins: origin (local / login providers), role,
  status, key count, lifetime spend and last login per account, with
  enable/disable and promote/demote actions. Disabling an account deletes
  its sessions and turns off all of its API keys immediately and
  reversibly; deletion is deliberately not offered, so keys and request
  history always stay attributable.
- Account scoping across the console: API keys and request logs carry an
  owning account, and every dashboard/analytics/cost view can be filtered
  by account.

## [0.1.5] - 2026-08-14

### Added

- Official Docker deployment. Every release now also publishes a multi-arch
  (amd64/arm64) image to `ghcr.io/yolorouter/yolorouter` (`latest` plus the
  exact version tag), and the repository gained a standalone multi-stage
  `Dockerfile` — `docker build .` works from a plain checkout — plus a
  one-file `docker-compose.yml`: single service, one bind mount holding all
  state (generated config and SQLite database), health-checked, restart on
  failure. Inside a container the version check keeps working while
  in-place self-update is disabled in favor of pulling the newer image.

- The console can now update Yolorouter in place. The sidebar gained an
  About entry (with a new-version indicator when a release is out) whose
  page shows the version, checks for updates on demand, and offers a
  one-click update: the server downloads and verifies the release, swaps
  the binary, and restarts itself so the service manager brings the new
  version up. Runtimes where an in-place swap would be wrong — containers,
  Windows, binaries carrying Linux file capabilities, non-release builds —
  get a matching upgrade hint instead of the button, and `yolorouter
  update` inside a container now points at pulling the newer image.

- Models that cannot see images can now answer image requests. Declare a
  model unable to read images and pick a vision fallback model in the
  console: the gateway describes each incoming image through that model
  (billed to the calling key as its own logged request, linked to the
  parent) and forwards the description as text — the answer still comes
  from the model the caller asked for. With no fallback model configured,
  images become explanatory placeholders instead of failing the request
  upstream. Undeclared models are never touched. The request log shows
  describe sub-calls with a badge, links them to their parent request, and
  can filter by source.

## [0.1.4] - 2026-08-12

### Added

- The console's danger actions now say what they touch before they do it.
  Disabling a model lists the API keys whose allowlist names it, how many
  allow-all keys can also call it, and how many requests asked for it in the
  last week; renaming a model leads with that live-traffic number, since
  allowlists follow the model but callers ask by name; disabling a provider
  names the models with candidates on it, flags any that would be left with
  no routable source at all, and names the keys whose allowlist contains such
  a model. The model detail page's impact tab shows the
  same facts instead of a placeholder.
- A candidate that cannot be routed to now says what blocks it — the provider
  switched off, no usable key, a missing provider-side model name, a missing
  probe, or its own switch — in the order the operator has to fix them.
- The model list can be filtered by provider, answering "which models route
  through this one" at a glance.
- List rows now expand in place. A model row opens its full candidate chain
  in route order — provider, provider-side name, and whether the gateway can
  actually route there, with the blocking reason when it can't. A provider
  row opens the mirror view: every model that routes through it and the
  provider-side name each one maps to.
- Picking a provider for a candidate now shows what you're getting into:
  each option in the dropdown carries the provider's live status (or
  "disabled" when it's switched off) and how many of its keys are actually
  usable for routing, so a candidate that would go nowhere is visible before
  it is saved.
- The dashboard and the analytics reports drill through. Every KPI card,
  trend-chart day, upstream-status figure and top-caller row on the
  dashboard now opens the page that explains it — destinations in the
  request log carry the dashboard's selected window; the cost and token
  cards open their pages on those pages' own default windows. Every row of
  the four report tables opens the request log pre-filtered to that row's
  model, provider, key or time bucket on top of the report's own filters.
  Dashboard blocks are keyboard-operable; table rows follow the console's
  existing pointer-click convention, and the trend chart is a canvas
  (pointer only).
- The request log's merged "attempts" count is split into what it was
  hiding: key switches (rotating keys within one provider) and failovers
  (moving to another provider) are different events with different fixes.
  They surface as badges on the provider column — which now also names the
  provider-side model that actually served the request — and in full in a
  new per-row expansion alongside the complete request id and cache tokens.
  The table itself was reworked to never scroll sideways. The list can also
  be filtered by API key prefix and by ingress endpoint (the fixed paths
  exactly, the Gemini-compatible family by its /v1beta/ prefix, since those
  paths embed the model name), and the CSV export carries the split columns
  in place of the merged count.
- The request log gains a cost filter (priced / unknown cost) — the
  dashboard's unknown-cost figure drills into exactly the rows it counts.
- The provider report shows how many failovers each provider caused —
  charged to the provider that failed the request, not the one that rescued
  it, with key rotation within one provider not counted. A provider every
  request failed away from now appears in the report even though it served
  nothing, because that is precisely the provider worth noticing.
- The caller ranking now leads with spend instead of call volume — the row
  to look at first is whoever costs the most — and the calls column is
  sortable for the old ordering. The model and provider report tables gain
  the same sortable calls/cost headers (their default order is unchanged),
  and on mobile, where cards have no headers, a sort selector covers all
  three tabs. The time tab stays chronological.
- A provider key's row now shows the outcome category of its last test —
  rate limited, unreachable, quota unavailable, and so on — with the
  category's actionable hint. Previously the category flashed by in a toast
  and a page reload reduced it to pass/fail.
- A model with no usable route now tells the caller which kind of unusable:
  "no enabled route" (an operator switched routing off) versus "routes not
  verified yet" (a probe has not passed). Both were one shared "model is not
  available".
- An upstream key that answers 429 with an exhausted-quota body (OpenAI's
  insufficient_quota, or a quota/billing/credit message) is now marked for
  retest and taken out of routing, exactly as a 401 is — previously it kept
  being offered and every request burned an attempt on it. A plain
  rate-limit 429 still rotates without marking, since it heals on its own.
- The per-key TPM limit is now enforced. Settled usage is tallied into a
  per-key minute window and requests are rejected with 429 once the window
  reaches the configured limit — no prompt-token estimation, so a concurrent
  burst can overshoot the window once before it starts rejecting. Previously
  the console accepted and stored the limit but nothing ever applied it.

### Fixed

- A request asking for more output than the candidate serving it allows is now
  held down to that candidate's ceiling instead of being forwarded as written.
  Upstreams disagreed about what to do with an over-large ceiling — clamp,
  refuse, or accept and bill for it — so the same request could cost wildly
  different amounts depending on which provider took it.
- An OpenAI-format request stating its output ceiling as `max_completion_tokens`
  no longer loses it on the way to a provider that speaks another format. Only
  the older `max_tokens` spelling was read, so the ceiling of exactly the
  newest requests — the reasoning models require this field and reject the old
  one — vanished during protocol conversion, and the provider's own default
  applied instead. Where a request states both, the lower is used.

### Changed

- The relay kernel keeps growing extension points rather than feature code.
  A capability can now rewrite the caller's body before any candidate is
  chosen (which is how input compression works), repair a body an upstream
  refused and have it retried against the same candidate, and be asked to admit
  a request a second time once its price is known — the point at which a
  reservation can be computed at all. Providers carry a health record the
  decision table books failures against, and every request carries a retry
  budget that table prices.

## [0.1.3] - 2026-08-09

A feature and hardening release on top of 0.1.2: the console adapts to
mobile, API keys can be re-viewed after creation, provider prices refresh
themselves, and the relay kernel is restructured around explicit extension
points with fixes to failover, usage accounting and the CLI's deployment
handling.

### Added

- Responsive mobile layout across the console: pages, tables and dialogs adapt below the desktop breakpoint, with a shared bottom-sheet pattern for actions.
- API keys can be re-viewed and copied in full from the list page after creation, with the revealed key kept out of caches. Importing a key into Claude Code Switch prefills the model and prefers one that is actually available.
- The provider price catalog refreshes itself daily through the worker, and the price prefill hints in the provider forms follow the refreshed catalog.
- Request log: a stream body viewer renders captured SSE stream bodies frame by frame, and the body viewer gained copy / expand actions.

### Changed

- The relay kernel is restructured around explicit extension points — admission, request rewrites, response observation, terminal recording — with a single closed decision table for routing verdicts, held in place by structural tests that run with the suite. No routing behaviour change is intended.
- A service error crossing into another handler's domain maps to its own error code instead of falling through to a generic 500.
- "Today" on the dashboard and in analytics is computed from one clock reading taken when the request arrives, so the day boundary can no longer drift around local midnight within a single request.

### Fixed

- Failover now triggers when an upstream refuses a payload on content-inspection grounds, instead of surfacing the refusal to the caller while other providers might accept it. The refusal does not count against the provider's circuit breaker.
- Stop reasons, usage, and cache token counts are normalised across every protocol boundary, so a caller sees the same accounting whichever upstream protocol served the request.
- The Responses API accepts a content list as `function_call_output` output.
- Credentials carried by a failed dispatch are no longer persisted with the request log.
- Actions on revoked API keys are restricted to what a revoked key can meaningfully do.
- Provider probes tell timeouts apart from unreachable destinations, so the test result names the actual failure.
- The qwen price fetcher aligns columns by header rather than position, surviving column reordering in the source.
- `stop` reported `no running instance` and exited 0 while the server was running, whenever it was invoked from a directory other than the one the service runs in. It generated a config in the working directory and probed that empty deployment's lock instead of the real one. It no longer generates anything: it reports the path it looked at, and — when the binary belongs to an installation — the `--config` line to use instead. It also prints the config it resolved alongside every result, so an answer about a deployment is never shown without the deployment it is about.
- `db:rollback` generated a config when none was there, which on a deployment whose config had been lost pointed it straight back at the database still on disk. It now requires the config to exist, and prints the config and database it is about to act on.
- `db:rollback` ran its down migrations against a live server. It now takes the same instance lock `serve` holds for its lifetime, which `db:reset` has always required, instead of dropping tables and columns out from under a running process.
- A relative `sqlite_path` now resolves to one spelling however the config was reached. The lock file and, on Windows, the name of the shutdown event `stop` signals are both derived from that string, so two spellings of one deployment could leave `stop` signalling an event nobody listens on and timing out against a healthy server.

## [0.1.2] - 2026-07-31

A maintenance release on top of 0.1.1: native Windows install and development
scripts, the hosted service as a provider preset, and fixes to usage
accounting, the documentation links and the dashboard trend chart.

### Added

- Windows PowerShell scripts: `install.ps1`, runnable as an `irm ... | iex` one-liner, which registers the server as a scheduled task and installs machine-wide or per-user depending on elevation; and `dev.ps1` for local development. Both are documented in the READMEs and CONTRIBUTING.
- The hosted service as a provider preset. It runs this same gateway, so all four protocols are declared against a single base URL through a new optional `extraProtocols` preset field, giving Anthropic, Gemini and Responses callers native passthrough instead of a cross-protocol translation. No key ships with it.
- Preset cards link to their own provider console once picked, since by then a key is the only thing left to supply.
- Actions menu on the provider list with a direct link to that provider's cost detail page.

### Changed

- The custom system prompt is now a single "Concise Output" toggle. The free-form prompt editor is gone from both the global cost-optimization modal and the per-key one; the text comes from the built-in concise and minimal-code presets instead.
- The create-provider dialog opens with the hosted preset applied, so the common path is paste a key and save. Picking "custom" now clears the preset-owned fields (name, base URL, test model, protocol config) rather than only moving the highlight, which previously let a custom provider inherit the preselected preset's values.
- The READMEs are a user-facing overview again: what the project is, how to run it, and where the full documentation lives. The build, test and local development material moved into CONTRIBUTING.md.

### Fixed

- Usage was dropped whole for OpenAI-compatible upstreams that front an Anthropic model and copy the net input count into `prompt_tokens` while reporting the cached portion only under `prompt_tokens_details`. Read under the OpenAI convention, where the cached count is a subset of the prompt, such a record had its cache subtracted from a prompt that never contained it, and went negative when the cache exceeded the prompt — at which point it was rejected entirely, zeroing the completion and cache counts along with the input and logging a successful request as costing nothing. The convention is now settled once, before pricing or persistence read the usage, and only when the inclusive reading is positively ruled out.
- The released binaries can update themselves. The repository that `update` and the update-check API look for releases in is injected at build time, and the variable holding it was never set once the repository went public, so 0.1.0 and 0.1.1 both shipped with it empty — visible only to a user who runs `update` and is told it is disabled. A release now fails outright rather than publishing artifacts that quietly cannot update.
- Documentation links in the READMEs. The documentation is a separate app the main site embeds, so every hardcoded `/docs/...` link resolved through the SPA fallback and bounced back to the homepage; all 16 now go through the embedding route, which keeps the site chrome and language preference around the page.
- Dashboard trend chart: the two Y axes picked their own tick counts, leaving the cost labels between grid lines with no reference to read the line against. Both axes now share a tick count and a pinned zero. Line smoothing is also gone — daily buckets are discrete, and the spline overshot around a zero-to-peak jump, drawing cost on days that had none.
- Both PowerShell scripts were saved in GBK. PowerShell 7 assumes UTF-8 without a byte-order mark, so every localized string decoded to replacement characters; 5.1 falls back to the system code page, which only lines up on a Chinese-locale host. They are now UTF-8 with a BOM, the one form both read correctly.

## [0.1.1] - 2026-07-31

Yolorouter becomes multi-protocol: it now accepts four wire protocols and can
translate any of them to any other on the way to the provider. Adds
cost-optimization features, deeper cost reporting, and Windows support.

### Added

- Protocol-agnostic intermediate representation with a full codec per protocol (request decode/encode, response decode/encode, and a streaming decoder/encoder pair each).
- Anthropic Messages ingress: `POST /v1/messages`, authenticated with `X-Api-Key` or `Authorization: Bearer`.
- OpenAI Responses ingress: `POST /v1/responses`.
- Native Gemini ingress: `POST /v1beta/models/{model}:generateContent` and `:streamGenerateContent`, additionally accepting `x-goog-api-key` and `?key=` auth.
- Protocol negotiation per request: the body passes through with only the model name rewritten when the caller's protocol matches the provider's, and round-trips through the IR when it does not.
- Per-provider protocol set with per-protocol endpoint configuration and verification.
- Model discovery: `GET /v1/models` and `GET /v1/models/{model}`, returning the OpenAI or the Anthropic envelope depending on the client. Read-only — no relay, no spend.
- Custom system prompt injection, set globally and overridable per API key, applied in the caller's own protocol shape.
- Input compression for bulky tool output (`go test` / build logs, git diffs, grep results, generic logs), with a savings dashboard and per-request skip reasons. Global setting, overridable per API key.
- Cost statistics page plus per-model, per-provider, and per-key cost detail pages.
- Dashboard time-range selector with range-aware KPIs and trend, and separate input / output / cache token cards.
- API keys: all-models scope as an alternative to an explicit allowlist.
- Provider preset catalogue, live upstream model-list fetch, and per-candidate capability probing with specific failure reasons.
- Preset batch model creation, provider model-name picker, and inline editing on model detail pages.
- Unified list filters across admin pages, plus owner and status filters for API keys.
- Configurable gateway timeouts covering seven independent phases (connect, TLS handshake, headers, first byte, inter-chunk idle, per-attempt, whole-request), validated for ordering at startup.
- `stop` command, and Windows and macOS cross-compilation targets.
- Optional GitHub mirror for both install and self-update (`update.github_proxy`).
- Startup log prints the bound listen address, a clickable localhost URL, and the primary LAN IPv4 URL.
- One-click hand-off of a model or API key to the CC Switch desktop app via a `ccswitch://` deep link.

### Changed

- Replaced the single 120s upstream wall clock with the staged idle-keepalive timeouts described above, so reasoning models that pause for minutes before the first token are no longer cut off mid-request.
- Candidate mappings are verified against the real upstream when saved, replacing the manual test buttons.
- Timezone is taken from a browser-supplied IANA identifier via the `X-Timezone` header instead of being inferred server-side.
- Redesigned login and first-run setup shell with a shared language switcher; the dashboard empty state is now a setup-progress funnel banner.
- Restructured the admin sidebar into grouped navigation; empty states and the setup banner gained contextual icon tiles.
- All create and edit panels unified on a modal layout, dropping the previous drawer.
- Reworked the API key form's custom-prompt and expiry layout.
- `make build-windows` now cross-compiles runnable Windows binaries for amd64 and arm64 with the frontend embedded. The previous compile-only check, which produced no artifact, is now `make build-windows-check`.
- CI runs the test suite on a Windows host in addition to Linux; cross-compiling alone cannot catch platform behavior that only differs at runtime.

### Fixed

- Windows builds could not start at all: the config file's permission check rejected every file on Windows, where Go synthesizes `0666` for any writable file and `os.Chmod` only toggles the read-only attribute. Since first run generates the config and then reads it back, the server failed before serving anything. The check is now platform-split; Windows logs a warning that permissions cannot be enforced there and continues.
- Net (non-cached) input tokens no longer deduct cache-write tokens from the prompt total. No protocol counts cache writes inside the prompt, so the deduction understated both the input line of the bill and the stored input token count. Net input is now persisted consistently across the streaming and non-streaming paths.
- DeepSeek usage mapping: `prompt_cache_miss_tokens` is the non-cached prompt remainder, not a cache write, and was being priced at the cache-write rate while driving net input to zero. `prompt_cache_hit_tokens` was never read on the passthrough path, leaving cache reads billed at the full input price.
- Upstream token counts are checked for coherence before pricing — negatives, a cache read larger than an inclusive prompt, and parts exceeding a stated total now mark usage unknown instead of producing a fabricated charge. The micros conversion is range-guarded so an out-of-range value cannot corrupt budget accounting.
- Claude streaming `input_tokens` is adopted from the terminal `message_delta` when `message_start` reports zero, which is what a translating upstream does. Total tokens are recomputed from the per-field values on each delta rather than held at a high-water mark.
- Gemini thinking and tool-use tokens are accounted for correctly.
- The provider streaming probe now requires both a non-empty content delta and a clean termination, so an endpoint that emits one delta and then hangs is no longer certified as streaming-capable. Provider URLs and transport error strings are redacted in probe logs so credentials embedded in a base URL cannot leak.
- Installer error handling, uninstall scope detection, and upgrade safety.
- Frontend: TypeScript type errors, auth layout, chart empty state, and import toast behavior.

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

[Unreleased]: https://github.com/yolorouter/yolorouter/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/yolorouter/yolorouter/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/yolorouter/yolorouter/compare/v0.1.9...v0.2.0
[0.1.9]: https://github.com/yolorouter/yolorouter/compare/v0.1.8...v0.1.9
[0.1.8]: https://github.com/yolorouter/yolorouter/compare/v0.1.7...v0.1.8
[0.1.7]: https://github.com/yolorouter/yolorouter/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/yolorouter/yolorouter/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/yolorouter/yolorouter/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/yolorouter/yolorouter/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/yolorouter/yolorouter/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/yolorouter/yolorouter/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/yolorouter/yolorouter/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/yolorouter/yolorouter/releases/tag/v0.1.0
