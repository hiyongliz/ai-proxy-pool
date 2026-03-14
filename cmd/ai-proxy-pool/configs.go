package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// switchConfigInteractive lets users select a config and makes it active.
func switchConfigInteractive() error {
	configsDir := resolveConfigsDir()
	activeConfig := resolveActiveConfigForDisplay(resolveConfigPath())

	files, err := listConfigFiles(configsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			files = nil
		} else {
			return fmt.Errorf("failed to list config files: %w", err)
		}
	}
	files = appendLegacyConfigIfExists(files, filepath.Join(defaultDir(), "config.yaml"))
	if len(files) == 0 {
		return fmt.Errorf("no config files found in %s (and default config.yaml)", configsDir)
	}
	if shouldInferActiveConfigFromContent() {
		activeConfig = inferActiveConfigFromCandidates(activeConfig, files)
	}

	model := newSwitchConfigModel(files, activeConfig, resolveConfigPath())
	finalModel, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	if err != nil {
		return fmt.Errorf("failed to run config switch TUI: %w", err)
	}

	if _, ok := finalModel.(switchConfigModel); !ok {
		return fmt.Errorf("unexpected TUI result type")
	}
	return nil
}

func appendLegacyConfigIfExists(files []string, configPath string) []string {
	legacy := normalizedPath(configPath)
	for _, file := range files {
		if normalizedPath(file) == legacy {
			return files
		}
	}
	if _, err := os.Stat(configPath); err != nil {
		return files
	}
	// Keep legacy config at the front so users can still choose it explicitly.
	files = append([]string{configPath}, files...)
	return files
}

func selectedConfigPathFile() string {
	return filepath.Join(defaultDir(), "active_config_path")
}

func writeSelectedConfigPath(path string) error {
	stateFile := selectedConfigPathFile()
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	return os.WriteFile(stateFile, []byte(normalizedPath(path)+"\n"), 0o644)
}

func readSelectedConfigPath() (string, error) {
	data, err := os.ReadFile(selectedConfigPathFile())
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(data))
	if path == "" {
		return "", os.ErrNotExist
	}
	return normalizedPath(path), nil
}

func resolveActiveConfigForDisplay(activeConfig string) string {
	active := normalizedPath(activeConfig)
	selected, err := readSelectedConfigPath()
	if err != nil {
		return active
	}
	if !sameConfigContent(active, selected) {
		return active
	}
	return selected
}

func inferActiveConfigFromCandidates(activeConfig string, candidates []string) string {
	active := normalizedPath(activeConfig)
	matches := make([]string, 0, 1)

	for _, candidate := range candidates {
		normalizedCandidate := normalizedPath(candidate)
		if normalizedCandidate == active {
			continue
		}
		if sameConfigContent(active, normalizedCandidate) {
			matches = append(matches, normalizedCandidate)
		}
	}

	if len(matches) == 1 {
		return matches[0]
	}
	return active
}

func shouldInferActiveConfigFromContent() bool {
	_, err := readSelectedConfigPath()
	return err != nil
}

func normalizedPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}
