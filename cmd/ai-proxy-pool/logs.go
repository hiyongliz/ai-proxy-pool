package main

import (
	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show and follow log output",
	Run: func(cmd *cobra.Command, args []string) {
		showLogs()
	},
}

func init() {
	rootCmd.AddCommand(logsCmd)
}
