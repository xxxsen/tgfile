package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xxxsen/tgfile/maintenance"
)

func newAuditCommand(ctx context.Context) *cobra.Command {
	var configFile string
	var outputFile string
	command := &cobra.Command{
		Use:   "audit",
		Short: "Audit a tgfile SQLite database in read-only mode",
		Args:  noPositionalArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if outputFile == "" {
				return usageError("audit requires --output")
			}
			databaseFile, err := maintenance.DatabaseFileFromConfig(configFile)
			if err != nil {
				return fmt.Errorf("read audit config: %w", err)
			}
			report, err := maintenance.Audit(ctx, databaseFile)
			if err != nil {
				return fmt.Errorf("audit database: %w", err)
			}
			if err := maintenance.WriteAuditReport(outputFile, report); err != nil {
				return fmt.Errorf("write audit report: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&configFile, "config", "./config.json", "config file path")
	command.Flags().StringVar(&outputFile, "output", "", "audit report output file")
	return command
}
