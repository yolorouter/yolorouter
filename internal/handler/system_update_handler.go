// Package handler additions: the system update endpoint (POST
// /api/admin/system/update) applies the in-place binary upgrade and
// schedules a graceful process restart so the service manager brings the
// new binary up.
package handler

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/selfupdate"
	"github.com/yolorouter/yolorouter/pkg/database"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/logger"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// UpdateApplier runs one in-place update attempt. In production it wraps
// selfupdate.Apply with the resolved repo/proxy/current version; tests
// substitute a fake so no network or filesystem is touched.
type UpdateApplier func(ctx context.Context) (selfupdate.Result, error)

// UpdatePrecheck gates one update attempt before any download starts.
// Production wires database.NewUpdatePreflight: on sqlite it rotates old
// pre-migration backups down to the keep cap and then refuses with exact
// numbers (*database.PrecheckRejectedError) when the backup filesystem
// cannot fit one more backup — telling the operator BEFORE the upgrade
// begins, while the process is still up, instead of after the restart
// when the startup migration path hits the same wall. nil skips the gate
// entirely (assembly variants without a preflight).
type UpdatePrecheck func() error

// PostSystemUpdate handles POST /api/admin/system/update.
//
// updateMode is resolved once at assembly (selfupdate.Mode) — it depends only
// on process-lifetime facts (container marker, GOOS, config, build version).
// Only ModeInPlace may proceed; every other mode gets a 400 with
// SystemUpdateUnsupported, and the console decides what to show from the
// version endpoint's update_mode field before ever calling here.
//
// restart is invoked only after a successful binary replacement, after the
// success response has been written. In production it is
// selfupdate.ScheduleRestart (deferred SIGTERM to self → graceful drain →
// the service manager restarts onto the new binary).
//
// precheck (see UpdatePrecheck) runs after the process-wide gate is taken
// and before apply, so the rotation and the disk probe it performs are
// serialized against any in-flight update instead of racing one.
func PostSystemUpdate(updateMode string, precheck UpdatePrecheck, apply UpdateApplier, restart func()) gin.HandlerFunc {
	// Process-wide update gate. The selfupdate package's file lock only
	// covers one Apply run: after a successful apply it is released, while
	// this process keeps reporting the OLD version until the delayed
	// graceful restart completes. A second POST in that window would apply
	// the same release again — and its replaceBinary would overwrite
	// <exe>.bak (the only rollback copy) with the already-new binary. The
	// gate closes when an apply starts, reopens only after a FAILED or
	// up-to-date run, and stays closed forever once an update succeeded —
	// the restart is the only valid next step.
	const (
		updateIdle int32 = iota
		updateRunning
		updateApplied
	)
	var gate atomic.Int32
	return func(c *gin.Context) {
		if updateMode != selfupdate.ModeInPlace {
			response.Error(c, errcode.SystemUpdateUnsupported, updateUnsupportedMessage(updateMode))
			return
		}
		if !gate.CompareAndSwap(updateIdle, updateRunning) {
			response.ErrorStatus(c, http.StatusConflict, errcode.SystemUpdateInProgress, errcode.GetMessage(errcode.SystemUpdateInProgress))
			return
		}

		// The pre-upgrade refusal, before anything is downloaded or
		// replaced. It carries its own error code (not SystemUpdateFailed:
		// nothing was attempted) and its own data payload — the exact
		// required/free numbers, following WriteAdminErrorWithData's
		// precedent (AccountLoginLocked's locked_until) for the one error
		// family whose structured details a client must be able to show.
		// The full text with the human-readable numbers goes to the server
		// log; the envelope's message is the registry text, which the
		// console replaces with its localized version.
		if precheck != nil {
			if err := precheck(); err != nil {
				var rejected *database.PrecheckRejectedError
				if errors.As(err, &rejected) {
					// Nothing ran, so the gate reopens: the operator frees
					// space and clicks again against the same process.
					gate.Store(updateIdle)
					logger.Warn("system update refused by disk-space precheck", zap.Error(err))
					middleware.WriteAdminErrorWithData(c, http.StatusBadRequest, errcode.SystemUpdateInsufficientDisk, gin.H{
						"required_bytes": rejected.RequiredBytes,
						"free_bytes":     rejected.FreeBytes,
						"backup_dir":     rejected.BackupDir,
					})
					return
				}
				// The preflight's only refusal is the rejection above;
				// anything else is a malfunction of the check itself, and
				// an informative precheck must not become a new failure
				// source — the same call the startup path makes for a probe
				// that cannot answer. Log and proceed; the startup
				// precheck still guards the backup itself.
				logger.Warn("disk-space precheck before update failed, continuing", zap.Error(err))
			}
		}

		// The confirmed click is the commitment point: once the update is
		// running it must not die with the HTTP connection (a closed tab, a
		// proxy cutting the long request) — that would abort a download the
		// operator believes is in progress, and the console's polling would
		// then misreport "the service never came back". Detach from the
		// request's cancellation; the download client's own timeout still
		// bounds the run.
		res, err := apply(context.WithoutCancel(c.Request.Context()))
		if err != nil {
			// Nothing was replaced — reopen the gate so the operator can
			// retry after fixing the cause.
			gate.Store(updateIdle)
			// The full cause goes to the server log — the envelope's message
			// is replaced client-side by the code's localized text, so the
			// log is where an operator finds out what actually failed.
			logger.Error("system update failed", zap.Error(err))
			response.ErrorStatus(c, http.StatusInternalServerError, errcode.SystemUpdateFailed, err.Error())
			return
		}
		if res.UpToDate {
			gate.Store(updateIdle)
			// The badge said an update existed but the release moved on (or a
			// concurrent update already applied it). Nothing was changed; no
			// restart.
			response.Success(c, gin.H{
				"status": "up_to_date",
				"target": res.Target,
			})
			return
		}

		gate.Store(updateApplied)
		response.Success(c, gin.H{
			"status": "updated",
			"target": res.Target,
		})
		restart()
	}
}

// updateUnsupportedMessage appends the mode-specific upgrade path to the
// generic refusal, so a direct API caller (curl, a stale UI) learns what to
// do — the localized console renders its own hints from update_mode, but
// this response is all a bare client gets.
func updateUnsupportedMessage(mode string) string {
	base := errcode.GetMessage(errcode.SystemUpdateUnsupported)
	switch mode {
	case selfupdate.ModeContainer:
		return base + ": running in a container — pull the newer image and recreate the container (docker compose pull && docker compose up -d)"
	case selfupdate.ModeWindows:
		return base + ": windows cannot replace a running executable — download the latest release manually"
	case selfupdate.ModeCapabilities:
		return base + ": the binary carries Linux file capabilities — update via `yolorouter update` and reapply them before restarting"
	case selfupdate.ModeDisabled:
		return base + ": updates are disabled by configuration"
	case selfupdate.ModeDevBuild:
		return base + ": this is not a release build"
	default:
		return base
	}
}
