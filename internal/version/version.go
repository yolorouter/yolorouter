// Package version holds build-time metadata (version string, git commit,
// build timestamp, default GitHub release source) injected via -ldflags, plus
// the process start time the system info handler uses to compute uptime.
//
// It is also the shared home of the routing policy for update-related GitHub
// traffic — the mirror fallback order (UpdateRoutes, DefaultMirror), URL
// prefixing (ProxyURL) and per-attempt budget discipline
// (RouteAttemptContext) — because both the self-updater and the version-check
// service walk the same routes and must agree on them.
//
// All vars default to development placeholders ("dev" / "unknown" / "") so a
// plain `go build` without -ldflags still works; release builds override them
// via `make build-release` (Makefile) or goreleaser (.goreleaser.yaml).
package version

import (
	"context"
	"strings"
	"time"
)

var (
	// Version is the semver tag of this build ("v0.1.0"), or "dev" for a
	// plain `go build`. Release builds inject it via ldflags; it MUST carry
	// the leading "v" to satisfy golang.org/x/mod/semver and to match the
	// GitHub release tag_name / asset naming convention.
	Version = "dev"

	// Commit is the short git sha at build time ("abc1234"), or "unknown".
	Commit = "unknown"

	// BuildTime is the UTC build timestamp (RFC3339), or "unknown".
	BuildTime = "unknown"

	// DefaultGitHubRepo is the compiled-in "owner/repo" release source
	// (e.g. "yolorouter/yolorouter"), injected at release time via
	// ldflags. Empty in dev builds. config.update.github_repo overrides it
	// per-deployment; both empty (or update.enabled=false) disable the
	// update feature entirely.
	DefaultGitHubRepo = ""

	// StartTime records the process start instant (captured at package
	// init). The system info handler reports uptime as time.Since(StartTime).
	StartTime = time.Now()
)

// ResolveRepo returns the effective "owner/repo" release source, or "" when
// the update feature is disabled. Precedence: an explicit config
// update.github_repo wins; otherwise the compiled-in DefaultGitHubRepo; the
// feature is disabled entirely when enabled is false or both repo sources are
// empty.
//
// Taking enabled + githubRepo as plain args (rather than importing config)
// keeps this package free of any config dependency — both the running server
// (which holds a *config.Config) and the standalone `update` CLI (which loads
// config itself) call this with their own resolved values.
func ResolveRepo(enabled bool, githubRepo string) string {
	if !enabled {
		return ""
	}
	if githubRepo != "" {
		return githubRepo
	}
	return DefaultGitHubRepo
}

// DefaultMirror is the project's public GitHub mirror (a Cloudflare proxy
// that prefixes the original URL). It is only ever used as an automatic
// FALLBACK: direct GitHub is always tried first, so deployments with a
// healthy path to GitHub never send update traffic through it, and an
// explicitly configured proxy replaces it with the operator's proxy.
const DefaultMirror = "https://gh.yolorouter.com"

// UpdateRoutes is the ordered list of proxy prefixes update-related GitHub
// traffic (release lookups, version checks, asset downloads) tries. "" means
// direct. An explicit proxy goes first, with direct GitHub behind it as a
// fallback: a shared mirror answers from a pool of egress addresses whose
// GitHub quota every deployment behind it spends, so it can be rate-limited
// into 403 while the deployment's own path to GitHub is perfectly healthy.
// The fallback only ever runs once the proxy attempt has failed, and it is
// cheap to the proxy: it is handed a reserveShare slice of the walk rather
// than an even split, so a working proxy keeps nearly all of its budget. A
// deployment on a network without direct access loses nothing either — the
// extra attempt fails on connect and the walk ends in the failure it would
// have had anyway.
func UpdateRoutes(explicitProxy string) []string {
	if explicitProxy != "" {
		return []string{explicitProxy, ""}
	}
	return []string{"", DefaultMirror}
}

// fallbackReserve caps how much of a route walk's remaining budget a
// non-final attempt leaves untouched for the routes behind it. One minute is
// enough for the mirror to move an asset at ordinary speed while costing a
// healthy earlier route only its final sliver of budget.
const fallbackReserve = time.Minute

// reserveShare is the fraction of the remaining budget held back when
// fallbackReserve is more than there is to give — which is every short walk,
// the release lookup among them.
//
// A quarter, not a half. On the lookup's ten-second budget a half left an
// explicit proxy five seconds where it used to have all ten, so a mirror
// that answered in seven stopped working the day the direct fallback was
// added — the fallback is supposed to cost the route in front of it nothing
// it was using. A quarter leaves that proxy seven and a half seconds and
// still hands the route behind it two and a half, which is ample for a
// lookup that answers in well under a second. Longer walks never see this:
// asset downloads are budgeted in minutes, so fallbackReserve caps them
// first and their share is unchanged.
const reserveShare = 4

// RouteAttemptContext bounds one route attempt of a walk. The final attempt
// runs on the walk context as-is — everything that remains is its to spend.
// Any earlier attempt is capped below the walk deadline so the fallback
// routes behind it always inherit a usable share: without this, a first
// route that hangs until the walk deadline starves the very fallback the
// walk exists for — and a fallback that only survives its predecessor's
// FAST failures is no fallback against the slow ones. The cap is the
// remainder minus a reserve — fallbackReserve, or a reserveShare slice of
// the remainder when less than that is left. A context without a deadline is
// returned as-is.
func RouteAttemptContext(ctx context.Context, finalAttempt bool) (context.Context, context.CancelFunc) {
	if finalAttempt {
		return ctx, func() {}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, func() {}
	}
	reserve := min(time.Until(deadline)/reserveShare, fallbackReserve)
	if reserve <= 0 {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline.Add(-reserve))
}

// ProxyURL prefixes rawURL with a mirror when proxy is non-empty, so a
// deployment behind a slow or blocked GitHub can route release lookups and
// asset downloads through a proxy. Trailing slashes on the proxy are trimmed
// and exactly one separator is inserted, so both "https://host/" and
// "https://host" produce a well-formed "https://host/https://github.com/...".
// An empty proxy returns rawURL unchanged.
func ProxyURL(proxy, rawURL string) string {
	if proxy == "" {
		return rawURL
	}
	return strings.TrimRight(proxy, "/") + "/" + rawURL
}
