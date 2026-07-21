package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/yolorouter/yolorouter/internal/version"
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
				// flag.ErrHelp. That's a normal, successful exit (design
				// doc §14 criterion 6: "serve --help 同样退出码 0"), not a
				// real failure — the flag package already printed usage.
				if errors.Is(err, flag.ErrHelp) {
					return 0, nil
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
