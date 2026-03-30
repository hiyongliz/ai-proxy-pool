package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

func listConfigFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		// 隐藏最终激活生成的 ~/.ai_proxy_pool/config.yaml 文件
		if filepath.Join(dir, name) == filepath.Join(defaultDir(), "config.yaml") {
			continue
		}

		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}

	sort.Strings(files)
	return files, nil
}

func activateConfig(sourcePath, targetPath string) error {
	if normalizedPath(sourcePath) == normalizedPath(targetPath) {
		return nil
	}

	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("read source config: %w", err)
	}

	targetDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}

	if err := os.WriteFile(targetPath, data, 0o644); err != nil {
		return fmt.Errorf("write target config: %w", err)
	}

	return nil
}

func sameConfigContent(pathA, pathB string) bool {
	dataA, err := os.ReadFile(pathA)
	if err != nil {
		return false
	}
	dataB, err := os.ReadFile(pathB)
	if err != nil {
		return false
	}
	return bytes.Equal(dataA, dataB)
}

// parseConfigSummary reads a config YAML file and extracts display metadata.
func parseConfigSummary(path string) configSummary {
	data, err := os.ReadFile(path)
	if err != nil {
		return configSummary{ParseError: err.Error()}
	}

	var raw struct {
		Server struct {
			ListenAddr string `yaml:"listen_addr"`
		} `yaml:"server"`
		Router struct {
			Strategy        string `yaml:"strategy"`
			DefaultProvider string `yaml:"default_provider"`
		} `yaml:"router"`
		Providers []struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return configSummary{ParseError: err.Error()}
	}

	total := len(raw.Providers)
	enabled := 0
	for _, p := range raw.Providers {
		if p.Enabled == nil || *p.Enabled {
			enabled++
		}
	}

	return configSummary{
		ProviderCount:   total,
		EnabledCount:    enabled,
		ListenAddr:      raw.Server.ListenAddr,
		Strategy:        raw.Router.Strategy,
		DefaultProvider: raw.Router.DefaultProvider,
	}
}

// signalDaemonReloadResult contains the result of attempting to reload the daemon.
type signalDaemonReloadResult struct {
	daemonRunning bool
	signalSent    bool
	pid           int
	err           error
}

// signalDaemonReload sends SIGHUP to the running daemon to trigger config reload.
func signalDaemonReload() signalDaemonReloadResult {
	result := signalDaemonReloadResult{}

	record, err := readPIDRecord(pidPath())
	if err != nil {
		return result // daemon not running
	}
	result.daemonRunning = true

	pid := record.PID
	matches, matchErr := managedProcessMatches(record)
	if matchErr != nil {
		if isProcessNotRunningError(matchErr) {
			result.daemonRunning = false
			return result
		}
		result.err = fmt.Errorf("inspect process: %w", matchErr)
		return result
	}
	if !matches {
		result.err = fmt.Errorf("pid file points to a different process, pid=%d", pid)
		return result
	}
	result.pid = pid

	proc, err := os.FindProcess(pid)
	if err != nil {
		result.err = fmt.Errorf("find process: %w", err)
		return result
	}

	// Check if process is actually running
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		result.daemonRunning = false
		return result
	}

	if err := proc.Signal(syscall.SIGHUP); err != nil {
		result.err = fmt.Errorf("send SIGHUP: %w", err)
		return result
	}
	result.signalSent = true
	return result
}
