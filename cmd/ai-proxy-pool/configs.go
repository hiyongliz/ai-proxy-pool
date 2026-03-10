package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	ProviderCount   int
	EnabledCount    int
	ListenAddr      string
	Strategy        string
	DefaultProvider string
	ParseError      string
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
				return m, nil
			}
			m.activeConfig = normalizedPath(selected)
			reloadResult := signalDaemonReload()
			if reloadResult.signalSent {
				m.status = fmt.Sprintf("activated: %s (daemon reloaded, pid=%d)", filepath.Base(selected), reloadResult.pid)
				m.statusIsError = false
			} else if reloadResult.daemonRunning {
				m.status = fmt.Sprintf("activated: %s (daemon reload failed: %v)", filepath.Base(selected), reloadResult.err)
				m.statusIsError = true
			} else {
				m.status = fmt.Sprintf("activated: %s (daemon not running)", filepath.Base(selected))
				m.statusIsError = false
			}
			return m, nil
		case "e", "E":
			if len(m.files) == 0 {
				return m, nil
			}
			selected := m.files[m.cursor]
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vim"
			}
			cmd := exec.Command(editor, selected)
			return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
				if err != nil {
					return editorErrorMsg{err}
				}
				return editorFinishedMsg{selected}
			})
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	case editorErrorMsg:
		m.status = fmt.Sprintf("Error opening editor: %v", msg.err)
		m.statusIsError = true
		return m, nil
	case editorFinishedMsg:
		// Reload summaries when returning from editor
		m.summaries[normalizedPath(msg.file)] = parseConfigSummary(msg.file)

		// If the edited file is the currently active config, activate it and reload daemon
		if normalizedPath(msg.file) == normalizedPath(m.activeConfig) {
			if err := activateConfig(msg.file, m.activeTargetPath); err != nil {
				m.status = fmt.Sprintf("failed to activate edited config: %v", err)
				m.statusIsError = true
				return m, nil
			}
			reloadResult := signalDaemonReload()
			if reloadResult.signalSent {
				m.status = fmt.Sprintf("config saved, daemon reloaded (pid=%d)", reloadResult.pid)
				m.statusIsError = false
			} else if reloadResult.daemonRunning {
				m.status = fmt.Sprintf("config saved, daemon reload failed: %v", reloadResult.err)
				m.statusIsError = true
			} else {
				m.status = "config saved (daemon not running)"
				m.statusIsError = false
			}
		} else {
			m.status = fmt.Sprintf("edited: %s (press Enter to activate)", filepath.Base(msg.file))
			m.statusIsError = false
		}
		return m, nil
	}
	return m, nil
}

type editorFinishedMsg struct{ file string }
type editorErrorMsg struct{ err error }

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
			tuiKeyStyle.Render("e") + " Edit" + sep +
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

	// 推荐使用标准的 os.WriteFile 直接对目标文件重写
	// 这包含了 O_WRONLY|O_CREATE|O_TRUNC 以及对应权限设定
	if err := os.WriteFile(targetPath, data, 0o644); err != nil {
		return fmt.Errorf("write target config: %w", err)
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

	data, err := os.ReadFile(pidPath())
	if err != nil {
		return result // daemon not running
	}
	result.daemonRunning = true

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		result.err = fmt.Errorf("invalid pid in file: %w", err)
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
