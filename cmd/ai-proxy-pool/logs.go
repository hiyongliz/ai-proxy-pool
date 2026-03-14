package main

import (
	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show and follow log output",
	RunE: func(cmd *cobra.Command, args []string) error {
		return showLogs()
	},
}

func init() {
	rootCmd.AddCommand(logsCmd)
}
