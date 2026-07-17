package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"strconv"
	"time"

	"github.com/yolorouter/yolorouter-ce/internal/bootstrap"
	"github.com/yolorouter/yolorouter-ce/internal/config"
	"github.com/yolorouter/yolorouter-ce/migrations"
	"github.com/yolorouter/yolorouter-ce/pkg/database"
)

// parseCommandFlags builds a FlagSet named `name` with the shared --config
// flag (plus whatever extra registers), parses args, and rejects any
// positional arguments beyond maxPositional — all before any resource
// initialization happens, so a malformed invocation never touches
// bootstrap.Init. extra may be nil for commands that take no flags beyond
// --config.
//
// maxPositional matters here because of a Go flag-parsing gotcha:
// flag.FlagSet.Parse stops consuming flags at the first non-flag argument,
// e.g. "db:rollback 1 --config prod.yaml" parses "1" as the positional
// version and leaves "--config"/"prod.yaml" as unconsumed extra args,
// silently ignoring --config. Rejecting extra args (and, for rollback,
// validating the version format) before bootstrap.Init runs — rather than
// after, as an earlier version did — matters because bootstrap.Init already
// connects to (and for SQLite, creates) a database file; catching the
// mistake only after that has run against the wrong default database
// defeats the point.
func parseCommandFlags(name string, args []string, maxPositional int, extra func(fs *flag.FlagSet)) (*flag.FlagSet, error) {
	flagSet := flag.NewFlagSet(name, flag.ContinueOnError)
	flagSet.String("config", "", "path to config.yaml")
	if extra != nil {
		extra(flagSet)
	}
	if err := flagSet.Parse(args); err != nil {
		return nil, err
	}
	if flagSet.NArg() > maxPositional {
		return nil, fmt.Errorf("unexpected extra arguments %v — flags must come before positional arguments", flagSet.Args()[maxPositional:])
	}
	return flagSet, nil
}

// bootstrapCommand wraps parseCommandFlags with bootstrap.Init — the common
// case for every subcommand that needs a live database connection
// (db:migrate, db:status, db:reset, serve). db:rollback and db:backup call
// parseCommandFlags/config.Load directly instead, since each needs to
// validate something (a version argument, source-file existence) before
// bootstrap.Init runs.
func bootstrapCommand(name string, args []string, maxPositional int, extra func(fs *flag.FlagSet)) (*flag.FlagSet, *bootstrap.App, error) {
	flagSet, err := parseCommandFlags(name, args, maxPositional, extra)
	if err != nil {
		return nil, nil, err
	}
	app, err := bootstrap.Init(flagSet.Lookup("config").Value.String())
	if err != nil {
		return nil, nil, err
	}
	return flagSet, app, nil
}

func runDBMigrate(ctx context.Context, args []string) error {
	_, app, err := bootstrapCommand("db:migrate", args, 0, nil)
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()

	sqlDB, err := app.DB.DB()
	if err != nil {
		return err
	}
	migrationsFS, dir := migrationsFor(app.Config.Database.Driver)
	return database.RunMigrations(sqlDB, app.Config.Database.Driver, migrationsFS, dir)
}

func runDBRollback(ctx context.Context, args []string) error {
	flagSet, err := parseCommandFlags("db:rollback", args, 1, nil)
	if err != nil {
		return err
	}

	// Validate the version argument's format before bootstrap.Init runs —
	// "db:rollback abc" must fail immediately, not after already having
	// generated a default config and connected to (and for SQLite, created)
	// a database file.
	hasVersion := flagSet.NArg() > 0
	var version int64
	if hasVersion {
		version, err = strconv.ParseInt(flagSet.Arg(0), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid version argument %q: %w", flagSet.Arg(0), err)
		}
		if version < 0 {
			return fmt.Errorf("invalid version argument %q: migration versions cannot be negative", flagSet.Arg(0))
		}
	}

	app, err := bootstrap.Init(flagSet.Lookup("config").Value.String())
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()

	sqlDB, err := app.DB.DB()
	if err != nil {
		return err
	}
	migrationsFS, dir := migrationsFor(app.Config.Database.Driver)

	if hasVersion {
		return database.RollbackTo(sqlDB, app.Config.Database.Driver, migrationsFS, dir, version)
	}
	return database.Rollback(sqlDB, app.Config.Database.Driver, migrationsFS, dir)
}

func runDBStatus(ctx context.Context, args []string) error {
	_, app, err := bootstrapCommand("db:status", args, 0, nil)
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()

	sqlDB, err := app.DB.DB()
	if err != nil {
		return err
	}
	version, err := database.GetCurrentVersion(sqlDB, app.Config.Database.Driver)
	if err != nil {
		return err
	}
	fmt.Printf("current migration version: %d\n", version)
	return nil
}

func runDBBackup(ctx context.Context, args []string) error {
	var outputDir *string
	flagSet, err := parseCommandFlags("db:backup", args, 0, func(fs *flag.FlagSet) {
		outputDir = fs.String("output-dir", "backups", "directory to write the backup into")
	})
	if err != nil {
		return err
	}

	// Deliberately does NOT go through bootstrapCommand/bootstrap.Init: that
	// would open a database connection before this function ever gets a
	// chance to check anything, and for SQLite specifically, merely
	// connecting creates the file if it's missing — silently turning "back
	// up a database that doesn't exist" into "create an empty database and
	// back that up instead", with no error and a backup file that looks
	// legitimate. db:backup only ever needs the config (for connection
	// parameters), never a live app.DB.
	cfg, err := config.Load(flagSet.Lookup("config").Value.String())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cfg.Database.Driver == "sqlite" {
		// Nanosecond-precision timestamp (not just second-precision) so two
		// backups started within the same second don't collide; BackupSQLite
		// itself also uses unique temp files internally and publishes via a
		// non-overwriting hard link, so even a genuine collision here
		// surfaces as an error rather than silently clobbering a backup.
		// BackupSQLite itself also errors out if the source file doesn't
		// exist yet, rather than fabricating one.
		timestamp := time.Now().UTC().Format("20060102_150405.000000000")
		outputPath := fmt.Sprintf("%s/sqlite_%s.db.gz", *outputDir, timestamp)
		if err := database.BackupSQLite(cfg.Database.SQLitePath, outputPath); err != nil {
			return err
		}
		fmt.Printf("backup written to %s\n", outputPath)
		return nil
	}

	path, err := database.BackupPostgres(database.PostgresDSN{
		Host: cfg.Database.Host, Port: cfg.Database.Port,
		User: cfg.Database.User, Password: cfg.Database.Password,
		DBName: cfg.Database.DBName, SSLMode: cfg.Database.SSLMode,
	}, *outputDir)
	if err != nil {
		return err
	}
	fmt.Printf("backup written to %s\n", path)
	return nil
}

// migrationsFor returns the embedded migration filesystem and goose
// subdirectory for driver, so every subcommand that runs migrations picks
// the same source (design doc §9 — the binary must be self-contained; see
// migrations.SQLiteFS / migrations.PostgresFS).
func migrationsFor(driver string) (fs.FS, string) {
	if driver == "postgres" {
		return migrations.PostgresFS, "postgres"
	}
	return migrations.SQLiteFS, "sqlite"
}

// instanceLockPath returns the cross-process instance lock file path shared
// by `serve` (holds it for its whole lifetime) and `db:reset` (must acquire
// it exclusively before deleting anything) — design doc §2.2. This applies
// to both drivers: even on the Postgres branch, config.go always resolves
// Database.SQLitePath to an absolute path alongside the config file, so it
// doubles as a stable per-deployment location for the lock file.
func instanceLockPath(sqlitePath string) string {
	return sqlitePath + ".lock"
}
