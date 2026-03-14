package main

import (
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Interactive switch active config",
	RunE: func(cmd *cobra.Command, args []string) error {
		return switchConfigInteractive()
	},
}

func init() {
	rootCmd.AddCommand(configCmd)
}
