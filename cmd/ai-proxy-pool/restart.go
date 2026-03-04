package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the running daemon",
	Run: func(cmd *cobra.Command, args []string) {
		pid, err := stopDaemonAndWait()
		switch {
		case err == nil:
			fmt.Fprintf(os.Stderr, "daemon stopped, pid=%d\n", pid)
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(os.Stderr, "daemon not running, starting a new daemon\n")
		default:
			fmt.Fprintf(os.Stderr, "failed to restart daemon: %v\n", err)
			os.Exit(1)
		}

		daemonize()
	},
}

func init() {
	rootCmd.AddCommand(restartCmd)
}
