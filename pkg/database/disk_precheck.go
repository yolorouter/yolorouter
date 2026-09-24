package database

import (
	"errors"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/pkg/logger"
)

// ErrFreeSpaceProbeUnsupported is what a FreeSpaceProbe returns on platforms
// where filesystem free space is not probed (windows). The precheck treats it
// as "cannot answer" and lets the migration proceed: windows deployments keep
// the pre-existing fail-closed backup semantics instead of gaining a second,
// untested platform implementation.
var ErrFreeSpaceProbeUnsupported = errors.New("filesystem free space probing is not supported on this platform")

// FreeSpaceProbe reports the bytes available to unprivileged writers on the
// filesystem containing dir. It is a dependency rather than a direct syscall
// call so the precheck logic can be exercised against fake values (a "just
// enough" vs "one byte short" boundary, an unavailable probe) without
// manufacturing a full disk in tests. OSFreeSpaceProbe is the production
// implementation.
type FreeSpaceProbe func(dir string) (freeBytes int64, err error)

// estimatePeakBackupNeed returns the disk space one pre-migration backup
// transiently requires, as a multiple of the database file size. At the peak
// moment the uncompressed VACUUM INTO snapshot and the gzip file being built
// from it coexist on the same filesystem, so a backup needs well over the
// database size at once; 2.5x (both copies plus slack) is the factor measured
// on a real upgrade that ran out of space halfway through the backup.
// Integer math, rounded up, keeps the estimate exact and deterministic.
func estimatePeakBackupNeed(dbSize int64) int64 {
	return (dbSize*5 + 1) / 2
}

// PrecheckRejectedError is returned by PrecheckMigrationDiskSpace when the
// filesystem that would hold the pre-migration backup cannot fit one more
// backup. It carries the exact numbers so callers can show "need about X,
// only Y free" instead of a generic failure — today the startup failure
// output, and the update API response once the update entry point is
// wired to the precheck.
type PrecheckRejectedError struct {
	RequiredBytes int64
	FreeBytes     int64
	BackupDir     string
}

func (e *PrecheckRejectedError) Error() string {
	return fmt.Sprintf(
		"insufficient disk space for the pre-migration backup: need about %s, only %s free on %s; refusing to migrate — the database is untouched, free up disk space and restart",
		formatByteCount(e.RequiredBytes), formatByteCount(e.FreeBytes), e.BackupDir)
}

// PrecheckMigrationDiskSpace verifies that the filesystem holding the
// pre-migration backups has room for one more backup, estimated from the
// SQLite database file size. The startup migration path calls it today,
// through MigrateWithBackup; the in-app update entry point will call this
// same function too once its precheck wiring lands, so the estimate and
// the comparison against free space exist exactly once.
//
// The result is either nil or a *PrecheckRejectedError — never any other
// error. A probe that cannot answer (unsupported platform, unreadable
// filesystem) and a missing database file both allow the migration to
// proceed: the precheck is an early, informative refusal, while the backup
// itself stays fail-closed and remains the actual safety net, so a hedging
// check must not invent new startup failures of its own. An exact tie
// (free == required) passes; one byte less is rejected.
func PrecheckMigrationDiskSpace(sqlitePath string, probe FreeSpaceProbe) error {
	if probe == nil {
		probe = OSFreeSpaceProbe
	}
	fi, err := os.Stat(sqlitePath)
	if err != nil {
		// Nothing to size: either the database is about to be created fresh
		// (no pre-upgrade state to protect) or the backup will fail-closed
		// on the missing file. Either way there is no number to compare.
		return nil
	}
	required := estimatePeakBackupNeed(fi.Size())
	dir := preMigrationBackupDir(sqlitePath)
	free, err := probe(dir)
	if err != nil {
		if !errors.Is(err, ErrFreeSpaceProbeUnsupported) {
			logger.Warn("disk space precheck unavailable, continuing", zap.String("dir", dir), zap.Error(err))
		}
		return nil
	}
	if free >= required {
		return nil
	}
	return &PrecheckRejectedError{
		RequiredBytes: required,
		FreeBytes:     free,
		BackupDir:     dir,
	}
}

// formatByteCount renders a byte count with binary units for error messages
// and logs: "512 B", "1.5 KiB", "2.5 MiB", "20.0 GiB". One fractional digit
// is enough for the "is this ballpark plausible?" judgement these numbers
// exist for.
func formatByteCount(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
