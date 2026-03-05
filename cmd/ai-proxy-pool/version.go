package main

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
	builtBy   = "unknown"
)

type versionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	BuiltBy   string `json:"built_by"`
	Go        string `json:"go"`
	Platform  string `json:"platform"`
}

func init() {
	rootCmd.AddCommand(newVersionCommand())
}

func newVersionCommand() *cobra.Command {
	var short bool
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			if short && jsonOut {
				return fmt.Errorf("--short and --json cannot be used together")
			}

			info := getVersionInfo()
			out, err := formatVersionOutput(info, short, jsonOut)
			if err != nil {
				return err
			}

			_, err = cmd.OutOrStdout().Write([]byte(out))
			return err
		},
	}

	cmd.Flags().BoolVar(&short, "short", false, "print version only")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print version info in JSON")

	return cmd
}

func getVersionInfo() versionInfo {
	return versionInfo{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		BuiltBy:   builtBy,
		Go:        runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

func formatVersionOutput(info versionInfo, short, jsonOut bool) (string, error) {
	if short {
		return info.Version + "\n", nil
	}

	if jsonOut {
		data, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return "", fmt.Errorf("marshal version info: %w", err)
		}
		return string(data) + "\n", nil
	}

	var b strings.Builder
	b.WriteString("appool version\n")
	b.WriteString("  version: " + info.Version + "\n")
	b.WriteString("  commit: " + info.Commit + "\n")
	b.WriteString("  build_date: " + info.BuildDate + "\n")
	b.WriteString("  built_by: " + info.BuiltBy + "\n")
	b.WriteString("  go: " + info.Go + "\n")
	b.WriteString("  platform: " + info.Platform + "\n")
	return b.String(), nil
}
