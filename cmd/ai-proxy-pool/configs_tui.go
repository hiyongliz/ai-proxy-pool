package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

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

type editorFinishedMsg struct{ file string }
type editorErrorMsg struct{ err error }

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
