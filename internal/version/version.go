// Package version holds build-time metadata (version string, git commit,
// build timestamp, default GitHub release source) injected via -ldflags, plus
// the process start time the system info handler uses to compute uptime.
//
// All vars default to development placeholders ("dev" / "unknown" / "") so a
// plain `go build` without -ldflags still works; release builds override them
// via `make build-release` (Makefile) or goreleaser (.goreleaser.yaml).
package version

import "time"

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
	// (e.g. "yolorouter/yolorouter-ce"), injected at release time via
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
