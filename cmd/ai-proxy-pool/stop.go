package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon",
	Run: func(cmd *cobra.Command, args []string) {
		pid, err := stopDaemonAndWait()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to stop daemon: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "daemon stopped, pid=%d\n", pid)
		os.Exit(0)
	},
}

func init() {
	rootCmd.AddCommand(stopCmd)
}
