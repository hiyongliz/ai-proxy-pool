package main

import (
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Interactive switch active config",
	Run: func(cmd *cobra.Command, args []string) {
		switchConfigInteractive()
	},
}

func init() {
	rootCmd.AddCommand(configCmd)
}
