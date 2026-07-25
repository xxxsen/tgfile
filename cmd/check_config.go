package main

import (
	"context"
	"fmt"

	"github.com/xxxsen/tgfile/config"

	"github.com/spf13/cobra"
)

func newCheckConfigCommand(_ context.Context) *cobra.Command {
	var configFile string
	command := &cobra.Command{
		Use:   "check-config",
		Short: "Validate configuration without external side effects",
		Args:  noPositionalArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			serviceConfig, err := config.Parse(configFile)
			if err != nil {
				return fmt.Errorf("parse config: %w", err)
			}
			if err := serviceConfig.Validate(); err != nil {
				return fmt.Errorf("validate config: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&configFile, "config", "./config.json", "config file path")
	return command
}
