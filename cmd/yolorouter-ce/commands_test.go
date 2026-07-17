package main

import (
	"context"
	"errors"
	"testing"
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
	// initializing any resources (design doc §14 criterion 6), even though
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
	// 注册一个必定报错的假命令，验证 dispatch 把 Run 的 error 正确传播
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
