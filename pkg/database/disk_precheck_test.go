package database

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// recordingSpaceProbe is a FreeSpaceProbe fake that records every directory
// it is asked about and replays a canned answer, so the precheck can be
// driven from exact byte values (one byte short, exactly enough, unavailable)
// instead of manufacturing a full disk.
type recordingSpaceProbe struct {
	calls []string
	free  int64
	err   error
}

func (p *recordingSpaceProbe) probe(dir string) (int64, error) {
	p.calls = append(p.calls, dir)
	return p.free, p.err
}

// peakNeedFor restates the estimate the precheck is specified to use — the
// database file size × 2.5, rounded up — independently of the production
// helper, so tests derive their boundary values from the rule rather than
// from the code under test.
func peakNeedFor(t *testing.T, sqlitePath string) int64 {
	t.Helper()
	fi, err := os.Stat(sqlitePath)
	if err != nil {
		t.Fatalf("stat %s: %v", sqlitePath, err)
	}
	return (fi.Size()*5 + 1) / 2
}

func TestEstimatePeakBackupNeed(t *testing.T) {
	cases := []struct {
		size int64
		want int64
	}{
		{0, 0},
		{3, 8},              // 2.5×3 = 7.5 — rounds up, never underestimates
		{1 << 20, 2621440},  // 1 MiB → exactly 2.5 MiB
		{4 << 20, 10 << 20}, // 4 MiB → 10 MiB
	}
	for _, tc := range cases {
		if got := estimatePeakBackupNeed(tc.size); got != tc.want {
			t.Errorf("estimatePeakBackupNeed(%d) = %d, want %d (size × 2.5)", tc.size, got, tc.want)
		}
	}
}

func TestFormatByteCount(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{2621440, "2.5 MiB"},
		{3 << 30, "3.0 GiB"},
	}
	for _, tc := range cases {
		if got := formatByteCount(tc.n); got != tc.want {
			t.Errorf("formatByteCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestPrecheckMigrationDiskSpaceBoundary drives the decision with exact byte
// values around the requirement: strictly more passes, exactly equal passes,
// one byte less is rejected. The rejection carries both numbers so callers
// can surface them.
func TestPrecheckMigrationDiskSpaceBoundary(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "yolorouter.db")
	mustWriteFile(t, sqlitePath, make([]byte, 1<<20))
	const need = 2621440 // 1 MiB × 2.5
	wantDir := preMigrationBackupDir(sqlitePath)

	for _, tc := range []struct {
		name    string
		free    int64
		rejects bool
	}{
		{"free above need passes", need + 1, false},
		{"free exactly equal passes", need, false},
		{"free one byte short rejects", need - 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &recordingSpaceProbe{free: tc.free}
			err := PrecheckMigrationDiskSpace(sqlitePath, p.probe)
			if tc.rejects {
				var rejected *PrecheckRejectedError
				if !errors.As(err, &rejected) {
					t.Fatalf("expected a *PrecheckRejectedError, got %v", err)
				}
				if rejected.RequiredBytes != need {
					t.Fatalf("RequiredBytes = %d, want %d", rejected.RequiredBytes, need)
				}
				if rejected.FreeBytes != tc.free {
					t.Fatalf("FreeBytes = %d, want %d", rejected.FreeBytes, tc.free)
				}
			} else if err != nil {
				t.Fatalf("expected the precheck to pass, got %v", err)
			}
			if len(p.calls) != 1 || p.calls[0] != wantDir {
				t.Fatalf("probe consulted %v, want exactly [%s] (the pre-migration backup directory)", p.calls, wantDir)
			}
		})
	}
}

// TestPrecheckRejectedErrorNamesBothSizes pins the operator-facing message:
// both sizes in human-readable units plus what to do about it. The wanted
// strings are literals, not built by formatByteCount, so a formatting or
// wording regression cannot hide behind shared code.
func TestPrecheckRejectedErrorNamesBothSizes(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "yolorouter.db")
	mustWriteFile(t, sqlitePath, make([]byte, 8<<20)) // → 20 MiB required
	p := &recordingSpaceProbe{free: 3 << 19}          // 1.5 MiB free

	err := PrecheckMigrationDiskSpace(sqlitePath, p.probe)
	if err == nil {
		t.Fatal("expected a rejection with 1.5 MiB free against a 20 MiB requirement")
	}
	for _, want := range []string{"20.0 MiB", "1.5 MiB", "untouched", "restart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection message missing %q: %s", want, err.Error())
		}
	}
}

// An unavailable probe (its windows form is ErrFreeSpaceProbeUnsupported,
// but any error means "cannot answer") must never block the migration: the
// precheck is an early, informative refusal, not a new failure mode — the
// fail-closed backup remains the safety net.
func TestPrecheckMigrationDiskSpaceAllowsWhenProbeCannotAnswer(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "yolorouter.db")
	mustWriteFile(t, sqlitePath, make([]byte, 1<<20))

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unsupported platform", ErrFreeSpaceProbeUnsupported},
		{"probe error", errors.New("statfs: permission denied")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &recordingSpaceProbe{err: tc.err}
			if err := PrecheckMigrationDiskSpace(sqlitePath, p.probe); err != nil {
				t.Fatalf("expected the precheck to allow when it cannot answer, got %v", err)
			}
		})
	}
}

// A missing database file leaves nothing to size — the precheck passes (and
// does not even consult the probe); a fresh database has no pre-upgrade
// state to protect and a genuinely absent file fails in the backup instead.
func TestPrecheckMigrationDiskSpaceSkipsMissingDatabase(t *testing.T) {
	p := &recordingSpaceProbe{free: 0} // would reject instantly if consulted
	err := PrecheckMigrationDiskSpace(filepath.Join(t.TempDir(), "absent.db"), p.probe)
	if err != nil {
		t.Fatalf("expected nil for a missing database file, got %v", err)
	}
	if len(p.calls) != 0 {
		t.Fatalf("probe was consulted %d time(s) for a missing database file", len(p.calls))
	}
}

// A nil probe selects the production OS probe rather than panicking. A
// zero-byte database needs zero bytes, so this passes regardless of how
// much space the test machine actually has left.
func TestPrecheckMigrationDiskSpaceDefaultsNilProbeToOSProbe(t *testing.T) {
	sqlitePath := filepath.Join(t.TempDir(), "yolorouter.db")
	mustWriteFile(t, sqlitePath, nil)
	if err := PrecheckMigrationDiskSpace(sqlitePath, nil); err != nil {
		t.Fatalf("expected nil probe to fall back to the OS probe and pass, got %v", err)
	}
}

// The production probe must answer for the backup directory even before it
// exists (the first upgrade creates it): it climbs to the closest existing
// ancestor, which by construction lives on the same filesystem.
func TestOSFreeSpaceProbeClimbsToExistingAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the production probe reports unsupported on windows")
	}
	deep := filepath.Join(t.TempDir(), "backups", "pre-migration") // does not exist
	free, err := OSFreeSpaceProbe(deep)
	if err != nil {
		t.Fatalf("OSFreeSpaceProbe(%s): %v", deep, err)
	}
	if free <= 0 {
		t.Fatalf("expected a positive free-space reading for %s, got %d", deep, free)
	}
}

// The startup layer must refuse BEFORE anything is written: no snapshot, no
// migration, and the error is the precheck rejection itself (not wrapped
// into the migration-failure sentinel) with its numbers and guidance.
func TestMigrateWithBackupRejectsWhenBackupSpaceInsufficient(t *testing.T) {
	db, sqlitePath := stageDB(t, 1)
	dataDir := filepath.Dir(sqlitePath)
	need := peakNeedFor(t, sqlitePath)
	p := &recordingSpaceProbe{free: need - 1}

	_, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", p.probe)
	var rejected *PrecheckRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected a *PrecheckRejectedError from the startup layer, got %v", err)
	}
	var migrationFailure *MigrationFailedError
	if errors.As(err, &migrationFailure) {
		t.Fatal("a precheck rejection must not be typed as a migration failure — no snapshot was at stake")
	}
	if !strings.Contains(err.Error(), "untouched") || !strings.Contains(err.Error(), "restart") {
		t.Errorf("rejection lacks the data-untouched / free-up-and-restart guidance: %s", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "backups")); !os.IsNotExist(statErr) {
		t.Fatalf("expected no backups directory (nothing may be written), stat err = %v", statErr)
	}
	if got := mustVersion(t, db); got != 1 {
		t.Fatalf("db version = %d, want 1 (the migration must not run)", got)
	}
	if len(p.calls) != 1 || p.calls[0] != preMigrationBackupDir(sqlitePath) {
		t.Fatalf("probe consulted %v, want exactly [%s]", p.calls, preMigrationBackupDir(sqlitePath))
	}
}

// Control group for the rejection above: with just-over-enough space the
// startup path runs its normal backup-then-migrate sequence.
func TestMigrateWithBackupProceedsWhenBackupSpaceSufficient(t *testing.T) {
	db, sqlitePath := stageDB(t, 1)
	dataDir := filepath.Dir(sqlitePath)
	need := peakNeedFor(t, sqlitePath)
	p := &recordingSpaceProbe{free: need + 1<<20}

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", p.probe)
	if err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	wantPath := filepath.Join(dataDir, "backups", "pre-migration", "sqlite_v1.db.gz")
	if backupPath != wantPath {
		t.Fatalf("backup path = %q, want %q", backupPath, wantPath)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("snapshot missing on the sufficient-space path: %v", err)
	}
	if got := mustVersion(t, db); got != 2 {
		t.Fatalf("db version = %d, want 2 (backup and migration should both run)", got)
	}
}

// The windows exemption end-to-end at the startup layer: an unsupported
// probe means "cannot precheck", not "refuse" — the ordinary fail-closed
// backup path runs un-prechecked.
func TestMigrateWithBackupProceedsWhenSpaceProbeUnsupported(t *testing.T) {
	db, sqlitePath := stageDB(t, 1)
	dataDir := filepath.Dir(sqlitePath)
	p := &recordingSpaceProbe{err: ErrFreeSpaceProbeUnsupported}

	backupPath, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", p.probe)
	if err != nil {
		t.Fatalf("MigrateWithBackup: %v", err)
	}
	if want := filepath.Join(dataDir, "backups", "pre-migration", "sqlite_v1.db.gz"); backupPath != want {
		t.Fatalf("backup path = %q, want %q", backupPath, want)
	}
	if got := mustVersion(t, db); got != 2 {
		t.Fatalf("db version = %d, want 2", got)
	}
}

// cleanupCall is one recorded invocation of the cleanup seam.
type cleanupCall struct {
	dir  string
	keep string
}

// recordingCleanup swaps the package-level backup-cleanup seam for a fake
// that records every call, restoring the real cleanup when the test ends.
// The seam exists because the real cleanup only reads and deletes: against
// an empty or missing directory it is a silent no-op, so no
// filesystem-shape assertion can detect a call that should never happen —
// the postgres exemption observes the call itself through this fake.
func recordingCleanup(t *testing.T) *[]cleanupCall {
	t.Helper()
	calls := &[]cleanupCall{}
	realCleanup := cleanupPreMigrationBackups
	cleanupPreMigrationBackups = func(dirPath, keepName string) {
		*calls = append(*calls, cleanupCall{dir: dirPath, keep: keepName})
	}
	t.Cleanup(func() { cleanupPreMigrationBackups = realCleanup })
	return calls
}

// TestMigrateWithBackupPostgresNeverConsultsSpaceProbe guards the postgres
// exemption: neither with a pending upgrade nor without one may the startup
// path consult the space probe, run the backup cleanup, or write anything
// to the filesystem — no precheck, no backup, no cleanup, behavior
// unchanged.
//
// The sqlite path deliberately points at a real, non-empty leftover file
// from a former sqlite deployment, kept OUTSIDE the watched data dir. A
// precheck mistakenly wired into the postgres branch must size that file
// and consult the armed probe; pointing at a missing file instead would let
// the precheck's own missing-file skip keep this test green while the
// production sqlite-to-postgres switch — which leaves exactly such a
// leftover behind — gets refused by the real OS probe.
func TestMigrateWithBackupPostgresNeverConsultsSpaceProbe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target int
	}{
		{"pending upgrade", 2},
		{"nothing pending", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			// A leftover sqlite database from before the postgres switch:
			// non-empty so a mistakenly wired precheck computes a real
			// requirement the armed zero-free probe rejects, not a valid
			// sqlite file so a mistakenly wired snapshot attempt fails
			// loudly instead of quietly succeeding, and outside dataDir so
			// its existence cannot be misread as the postgres branch
			// writing something.
			leftoverDir := t.TempDir()
			sqlitePath := filepath.Join(leftoverDir, "leftover-from-sqlite-era.db")
			mustWriteFile(t, sqlitePath, make([]byte, 1<<10))
			// An in-memory SQLite connection stands in for postgres; it is
			// migrated to v1 first so the pending-upgrade probe has a real
			// version to read in the "pending" case.
			db := newMemoryDB(t)
			if err := RunMigrations(db, "sqlite", testMigrationsUpTo(1), "sqlite"); err != nil {
				t.Fatalf("stage stand-in at v1: %v", err)
			}
			p := &recordingSpaceProbe{free: 0} // rejects instantly if consulted
			cleanupCalls := recordingCleanup(t)

			_, err := MigrateWithBackup(db, "postgres", sqlitePath, testMigrationsUpTo(tc.target), "sqlite", p.probe)
			// The stand-in fails at the postgres advisory lock — the proof
			// the call reached the migration path rather than stopping at a
			// precheck or backup stage.
			if err == nil || !strings.Contains(err.Error(), "pg_advisory_lock") {
				t.Fatalf("expected the postgres migration path to be reached, got: %v", err)
			}
			if len(p.calls) != 0 {
				t.Fatalf("space probe was consulted %d time(s) on the postgres path: %v", len(p.calls), p.calls)
			}
			if len(*cleanupCalls) != 0 {
				t.Fatalf("backup cleanup ran on the postgres path: %v", *cleanupCalls)
			}
			// Neither directory may change: no backup, no backup directory,
			// no scratch file — the leftover file itself is the only thing
			// allowed to exist in leftoverDir.
			for _, dir := range []string{dataDir, leftoverDir} {
				wantEntries := 0
				if dir == leftoverDir {
					wantEntries = 1
				}
				entries, readErr := os.ReadDir(dir)
				if readErr != nil {
					t.Fatalf("read %s: %v", dir, readErr)
				}
				if len(entries) != wantEntries {
					t.Fatalf("postgres branch changed %s: %v", dir, entries)
				}
			}
		})
	}
}

// TestMigrateWithBackupFailureSentinels pins that both operator-facing
// failure stages surface as *MigrationFailedError (so a dispatcher can map
// the whole class to one dedicated exit code), with the backup-stage
// variant carrying no snapshot path and the migration-stage variant naming
// the preserved rollback point.
func TestMigrateWithBackupFailureSentinels(t *testing.T) {
	t.Run("backup failure", func(t *testing.T) {
		db, sqlitePath := stageDB(t, 1)
		// Occupy the backup directory's path with a regular file so
		// creating the backup is impossible.
		mustWriteFile(t, filepath.Join(filepath.Dir(sqlitePath), "backups", "pre-migration"), []byte("in the way"))

		_, err := MigrateWithBackup(db, "sqlite", sqlitePath, testMigrationsUpTo(2), "sqlite", unlimitedSpaceProbe)
		var mf *MigrationFailedError
		if !errors.As(err, &mf) {
			t.Fatalf("expected a *MigrationFailedError, got %T: %v", err, err)
		}
		if mf.SnapshotPath != "" {
			t.Fatalf("SnapshotPath = %q, want empty (no snapshot was taken)", mf.SnapshotPath)
		}
		if !strings.Contains(err.Error(), "pre-migration backup failed, refusing to migrate") {
			t.Errorf("backup failure lost its refusal wording: %s", err.Error())
		}
		var rejected *PrecheckRejectedError
		if errors.As(err, &rejected) {
			t.Error("a backup failure must not be typed as a precheck rejection")
		}
	})
	t.Run("migration failure", func(t *testing.T) {
		db, sqlitePath := stageDB(t, 2)
		broken := testMigrationsUpTo(3)
		breakVersion(broken, 3)

		_, err := MigrateWithBackup(db, "sqlite", sqlitePath, broken, "sqlite", unlimitedSpaceProbe)
		var mf *MigrationFailedError
		if !errors.As(err, &mf) {
			t.Fatalf("expected a *MigrationFailedError, got %T: %v", err, err)
		}
		if filepath.Base(mf.SnapshotPath) != "sqlite_v2.db.gz" {
			t.Fatalf("SnapshotPath = %q, want the just-taken sqlite_v2.db.gz", mf.SnapshotPath)
		}
		if !strings.Contains(err.Error(), mf.SnapshotPath) {
			t.Errorf("migration failure does not surface the snapshot path: %s", err.Error())
		}
	})
}
