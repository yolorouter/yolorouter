//go:build !release

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/yolorouter/yolorouter/pkg/database"
)

func runDBReset(ctx context.Context, args []string) error {
	var yes *bool
	_, app, err := bootstrapCommand("db:reset", args, 0, func(fs *flag.FlagSet) {
		yes = fs.Bool("yes", false, "skip interactive confirmation (for scripting)")
	})
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()

	fmt.Printf("target database: driver=%s\n", app.Config.Database.Driver)
	if !*yes {
		fmt.Print("this will delete ALL data and re-migrate. type \"yes\" to continue: ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		if answer != "yes\n" {
			return fmt.Errorf("aborted: confirmation not given")
		}
	}

	migrationsFS, dir := migrationsFor(app.Config.Database.Driver)
	lockPath := instanceLockPath(app.Config.Database.SQLitePath)

	if app.Config.Database.Driver == "sqlite" {
		// bootstrapCommand's bootstrap.Init already opened a connection to
		// this exact file (app.DB) — ResetSQLite is about to delete that
		// file out from under it, so close it first. sql.DB.Close is safe to
		// call again later via the deferred app.Close().
		sqlDB, err := app.DB.DB()
		if err != nil {
			return err
		}
		if err := sqlDB.Close(); err != nil {
			return fmt.Errorf("close pre-reset database connection: %w", err)
		}
		// ResetSQLite acquires lockPath itself (see pkg/database/reset_sqlite.go).
		return database.ResetSQLite(app.Config.Database.SQLitePath, lockPath, migrationsFS, dir)
	}

	// ResetPostgres acquires lockPath itself too (see pkg/database/reset_postgres.go) —
	// both Reset* functions are self-contained about the mutual-exclusion
	// precondition against a running `serve` instance (design doc §2.2).
	sqlDB, err := app.DB.DB()
	if err != nil {
		return err
	}
	return database.ResetPostgres(sqlDB, app.Config.Database.Driver, migrationsFS, dir, lockPath)
}
