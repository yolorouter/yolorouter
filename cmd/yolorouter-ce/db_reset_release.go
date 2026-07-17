//go:build release

package main

import (
	"context"
	"flag"
	"fmt"
)

// runDBReset is disabled in release builds before any config loading or
// resource initialization happens — see design doc §2.2. It still parses
// its own flags first (mirroring the dev build's --yes/--config surface)
// rather than ignoring args entirely — otherwise "db:reset --help" would
// return this disabled-error with exit code 1 instead of the usual
// exit-0 help behavior every other subcommand's --help gets, and unknown
// flags or extra positional args would be silently accepted instead of
// erroring.
func runDBReset(ctx context.Context, args []string) error {
	flagSet := flag.NewFlagSet("db:reset", flag.ContinueOnError)
	flagSet.String("config", "", "path to config.yaml")
	flagSet.Bool("yes", false, "skip interactive confirmation (for scripting)")
	if err := flagSet.Parse(args); err != nil {
		return err
	}
	if flagSet.NArg() > 0 {
		return fmt.Errorf("unexpected extra arguments %v — flags must come before positional arguments", flagSet.Args())
	}
	return fmt.Errorf("db:reset is disabled in release builds")
}
