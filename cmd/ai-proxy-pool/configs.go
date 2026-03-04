package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"
)

// switchConfigInteractive lets users select a config and makes it active.
func switchConfigInteractive() {
	configsDir := resolveConfigsDir()
	activeConfig := resolveActiveConfigForDisplay(resolveConfigPath())

	files, err := listConfigFiles(configsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			files = nil
		} else {
			fmt.Fprintf(os.Stderr, "failed to list config files: %v\n", err)
			os.Exit(1)
		}
	}
	files = appendLegacyConfigIfExists(files, filepath.Join(defaultDir(), "config.yaml"))
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no config files found in %s (and default config.yaml)\n", configsDir)
		os.Exit(1)
	}
	if shouldInferActiveConfigFromContent() {
		activeConfig = inferActiveConfigFromCandidates(activeConfig, files)
	}

	model := newSwitchConfigModel(files, activeConfig, resolveConfigPath())
	finalModel, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to run config switch TUI: %v\n", err)
		os.Exit(1)
	}

	if _, ok := finalModel.(switchConfigModel); !ok {
		fmt.Fprintln(os.Stderr, "unexpected TUI result type")
		os.Exit(1)
	}
	os.Exit(0)
}

type switchConfigModel struct {
	files            []string
	summaries        map[string]configSummary
	activeConfig     string
	activeTargetPath string
	cursor           int
	cancelled        bool
	status           string
	statusIsError    bool
}

// configSummary holds lightweight metadata for TUI display.
type configSummary struct {
	ProviderCount  int
	EnabledCount   int
	ListenAddr     string
	Strategy       string
	DefaultProvider string
	ParseError     string
}

var (
	tuiCursorStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("45"))
	tuiNameStyle        = lipgloss.NewStyle().Bold(true)
	tuiActiveMarkStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	tuiKeyStyle         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("222"))
	tuiSummaryStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	tuiSummaryDotStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	tuiStatusOKStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	tuiStatusErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func newSwitchConfigModel(files []string, activeConfig, activeTargetPath string) switchConfigModel {
	cursor := 0
	for i, file := range files {
		if normalizedPath(file) == activeConfig {
			cursor = i
			break
		}
	}
	summaries := make(map[string]configSummary, len(files))
	for _, file := range files {
		summaries[normalizedPath(file)] = parseConfigSummary(file)
	}
	return switchConfigModel{
		files:            files,
		summaries:        summaries,
		activeConfig:     activeConfig,
		activeTargetPath: activeTargetPath,
		cursor:           cursor,
	}
}

func (m switchConfigModel) Init() tea.Cmd {
	return nil
}

func (m switchConfigModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.files)-1 {
				m.cursor++
			}
		case "enter":
			if len(m.files) == 0 {
				return m, nil
			}
			selected := m.files[m.cursor]
			if normalizedPath(selected) == normalizedPath(m.activeConfig) {
				m.status = fmt.Sprintf("already active: %s", selected)
				m.statusIsError = false
				return m, nil
			}
			if err := activateConfig(selected, m.activeTargetPath); err != nil {
				m.status = fmt.Sprintf("failed to activate config: %v", err)
				m.statusIsError = true
				return m, nil
			}
			if err := writeSelectedConfigPath(selected); err != nil {
				m.status = fmt.Sprintf("persist selection failed: %v", err)
				m.statusIsError = true
			}
			m.activeConfig = normalizedPath(selected)
			signalDaemonReload()
			return m, nil
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m switchConfigModel) View() string {
	if len(m.files) == 0 {
		return "No config files found.\n"
	}

	var b strings.Builder
	b.WriteString("\n")

	// Calculate max name width for alignment
	maxNameWidth := 0
	for _, file := range m.files {
		name := filepath.Base(file)
		if len(name) > maxNameWidth {
			maxNameWidth = len(name)
		}
	}

	dot := tuiSummaryDotStyle.Render(" · ")
	for i, file := range m.files {
		isActive := normalizedPath(file) == normalizedPath(m.activeConfig)
		isCursor := i == m.cursor

		// Cursor indicator
		cursor := "  "
		if isCursor {
			cursor = tuiCursorStyle.Render(">") + " "
		}

		// Number
		num := fmt.Sprintf("%d.", i+1)

		// Name with padding, green highlight for active
		name := filepath.Base(file)
		paddedName := name + strings.Repeat(" ", maxNameWidth-len(name))
		if isActive {
			paddedName = tuiActiveMarkStyle.Render(paddedName)
		} else if isCursor {
			paddedName = tuiNameStyle.Render(paddedName)
		}

		// Build summary
		summary := m.summaries[normalizedPath(file)]
		var summaryParts []string
		if summary.ParseError != "" {
			summaryParts = append(summaryParts, tuiStatusErrorStyle.Render("parse error"))
		} else {
			if summary.EnabledCount > 0 {
				summaryParts = append(summaryParts, fmt.Sprintf("%d/%d providers", summary.EnabledCount, summary.ProviderCount))
			} else if summary.ProviderCount > 0 {
				summaryParts = append(summaryParts, fmt.Sprintf("%d providers", summary.ProviderCount))
			}
			if summary.ListenAddr != "" {
				summaryParts = append(summaryParts, summary.ListenAddr)
			}
			if summary.Strategy != "" {
				summaryParts = append(summaryParts, summary.Strategy)
			}
		}

		summaryText := ""
		if len(summaryParts) > 0 {
			summaryText = tuiSummaryStyle.Render(strings.Join(summaryParts, dot))
		}

		line := fmt.Sprintf("%s%s %s   %s", cursor, num, paddedName, summaryText)
		b.WriteString(line + "\n")
	}

	// Status message
	if m.status != "" {
		b.WriteString("\n")
		if m.statusIsError {
			b.WriteString(tuiStatusErrorStyle.Render("Error: "+m.status) + "\n")
		} else {
			b.WriteString(tuiStatusOKStyle.Render(m.status) + "\n")
		}
	}

	// Help bar
	b.WriteString("\n")
	sep := tuiSummaryDotStyle.Render(" | ")
	b.WriteString(
		tuiKeyStyle.Render("↑↓") + sep +
			tuiKeyStyle.Render("Enter") + " Select" + sep +
			tuiKeyStyle.Render("q") + " Quit\n",
	)

	return b.String()
}

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
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}

	sort.Strings(files)
	return files, nil
}

func appendLegacyConfigIfExists(files []string, configPath string) []string {
	if configPath == "" {
		return files
	}
	info, err := os.Stat(configPath)
	if err != nil || info.IsDir() {
		return files
	}

	target := normalizedPath(configPath)
	for _, file := range files {
		if normalizedPath(file) == target {
			return files
		}
	}

	files = append(files, configPath)
	sort.Strings(files)
	return files
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

	tmp, err := os.CreateTemp(targetDir, ".config-switch-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("replace active config: %w", err)
	}
	return nil
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

// signalDaemonReload sends SIGHUP to the running daemon to trigger config reload.
func signalDaemonReload() {
	data, err := os.ReadFile(pidPath())
	if err != nil {
		return // daemon not running
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGHUP)
}
