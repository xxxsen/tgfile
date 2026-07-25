package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xxxsen/tgfile/maintenance"
)

func newMigrateDefaultPrefixCommand(ctx context.Context) *cobra.Command {
	var configFile string
	var direction string
	var dryRun bool
	command := &cobra.Command{
		Use:   "migrate-default-prefix",
		Short: "Inspect or migrate the default upload directory prefix",
		Args:  noPositionalArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			databaseFile, err := maintenance.DatabaseFileFromConfig(configFile)
			if err != nil {
				return fmt.Errorf("read migration config: %w", err)
			}
			result, migrationErr := maintenance.MigrateDefaultPrefix(
				ctx,
				databaseFile,
				direction,
				dryRun,
			)
			if result != nil {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(result); err != nil {
					return fmt.Errorf("encode migration result: %w", err)
				}
			}
			if migrationErr != nil {
				wrapped := fmt.Errorf("migrate default prefix: %w", migrationErr)
				if maintenance.IsPreconditionError(migrationErr) {
					return commandError(wrapped)
				}
				return wrapped
			}
			return nil
		},
	}
	command.Flags().StringVar(&configFile, "config", "./config.json", "config file path")
	command.Flags().StringVar(
		&direction,
		"direction",
		"",
		"migration direction: forward or reverse",
	)
	command.Flags().BoolVar(&dryRun, "dry-run", true, "inspect the migration without changing data")
	return command
}
