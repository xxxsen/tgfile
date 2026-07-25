package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

var errCommandUsage = errors.New("invalid command usage")

type exitError struct {
	err error
}

func (e *exitError) Error() string {
	return e.err.Error()
}

func (e *exitError) Unwrap() error {
	return e.err
}

func commandError(err error) error {
	return &exitError{err: err}
}

func usageError(message string) error {
	return commandError(fmt.Errorf("%w: %s", errCommandUsage, message))
}

func noPositionalArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return usageError(fmt.Sprintf("unexpected positional arguments: %v", args))
	}
	return nil
}

func newRootCommand(ctx context.Context) *cobra.Command {
	command := &cobra.Command{
		Use:   "tgfile",
		Short: "File service and offline maintenance tools",
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(*cobra.Command, []string) error {
			return usageError("a subcommand is required")
		},
	}
	command.SetContext(ctx)
	command.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return commandError(fmt.Errorf("parse command flags: %w", err))
	})
	command.AddCommand(
		newServeCommand(ctx),
		newAuditCommand(ctx),
		newMigrateDefaultPrefixCommand(ctx),
		newCheckKeyCommand(),
	)
	return command
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	command := newRootCommand(ctx)
	command.SetArgs(args)
	command.SetOut(stdout)
	command.SetErr(stderr)
	if err := command.Execute(); err != nil {
		fmt.Fprintln(stderr, err)
		var typedError *exitError
		if errors.As(err, &typedError) {
			return 2
		}
		return 1
	}
	return 0
}
