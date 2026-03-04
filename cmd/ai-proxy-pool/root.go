package main

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var (
	flagConfig string
	flagLog    string
)

var rootCmd = &cobra.Command{
	Use:   "appool",
	Short: "AI proxy pool - multi-provider intelligent proxy service",
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "path to config file")
	rootCmd.PersistentFlags().StringVarP(&flagLog, "log", "l", "", "log file path (default: ~/.ai_proxy_pool/ai-proxy-pool.log)")
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func defaultDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".ai_proxy_pool")
	}
	return "."
}

func resolveConfigPath() string {
	if flagConfig != "" {
		return flagConfig
	}
	return filepath.Join(defaultDir(), "config.yaml")
}

func resolveLogPath() string {
	if flagLog != "" {
		return flagLog
	}
	return filepath.Join(defaultDir(), "ai-proxy-pool.log")
}

func resolveConfigsDir() string {
	return filepath.Join(defaultDir(), "configs")
}

func pidPath() string {
	return filepath.Join(defaultDir(), "ai-proxy-pool.pid")
}
