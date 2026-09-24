package database

import (
	"database/sql"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/pkg/logger"
)

// NewUpdatePreflight returns the gate the admin console's "update now"
// entry (POST /api/admin/system/update) runs before any download starts.
// For sqlite deployments it is "rotate first, then size": old
// pre-migration backups are rotated down to the keep cap — through the
// same cleanup var the startup path calls, pinning the snapshot file the
// post-update restart's backup will (over)write — and only then does
// PrecheckMigrationDiskSpace compare the estimate against the filesystem,
// so a deployment that fits after rotation is a real fit, not a refusal
// the program could have solved by tidying up after itself. A refusal is
// a *PrecheckRejectedError carrying the numbers; nil means proceed.
//
// Every other driver gets a preflight that allows unconditionally:
// postgres deployments are exempt from the button-layer rotation and
// precheck, the same exemption MigrateWithBackup grants the startup
// path. The exemption lives inside the returned closure rather than
// being a nil the wiring must remember to check, so a recording probe
// and a recording cleanup can mechanically prove the non-sqlite path
// consults neither.
//
// db supplies the schema version for the rotation pin and is queried on
// every run, not at construction: the router is assembled before this
// process's own startup migrations have run, so a version captured at
// wiring time would be one upgrade behind the button click. A nil db, or
// a version that cannot be read, degrades to an empty pin with a warning
// — the preflight is an informative early refusal and must not become a
// new failure source of its own; the fail-closed backup remains the
// actual safety net. probe may be nil, in which case
// PrecheckMigrationDiskSpace falls back to the OS probe.
func NewUpdatePreflight(driver string, db *sql.DB, sqlitePath string, probe FreeSpaceProbe) func() error {
	return func() error {
		if driver != "sqlite" {
			return nil
		}
		pin := ""
		if db != nil {
			if current, err := GetCurrentVersion(db, driver); err == nil && current > 0 {
				pin = preMigrationBackupFilename(current)
			} else if err != nil {
				logger.Warn("update preflight could not read the schema version for the rotation pin; rotating without a pin", zap.Error(err))
			}
		}
		cleanupPreMigrationBackups(preMigrationBackupDir(sqlitePath), pin)
		return PrecheckMigrationDiskSpace(sqlitePath, probe)
	}
}
