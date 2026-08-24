// Package handler exposes the system endpoints: build/runtime metadata plus
// the latest-release check (GET /api/admin/system/version, admin-only), and
// the gateway address clients should point at (GET
// /api/admin/system/endpoint, readable by any signed-in account).
package handler

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"

	versionsvc "github.com/yolorouter/yolorouter/internal/service/version"
	"github.com/yolorouter/yolorouter/internal/version"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// VersionChecker is the subset of *versionsvc.VersionService this handler needs.
// Declared as an interface so the handler test can substitute a fake instead
// of standing up a real VersionService against a httptest server.
type VersionChecker interface {
	Check(ctx context.Context) versionsvc.VersionStatus
	// CheckFresh bypasses the service's result cache — used when the
	// operator explicitly asks for a check (?force=1) rather than a page
	// passively showing the last known state.
	CheckFresh(ctx context.Context) versionsvc.VersionStatus
}

// SystemInfo is the static, build/runtime-known metadata the system info
// endpoint reports alongside the update status. router.New assembles it once
// (ldflags-injected version vars + runtime.* + the DB driver name) and hands
// it to GetSystemVersion, which only adds the per-request uptime and the
// fresh update-check result.
type SystemInfo struct {
	Version   string
	Commit    string
	BuildTime string
	GoVersion string
	GOOS      string
	GOARCH    string
	DBDriver  string
	// UpdateMode says how this process can be upgraded (one of the
	// selfupdate.Mode* values). The console uses it to render either the
	// one-click update button (in_place) or the matching guidance (pull a
	// newer image, download manually, ...). Resolved once at assembly — it
	// depends only on process-lifetime facts.
	UpdateMode string
}

// GetSystemVersion handles GET /api/admin/system/version. It merges the
// static SystemInfo with a fresh update check and the current uptime into the
// unified response envelope. The update check never errors (a failed check is
// an expected condition — pre-public repo, GitHub outage, rate limit —
// surfaced via check_failed in the payload, not as a 500), so this handler
// has no error branch.
func GetSystemVersion(info SystemInfo, svc VersionChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		// ?force=1 marks an operator-initiated "check now": skip the result
		// cache so a release published minutes ago is actually seen.
		var upd versionsvc.VersionStatus
		if c.Query("force") == "1" {
			upd = svc.CheckFresh(c.Request.Context())
		} else {
			upd = svc.Check(c.Request.Context())
		}
		response.Success(c, gin.H{
			"version":        info.Version,
			"commit":         info.Commit,
			"build_time":     info.BuildTime,
			"go_version":     info.GoVersion,
			"goos":           info.GOOS,
			"goarch":         info.GOARCH,
			"db_driver":      info.DBDriver,
			"update_mode":    info.UpdateMode,
			"uptime_seconds": int(time.Since(version.StartTime).Seconds()),
			"latest":         upd.Latest,
			"has_update":     upd.HasUpdate,
			"release_url":    upd.ReleaseURL,
			"check_failed":   upd.CheckFailed,
		})
	}
}

// GetSystemEndpoint handles GET /api/admin/system/endpoint: the base URL API
// clients should point at, before any protocol path such as /v1.
//
// It sits apart from the build info above because it is readable by any
// signed-in account, not just admins. Members provision and manage their own
// keys, which makes them precisely the people who need to know where to send
// traffic; and the address is this deployment's own public origin, which
// every one of them already typed into a browser to get here.
//
// externalURL is the configured server.external_url (may be empty) —
// publicBaseURL prefers it and derives from the request otherwise.
func GetSystemEndpoint(externalURL string) gin.HandlerFunc {
	return func(c *gin.Context) {
		response.Success(c, gin.H{"endpoint": publicBaseURL(c, externalURL)})
	}
}
