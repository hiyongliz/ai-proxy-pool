package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon",
	Run: func(cmd *cobra.Command, args []string) {
		pid, err := stopDaemonAndWait()
		switch {
		case err == nil:
			fmt.Fprintf(os.Stderr, "daemon stopped, pid=%d\n", pid)
			os.Exit(0)
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintln(os.Stderr, "daemon not running")
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "failed to stop daemon: %v\n", err)
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(stopCmd)
}
