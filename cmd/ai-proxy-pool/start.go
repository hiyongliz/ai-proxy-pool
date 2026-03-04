package main

import (
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the proxy server in the background (daemon)",
	Run: func(cmd *cobra.Command, args []string) {
		daemonize()
	},
}

func init() {
	rootCmd.AddCommand(startCmd)
}
