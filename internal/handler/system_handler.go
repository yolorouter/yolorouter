// Package handler additions: the system info endpoint (GET
// /api/admin/system/version). It reports build/runtime metadata plus the
// latest-release check result. See design doc
// .claude/docs/2026-07-20-version-update-design.md §4.4.
package handler

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/internal/service"
	"github.com/yolorouter/yolorouter-ce/internal/version"
	"github.com/yolorouter/yolorouter-ce/pkg/response"
)

// VersionChecker is the subset of *service.VersionService this handler needs.
// Declared as an interface so the handler test can substitute a fake instead
// of standing up a real VersionService against a httptest server.
type VersionChecker interface {
	Check(ctx context.Context) service.VersionStatus
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
}

// GetSystemVersion handles GET /api/admin/system/version. It merges the
// static SystemInfo with a fresh update check and the current uptime into the
// unified response envelope. The update check never errors (a failed check is
// an expected condition — pre-public repo, GitHub outage, rate limit —
// surfaced via check_failed in the payload, not as a 500), so this handler
// has no error branch.
func GetSystemVersion(info SystemInfo, svc VersionChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		upd := svc.Check(c.Request.Context())
		response.Success(c, gin.H{
			"version":        info.Version,
			"commit":         info.Commit,
			"build_time":     info.BuildTime,
			"go_version":     info.GoVersion,
			"goos":           info.GOOS,
			"goarch":         info.GOARCH,
			"db_driver":      info.DBDriver,
			"uptime_seconds": int(time.Since(version.StartTime).Seconds()),
			"latest":         upd.Latest,
			"has_update":     upd.HasUpdate,
			"release_url":    upd.ReleaseURL,
			"check_failed":   upd.CheckFailed,
		})
	}
}
