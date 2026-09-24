package database

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// sqliteFileSize sizes the staged database file the way the estimate does,
// so a probe value of required-1 is exactly one byte short regardless of
// how big the staged file happens to be.
func sqliteFileSize(t *testing.T, sqlitePath string) int64 {
	t.Helper()
	fi, err := os.Stat(sqlitePath)
	if err != nil {
		t.Fatalf("stat %s: %v", sqlitePath, err)
	}
	return fi.Size()
}

// backupDirEntryCount counts the snapshot files in dir, for probes that
// need to observe the directory state at consult time (the rotation must
// have already trimmed it).
func backupDirEntryCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return len(entries)
}

// stageOldBackups plants n preset snapshots v1..vn (payload bytes are
// irrelevant — the rotation ranks by filename version alone).
func stageOldBackups(t *testing.T, backupDir string, versions ...int) {
	t.Helper()
	for _, v := range versions {
		mustWriteGzip(t, filepath.Join(backupDir, preMigrationBackupFilename(int64(v))), []byte("old"))
	}
}

func backupFileExists(t *testing.T, backupDir string, v int) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(backupDir, preMigrationBackupFilename(int64(v))))
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat preset v%d: %v", v, err)
	return false
}

// TestNewUpdatePreflightRotatesThenRefuses pins the button-layer order and
// numbers: the directory must already be trimmed when the probe is
// consulted, and a still-insufficient filesystem refuses as a
// *PrecheckRejectedError with the exact required/free figures. The db sits
// at a version whose pin name is not among the presets, so the terminal
// state is exactly the newest three.
func TestNewUpdatePreflightRotatesThenRefuses(t *testing.T) {
	db, sqlitePath := stageDB(t, 9)
	backupDir := preMigrationBackupDir(sqlitePath)
	stageOldBackups(t, backupDir, 1, 2, 3, 4, 5)

	var trimmedWhenProbed bool
	probeCalls := 0
	required := estimatePeakBackupNeed(sqliteFileSize(t, sqlitePath))
	probe := func(string) (int64, error) {
		probeCalls++
		trimmedWhenProbed = backupDirEntryCount(t, backupDir) == 3
		return required - 1, nil
	}

	err := NewUpdatePreflight("sqlite", db, sqlitePath, probe)()
	var rejected *PrecheckRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected a *PrecheckRejectedError, got %v", err)
	}
	if !trimmedWhenProbed {
		t.Fatal("the probe was consulted before the rotation trimmed the directory — order must be rotate first, then size")
	}
	if probeCalls != 1 {
		t.Fatalf("probe consulted %d time(s), want 1 (single estimate, single probe)", probeCalls)
	}
	if rejected.RequiredBytes != required || rejected.FreeBytes != required-1 || rejected.BackupDir != backupDir {
		t.Fatalf("rejection fields = required %d / free %d / dir %q, want %d / %d / %q",
			rejected.RequiredBytes, rejected.FreeBytes, rejected.BackupDir, required, required-1, backupDir)
	}
	for _, v := range []int{1, 2} {
		if backupFileExists(t, backupDir, v) {
			t.Fatalf("expected v%d rotated away before the refusal", v)
		}
	}
	for _, v := range []int{3, 4, 5} {
		if !backupFileExists(t, backupDir, v) {
			t.Fatalf("expected v%d to survive the rotation", v)
		}
	}
}

// TestNewUpdatePreflightPassStillRotatesThroughSeam: enough space passes,
// the rotation is unconditional, and — the wiring contract the startup
// path already owes — the call goes through the package-level cleanup var
// rather than a private side path: with the var swapped for the recorder,
// nothing is deleted and exactly one call is recorded with the directory
// and the pin derived from the current schema version.
func TestNewUpdatePreflightPassStillRotatesThroughSeam(t *testing.T) {
	db, sqlitePath := stageDB(t, 4)
	backupDir := preMigrationBackupDir(sqlitePath)
	stageOldBackups(t, backupDir, 1, 2, 3, 4)

	calls := recordingCleanup(t)
	if err := NewUpdatePreflight("sqlite", db, sqlitePath, unlimitedSpaceProbe)(); err != nil {
		t.Fatalf("expected a pass, got %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("cleanup called %d time(s), want 1 even on a pass", len(*calls))
	}
	if (*calls)[0].dir != backupDir || (*calls)[0].keep != preMigrationBackupFilename(4) {
		t.Fatalf("cleanup call = %+v, want {dir:%s keep:%s}", (*calls)[0], backupDir, preMigrationBackupFilename(4))
	}
	// The recorder deleted nothing: a bypass around the var (a real cleanup
	// on the sqlite path) would have trimmed v1..v4 to v2..v4 here.
	for _, v := range []int{1, 2, 3, 4} {
		if !backupFileExists(t, backupDir, v) {
			t.Fatalf("expected v%d untouched while the seam is swapped — the preflight must call through the var, not a private copy", v)
		}
	}
}

// TestNewUpdatePreflightPinsSnapshotOfNextBackup pins the button-layer pin
// semantics: the pinned name is the snapshot file the post-update restart's
// backup will (over)write — the current schema version — so a database
// restored to an older schema keeps its recovery point through the
// rotation while a newer, cap-exceeding one goes (the same boundary the
// startup rotation protects, held at the earlier layer).
func TestNewUpdatePreflightPinsSnapshotOfNextBackup(t *testing.T) {
	// Restored-schema shape: the database sits at v2 while snapshots v2 and
	// v5..v8 exist (the cap keeps v6..v8; beyond it stand v2 and v5).
	db, sqlitePath := stageDB(t, 2)
	backupDir := preMigrationBackupDir(sqlitePath)
	stageOldBackups(t, backupDir, 2, 5, 6, 7, 8)

	// Freeze at the refusal so only the rotation's deletions are observed.
	required := estimatePeakBackupNeed(sqliteFileSize(t, sqlitePath))
	probe := func(string) (int64, error) { return required - 1, nil }
	if err := NewUpdatePreflight("sqlite", db, sqlitePath, probe)(); err == nil {
		t.Fatal("expected the insufficient probe to refuse")
	}
	for _, v := range []int{2, 6, 7, 8} {
		if !backupFileExists(t, backupDir, v) {
			t.Fatalf("expected v%d to survive the rotation", v)
		}
	}
	if backupFileExists(t, backupDir, 5) {
		t.Fatal("expected v5 (beyond the cap, not the pin) rotated away")
	}
}

// TestNewUpdatePreflightPostgresExempt guards the non-sqlite exemption of
// the button layer: neither the space probe nor the rotation cleanup may
// be consulted, and nothing on disk may change — exactly like the startup
// path's postgres branch. The sqlite path points at a real, non-empty
// leftover file (kept outside the watched dir) so a precheck mistakenly
// wired into the non-sqlite branch would size it and trip the armed probe;
// preset backups under the derived directory catch a rotation wired (or
// bypassed around the seam) into that branch.
func TestNewUpdatePreflightPostgresExempt(t *testing.T) {
	leftoverDir := t.TempDir()
	leftoverPath := filepath.Join(leftoverDir, "yolorouter.db")
	if err := os.WriteFile(leftoverPath, []byte("leftover sqlite bytes"), 0o600); err != nil {
		t.Fatalf("stage leftover sqlite file: %v", err)
	}
	stageOldBackups(t, preMigrationBackupDir(leftoverPath), 1, 2, 3, 4, 5)

	probeCalls := 0
	probe := func(string) (int64, error) {
		probeCalls++
		return 0, nil
	}
	calls := recordingCleanup(t)

	if err := NewUpdatePreflight("postgres", nil, leftoverPath, probe)(); err != nil {
		t.Fatalf("postgres deployment must pass unconditionally, got %v", err)
	}
	if probeCalls != 0 {
		t.Fatalf("space probe consulted %d time(s) on the postgres path, want 0", probeCalls)
	}
	if len(*calls) != 0 {
		t.Fatalf("backup cleanup ran on the postgres path: %+v", *calls)
	}
	for v := 1; v <= 5; v++ {
		if !backupFileExists(t, preMigrationBackupDir(leftoverPath), v) {
			t.Fatalf("expected preset v%d untouched on the postgres path", v)
		}
	}
}

// TestNewUpdatePreflightNilOrUnreadableVersionRotatesWithoutPin covers the
// degrade path: a nil handle (assembly variants) or a version that cannot
// be read must not turn the preflight into a failure source — the rotation
// runs with an empty pin and the space check still applies. An empty pin
// is observable because the oldest preset goes: had the version been read,
// that preset would be the pin and survive.
func TestNewUpdatePreflightNilOrUnreadableVersionRotatesWithoutPin(t *testing.T) {
	t.Run("nil db", func(t *testing.T) {
		dir := t.TempDir()
		sqlitePath := filepath.Join(dir, "yolorouter.db")
		if err := os.WriteFile(sqlitePath, []byte("abcd"), 0o600); err != nil {
			t.Fatalf("stage sqlite file: %v", err)
		}
		backupDir := preMigrationBackupDir(sqlitePath)
		stageOldBackups(t, backupDir, 1, 2, 3, 4)

		required := estimatePeakBackupNeed(4)
		probe := func(string) (int64, error) { return required - 1, nil }
		var rejected *PrecheckRejectedError
		if err := NewUpdatePreflight("sqlite", nil, sqlitePath, probe)(); !errors.As(err, &rejected) {
			t.Fatalf("nil db must degrade to an empty pin, not skip the check; got %v", err)
		}
		if rejected.RequiredBytes != 10 || rejected.FreeBytes != 9 {
			t.Fatalf("rejection numbers = %d/%d, want 10/9", rejected.RequiredBytes, rejected.FreeBytes)
		}
		if backupFileExists(t, backupDir, 1) {
			t.Fatal("expected v1 rotated away — an empty pin spares nothing")
		}
		for _, v := range []int{2, 3, 4} {
			if !backupFileExists(t, backupDir, v) {
				t.Fatalf("expected v%d to survive", v)
			}
		}
	})

	t.Run("unreadable version", func(t *testing.T) {
		// A closed handle cannot answer the version query; the preflight
		// must warn, rotate with an empty pin, and still check space.
		db, sqlitePath := stageDB(t, 1)
		if err := db.Close(); err != nil {
			t.Fatalf("close staged db: %v", err)
		}
		backupDir := preMigrationBackupDir(sqlitePath)
		stageOldBackups(t, backupDir, 1, 2, 3, 4)

		if err := NewUpdatePreflight("sqlite", db, sqlitePath, unlimitedSpaceProbe)(); err != nil {
			t.Fatalf("an unreadable version must not become a refusal, got %v", err)
		}
		if backupFileExists(t, backupDir, 1) {
			t.Fatal("expected v1 rotated away — the unreadable version degrades to an empty pin (had it been read, v1 would be the pin and survive)")
		}
	})
}
