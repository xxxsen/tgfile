package main

import (
	"fmt"

	"github.com/spf13/cobra"

	filehandler "github.com/xxxsen/tgfile/server/handler/file"
)

func newCheckKeyCommand() *cobra.Command {
	var key string
	command := &cobra.Command{
		Use:   "check-key",
		Short: "Validate a direct-download file key",
		Args:  noPositionalArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			link, err := filehandler.ExtractLinkFromFileKey(key)
			if err != nil {
				return commandError(fmt.Errorf("validate file key: %w", err))
			}
			fmt.Fprintln(command.OutOrStdout(), link)
			return nil
		},
	}
	command.Flags().StringVar(&key, "key", "", "file key to validate")
	return command
}
