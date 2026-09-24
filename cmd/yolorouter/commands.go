package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/yolorouter/yolorouter/internal/version"
	"github.com/yolorouter/yolorouter/pkg/database"
)

type Command struct {
	Name  string
	Usage string
	Run   func(ctx context.Context, args []string) error
}

var commands = []Command{
	{Name: "serve", Usage: "start the HTTP server and background task supervisor", Run: runServe},
	{Name: "db:migrate", Usage: "run pending migrations", Run: runDBMigrate},
	{Name: "db:rollback", Usage: "roll back one migration, or to [version]", Run: runDBRollback},
	{Name: "db:status", Usage: "show current migration version", Run: runDBStatus},
	{Name: "db:backup", Usage: "back up the database", Run: runDBBackup},
	{Name: "db:reset", Usage: "drop all tables and re-migrate (dangerous)", Run: runDBReset},
	{Name: "update", Usage: "update to the latest GitHub release", Run: runUpdate},
	{Name: "stop", Usage: "stop the running server", Run: runStop},
}

// exitUpgradeChainFailed is the dedicated process exit code for "the startup
// upgrade chain is stuck and needs an operator": the disk-space precheck
// refused to migrate (*database.PrecheckRejectedError) or the
// backup/migration itself failed (*database.MigrationFailedError). Both
// share this one code because they call for the same response — a human
// fixes the underlying cause before the service may start again — while
// every other failure keeps the generic code 1. The value is sysexits.h's
// EX_CONFIG, the closest classic status to "cannot proceed until the
// environment is fixed". Service managers that restart on any non-zero
// exit (Restart=always) treat it exactly like 1; the number carries
// meaning only for operators and scripts reading the journal or
// `systemctl status`, distinguishing "the upgrade chain is stuck" from any
// other startup error.
const exitUpgradeChainFailed = 78

// upgradeChainOperatorHint returns the operator guidance appended to the
// error dispatch returns when it maps a failure to exitUpgradeChainFailed,
// so the process's final stderr output reads cause-first, guidance-under-it:
// the error line followed by what an operator should actually do next. It
// rides on the returned error rather than being written to os.Stderr inside
// dispatch so main's single error print stays the one output path, and so
// embedders of dispatch get the same text.
//
// The data-safety sentence is the one part that depends on the failure
// form, because only one of the three forms actually has a snapshot to
// point at: a migration that failed after a snapshot was preserved names
// that path in the error line above it. The other two — a disk-space
// precheck refusal and a backup-stage failure — protected the data the
// other way, by refusing to migrate at all (fail-closed), and their error
// lines carry no snapshot path; for those, "the snapshot named above"
// would dangle and send an operator hunting for a file that was never
// written.
func upgradeChainOperatorHint(err error) string {
	dataSafety := "the upgrade was refused and the database itself is untouched"
	var migrationFailed *database.MigrationFailedError
	if errors.As(err, &migrationFailed) && migrationFailed.SnapshotPath != "" {
		dataSafety = "recoverable from the pre-migration snapshot named above"
	}
	return fmt.Sprintf(`This exit is deliberate: the startup database upgrade could not be completed
safely and needs an operator before the service can start again. Your data is
protected — %s. Fix the cause reported in this message,
then start the service again. Under systemd, clear a tripped start limit
first: systemctl reset-failed yolorouter && systemctl restart yolorouter
(user-level installs: add --user to both systemctl commands)`, dataSafety)
}

// dispatch resolves args[0] against the command table and runs it,
// returning the process exit code. It never calls os.Exit itself so it can
// be unit tested directly.
func dispatch(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printUsage()
		return 0, nil
	}
	if args[0] == "--version" || args[0] == "-v" || args[0] == "version" {
		fmt.Println(version.Version)
		return 0, nil
	}

	name := args[0]
	rest := args[1:]

	for _, cmd := range commands {
		if cmd.Name == name {
			// Each command builds and parses its own flag.FlagSet inside Run
			// (see bootstrapCommand in db_commands.go) rather than one being
			// pre-built here, so sub-command-level --help is handled by that
			// inner FlagSet — surfaced back to us as flag.ErrHelp below.
			if err := cmd.Run(ctx, rest); err != nil {
				// A subcommand's own `--help`/`-h` (parsed by its private
				// flag.FlagSet in serve.go/db_commands.go) surfaces as
				// flag.ErrHelp. That's a normal, successful exit, not a
				// real failure — the flag package already printed usage.
				if errors.Is(err, flag.ErrHelp) {
					return 0, nil
				}
				// An upgrade-chain refusal is a class of its own: runServe
				// wraps MigrateWithBackup's result, so the sentinels reach
				// here through %w chains and errors.As sees through them.
				// errors.As (not errors.Is) because these are typed
				// payloads carrying the numbers and the snapshot path, not
				// comparable singletons. Mapping by type here — rather than
				// each command choosing its own code — keeps "which failures
				// mean the upgrade chain is stuck" defined in exactly one
				// place, for every current and future command.
				var precheckRejected *database.PrecheckRejectedError
				var migrationFailed *database.MigrationFailedError
				if errors.As(err, &precheckRejected) || errors.As(err, &migrationFailed) {
					return exitUpgradeChainFailed, fmt.Errorf("%w\n%s", err, upgradeChainOperatorHint(err))
				}
				return 1, err
			}
			return 0, nil
		}
	}

	printUsage()
	return 1, fmt.Errorf("unknown command: %s", name)
}

func printUsage() {
	fmt.Println("Usage: yolorouter <command> [flags]")
	fmt.Println("\nCommands:")
	for _, cmd := range commands {
		fmt.Printf("  %-15s %s\n", cmd.Name, cmd.Usage)
	}
	fmt.Println("  --help, -h      show this help")
	fmt.Println("  --version, -v   show version")
}
