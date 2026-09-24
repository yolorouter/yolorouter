package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/pkg/database"
)

func TestDispatchUnknownCommandReturnsError(t *testing.T) {
	_, err := dispatch(context.Background(), []string{"nonexistent-command"})
	if err == nil {
		t.Fatalf("expected error for unknown command")
	}
}

func TestDispatchNoArgsReturnsHelpExitZero(t *testing.T) {
	code, err := dispatch(context.Background(), []string{})
	if err != nil {
		t.Fatalf("no-args should not error: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected exit code 0 for help, got %d", code)
	}
}

func TestDispatchVersionExitZero(t *testing.T) {
	code, err := dispatch(context.Background(), []string{"--version"})
	if err != nil {
		t.Fatalf("--version should not error: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected exit code 0 for --version, got %d", code)
	}
}

func TestDispatchSubcommandHelpExitZero(t *testing.T) {
	// serve --help (and every other subcommand's --help) must exit 0 without
	// initializing any resources, even though
	// the underlying flag.FlagSet.Parse surfaces flag.ErrHelp as an error.
	code, err := dispatch(context.Background(), []string{"serve", "--help"})
	if err != nil {
		t.Fatalf("serve --help should not surface as an error: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected exit code 0 for serve --help, got %d", code)
	}
}

func TestRegisteredCommandRunError(t *testing.T) {
	// Register a fake command that always errors, verifying dispatch propagates the Run error correctly
	failing := Command{Name: "always-fails", Usage: "test", Run: func(ctx context.Context, args []string) error {
		return errors.New("boom")
	}}
	commands = append(commands, failing)
	defer func() { commands = commands[:len(commands)-1] }()

	_, err := dispatch(context.Background(), []string{"always-fails"})
	if err == nil {
		t.Fatalf("expected error to propagate from Run")
	}
}

// dispatchExitCodeFor registers a one-shot command whose Run returns runErr
// and reports what dispatch decided for it. The sentinels normally originate
// deep inside runServe's migration path; injecting them through a fake
// command isolates the mapping under test from all of that wiring, while the
// "wrapped like runServe" cases keep the real chain shape honest.
func dispatchExitCodeFor(t *testing.T, runErr error) (int, error) {
	t.Helper()
	commands = append(commands, Command{
		Name:  "exit-code-probe",
		Usage: "test",
		Run:   func(ctx context.Context, args []string) error { return runErr },
	})
	t.Cleanup(func() { commands = commands[:len(commands)-1] })
	return dispatch(context.Background(), []string{"exit-code-probe"})
}

// Both upgrade-chain failure classes — a disk-space precheck refusal and a
// backup/migration failure — must share ONE dedicated exit code (anything
// but 1), because both mean the same thing to whoever reads the journal or
// `systemctl status`: the upgrade chain is stuck, a human has to intervene.
// Each case also pins its output shape: the failure's own line stays first,
// and the appended hint carries the data-safety sentence that is TRUE for
// that form — a snapshot exists to point at only when the migration failed
// after one was preserved. The refusal forms' hint must not mention a
// snapshot at all: their failure lines name no snapshot path, so "the
// snapshot named above" would dangle and send an operator hunting for a
// file that was never written.
func TestDispatchMapsUpgradeChainSentinelsToDedicatedExitCode(t *testing.T) {
	if exitUpgradeChainFailed == 1 {
		t.Fatalf("the dedicated upgrade-chain exit code must differ from the generic failure code 1")
	}

	const snapshot = "/srv/yolorouter/data/backups/pre-migration/sqlite_v1.db.gz"
	cases := []struct {
		name           string
		runErr         error
		wantLinePrefix string
		wantDataSafety string
		hasSnapshot    bool
	}{
		{
			name: "precheck rejection",
			runErr: &database.PrecheckRejectedError{
				RequiredBytes: 2748779069,
				FreeBytes:     52428800,
				BackupDir:     "/srv/yolorouter/data/backups/pre-migration",
			},
			wantLinePrefix: "insufficient disk space for the pre-migration backup",
			wantDataSafety: "the upgrade was refused and the database itself is untouched",
			hasSnapshot:    false,
		},
		{
			name:           "backup failure before any snapshot",
			runErr:         &database.MigrationFailedError{Err: errors.New("write /srv/yolorouter/data/backups/pre-migration/sqlite_v1.db.gz.partial: no space left on device")},
			wantLinePrefix: "pre-migration backup failed, refusing to migrate",
			wantDataSafety: "the upgrade was refused and the database itself is untouched",
			hasSnapshot:    false,
		},
		{
			name:           "migration failure with preserved snapshot",
			runErr:         &database.MigrationFailedError{SnapshotPath: snapshot, Err: errors.New("migration 2 failed: disk I/O error")},
			wantLinePrefix: "migration failed, pre-migration snapshot preserved at " + snapshot,
			wantDataSafety: "recoverable from the pre-migration snapshot named above",
			hasSnapshot:    true,
		},
		{
			name:           "migration failure wrapped the way runServe returns it",
			runErr:         fmt.Errorf("startup migration failed: %w", &database.MigrationFailedError{SnapshotPath: snapshot, Err: errors.New("migration 2 failed: disk I/O error")}),
			wantLinePrefix: "startup migration failed: migration failed, pre-migration snapshot preserved at " + snapshot,
			wantDataSafety: "recoverable from the pre-migration snapshot named above",
			hasSnapshot:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := dispatchExitCodeFor(t, tc.runErr)
			if code != exitUpgradeChainFailed {
				t.Fatalf("expected the dedicated upgrade-chain exit code %d, got %d", exitUpgradeChainFailed, code)
			}
			if err == nil {
				t.Fatalf("expected the failure to propagate as an error too")
			}

			msg := err.Error()
			if !strings.HasPrefix(msg, tc.wantLinePrefix) {
				t.Fatalf("the failure's own line must stay intact and first, got: %q", msg)
			}
			_, hint, ok := strings.Cut(msg, "\n")
			if !ok {
				t.Fatalf("the operator hint must be appended under the failure line, got: %q", msg)
			}
			if !strings.Contains(hint, tc.wantDataSafety) {
				t.Fatalf("the hint must carry this form's data-safety sentence %q, got: %q", tc.wantDataSafety, hint)
			}
			if !tc.hasSnapshot && strings.Contains(hint, "snapshot") {
				t.Fatalf("no snapshot exists in the %q form (its failure line names none), so the hint must not point at one — an operator would hunt for a file that was never written, got: %q", tc.name, hint)
			}
		})
	}
}

// Everything that is not an upgrade-chain sentinel keeps the generic code 1,
// with its error message untouched — the dedicated code and the appended
// hint are for one failure class only.
func TestDispatchKeepsGenericFailuresAtExitCodeOne(t *testing.T) {
	code, err := dispatchExitCodeFor(t, errors.New("listen: address already in use"))
	if code != 1 {
		t.Fatalf("expected exit code 1 for a generic command failure, got %d", code)
	}
	if err == nil || err.Error() != "listen: address already in use" {
		t.Fatalf("generic failures must propagate unchanged, got %q", err)
	}

	// The unknown-command path is the other generic failure exit.
	code, err = dispatch(context.Background(), []string{"no-such-command"})
	if code != 1 {
		t.Fatalf("expected exit code 1 for an unknown command, got %d", code)
	}
	if err == nil {
		t.Fatalf("expected an error for an unknown command")
	}
}

// The process's last stderr line(s) are what an operator actually reads, so
// the mapping must not cost them the snapshot path: the original failure
// line stays first, the plain-language guidance is appended under it, and
// the wrap keeps the sentinel detectable for any consumer of the error.
func TestDispatchMigrationFailureOutputKeepsSnapshotLineAndAddsOperatorHint(t *testing.T) {
	const snapshot = "/srv/yolorouter/data/backups/pre-migration/sqlite_v1.db.gz"
	runErr := fmt.Errorf("startup migration failed: %w",
		&database.MigrationFailedError{SnapshotPath: snapshot, Err: errors.New("migration 2 failed: disk I/O error")})

	code, err := dispatchExitCodeFor(t, runErr)
	if code != exitUpgradeChainFailed {
		t.Fatalf("expected the dedicated upgrade-chain exit code %d, got %d", exitUpgradeChainFailed, code)
	}

	msg := err.Error()
	if !strings.HasPrefix(msg, "startup migration failed: migration failed, pre-migration snapshot preserved at "+snapshot+": migration 2 failed: disk I/O error") {
		t.Fatalf("the original failure line naming the preserved snapshot must stay intact and first, got: %q", msg)
	}
	if !strings.Contains(msg, "systemctl reset-failed yolorouter && systemctl restart yolorouter") {
		t.Fatalf("the appended hint must carry the systemd recovery command, got: %q", msg)
	}
	if !strings.HasSuffix(msg, upgradeChainOperatorHint(runErr)) {
		t.Fatalf("the hint must be appended verbatim after the failure line, got: %q", msg)
	}

	// The hint wrap must remain transparent: %w, not a formatted copy, so
	// errors.As still resolves the sentinel through dispatch's error.
	var migrationFailed *database.MigrationFailedError
	if !errors.As(err, &migrationFailed) || migrationFailed.SnapshotPath != snapshot {
		t.Fatalf("the sentinel must stay detectable through the wrapped error, got: %v", err)
	}
}

// A backup-stage failure never produced a snapshot — its failure line
// ("pre-migration backup failed, refusing to migrate: ...") names no path —
// so the data is safe for a different reason: the migration was refused,
// fail-closed, and the database itself is untouched. The hint must say
// exactly that and must NOT reference a snapshot (whose "named above" would
// dangle), or an operator would go hunting for a file that does not exist.
func TestDispatchBackupFailureOutputNamesNoSnapshotAndAddsOperatorHint(t *testing.T) {
	runErr := fmt.Errorf("startup migration failed: %w",
		&database.MigrationFailedError{Err: errors.New("write /srv/yolorouter/data/backups/pre-migration/sqlite_v1.db.gz.partial: no space left on device")})

	code, err := dispatchExitCodeFor(t, runErr)
	if code != exitUpgradeChainFailed {
		t.Fatalf("expected the dedicated upgrade-chain exit code %d, got %d", exitUpgradeChainFailed, code)
	}

	msg := err.Error()
	if !strings.HasPrefix(msg, "startup migration failed: pre-migration backup failed, refusing to migrate: write /srv/yolorouter/data/backups/pre-migration/sqlite_v1.db.gz.partial: no space left on device") {
		t.Fatalf("the original failure line must stay intact and first, got: %q", msg)
	}
	_, hint, ok := strings.Cut(msg, "\n")
	if !ok {
		t.Fatalf("the operator hint must be appended under the failure line, got: %q", msg)
	}
	if !strings.Contains(hint, "the upgrade was refused and the database itself is untouched") {
		t.Fatalf("the hint must state the real reason the data is safe in the no-snapshot form, got: %q", hint)
	}
	if strings.Contains(hint, "snapshot") {
		t.Fatalf("a backup-stage failure has no snapshot to name; referencing one sends the operator hunting for a nonexistent file, got: %q", hint)
	}
	if !strings.Contains(hint, "systemctl reset-failed yolorouter && systemctl restart yolorouter") {
		t.Fatalf("the appended hint must carry the systemd recovery command, got: %q", hint)
	}
	if !strings.HasSuffix(msg, upgradeChainOperatorHint(runErr)) {
		t.Fatalf("the hint must be appended verbatim after the failure line, got: %q", msg)
	}

	// The hint wrap must remain transparent: %w, not a formatted copy, so
	// errors.As still resolves the sentinel through dispatch's error, with
	// its empty SnapshotPath (the no-snapshot marker) intact.
	var migrationFailed *database.MigrationFailedError
	if !errors.As(err, &migrationFailed) || migrationFailed.SnapshotPath != "" {
		t.Fatalf("the sentinel must stay detectable through the wrapped error with no snapshot path, got: %v", err)
	}
}

// A precheck refusal renders its own numbers-and-what-to-do line; the hint
// adds the recovery command without disturbing either.
func TestDispatchPrecheckRejectionOutputKeepsNumbersAndAddsOperatorHint(t *testing.T) {
	runErr := fmt.Errorf("startup migration failed: %w",
		&database.PrecheckRejectedError{
			RequiredBytes: 26214400,
			FreeBytes:     1572864,
			BackupDir:     "/srv/yolorouter/data/backups/pre-migration",
		})

	code, err := dispatchExitCodeFor(t, runErr)
	if code != exitUpgradeChainFailed {
		t.Fatalf("expected the dedicated upgrade-chain exit code %d, got %d", exitUpgradeChainFailed, code)
	}

	msg := err.Error()
	for _, want := range []string{"need about 25.0 MiB", "only 1.5 MiB free", "the database is untouched"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the refusal's own numbers and guidance must survive, missing %q in: %q", want, msg)
		}
	}
	if !strings.Contains(msg, "systemctl reset-failed yolorouter && systemctl restart yolorouter") {
		t.Fatalf("the appended hint must carry the systemd recovery command, got: %q", msg)
	}

	var precheckRejected *database.PrecheckRejectedError
	if !errors.As(err, &precheckRejected) || precheckRejected.RequiredBytes != 26214400 {
		t.Fatalf("the sentinel must stay detectable through the wrapped error, got: %v", err)
	}
}
