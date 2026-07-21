//go:build release

package main

import (
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
)

// TestRunDBResetReleaseParsesHelpAsFlagErrHelp guards against the release
// stub silently ignoring its own args: "db:reset --help" must surface
// flag.ErrHelp (which dispatch() in commands.go treats as exit code 0,
// same as every other subcommand's --help), not the plain "disabled in
// release builds" error with exit code 1.
func TestRunDBResetReleaseParsesHelpAsFlagErrHelp(t *testing.T) {
	err := runDBReset(context.Background(), []string{"--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp for --help, got: %v", err)
	}
}

// TestRunDBResetReleaseRejectsExtraArgs asserts the specific "unexpected
// extra arguments" error, not just any non-nil error — an old "ignore all
// args, always return the disabled error" implementation would also
// produce a non-nil error here, so a bare err == nil check doesn't
// distinguish "args were actually parsed and rejected" from "args were
// never looked at".
func TestRunDBResetReleaseRejectsExtraArgs(t *testing.T) {
	err := runDBReset(context.Background(), []string{"unexpected-arg"})
	if err == nil {
		t.Fatalf("expected error for unexpected positional argument")
	}
	if !strings.Contains(err.Error(), "unexpected extra arguments") {
		t.Fatalf("expected 'unexpected extra arguments' in error (proving args were actually parsed), got: %v", err)
	}
}

// TestRunDBResetReleaseRejectsUnknownFlag guards the other half of "args
// are actually parsed": an unrecognized flag must surface flag's own
// "flag provided but not defined" parse error specifically — an old
// "ignore all args, always return the disabled error" implementation would
// also produce a non-nil, non-ErrHelp error here (the disabled-in-release
// error itself), so checking only err != nil / !errors.Is(ErrHelp) doesn't
// distinguish "the flag was actually rejected by parsing" from "args were
// never looked at at all".
func TestRunDBResetReleaseRejectsUnknownFlag(t *testing.T) {
	err := runDBReset(context.Background(), []string{"--not-a-real-flag"})
	if err == nil {
		t.Fatalf("expected error for an unrecognized flag")
	}
	if errors.Is(err, flag.ErrHelp) {
		t.Fatalf("unknown flag must not be treated as --help")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("expected flag package's own parse error (proving the flag was actually parsed, not ignored), got: %v", err)
	}
	if strings.Contains(err.Error(), "disabled in release builds") {
		t.Fatalf("unknown flag must be rejected by parsing, not fall through to the disabled-in-release message: %v", err)
	}
}

// TestRunDBResetReleaseDisabledWithValidArgs asserts the specific disabled
// message once args are known to be well-formed — distinguishing "reached
// the disabled-in-release check" from an unrelated parse failure.
func TestRunDBResetReleaseDisabledWithValidArgs(t *testing.T) {
	err := runDBReset(context.Background(), []string{"--yes"})
	if err == nil {
		t.Fatalf("expected db:reset to remain disabled in release builds")
	}
	if !strings.Contains(err.Error(), "disabled in release builds") {
		t.Fatalf("expected 'disabled in release builds' in error, got: %v", err)
	}
}
