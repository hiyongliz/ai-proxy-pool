package main

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	logTimeStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	logInfoStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	logWarnStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	logErrorStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	logAddrStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
	logNumStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	logStatus2xxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	logStatus3xxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
	logStatus4xxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	logStatus5xxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

// colorizeLine highlights fields in a slog text line.
func colorizeLine(line string) string {
	// Level coloring
	switch {
	case strings.Contains(line, "level=ERROR"):
		line = strings.Replace(line, "level=ERROR", "level="+logErrorStyle.Render("ERROR"), 1)
	case strings.Contains(line, "level=WARN"):
		line = strings.Replace(line, "level=WARN", "level="+logWarnStyle.Render("WARN"), 1)
	case strings.Contains(line, "level=INFO"):
		line = strings.Replace(line, "level=INFO", "level="+logInfoStyle.Render("INFO"), 1)
	}

	// Time coloring: time=2026-03-03T17:01:18.985+08:00
	if idx := strings.Index(line, "time="); idx >= 0 {
		end := strings.Index(line[idx:], " ")
		if end > 0 {
			timeVal := line[idx+5 : idx+end]
			line = strings.Replace(line, "time="+timeVal, "time="+logTimeStyle.Render(timeVal), 1)
		}
	}

	// Address coloring: remote_addr, listen, upstream_host
	for _, key := range []string{"remote_addr=", "listen=", "upstream_host="} {
		if idx := strings.Index(line, key); idx >= 0 {
			rest := line[idx+len(key):]
			end := strings.Index(rest, " ")
			if end < 0 {
				end = len(rest)
			}
			val := rest[:end]
			line = strings.Replace(line, key+val, key+logAddrStyle.Render(val), 1)
		}
	}

	// Special handling for HTTP Status code with dynamic coloring
	if idx := strings.Index(line, "status="); idx >= 0 {
		rest := line[idx+7:]
		end := strings.Index(rest, " ")
		if end < 0 {
			end = len(rest)
		}
		val := rest[:end]
		if statusInt, err := strconv.Atoi(val); err == nil {
			var coloredStatus string
			switch {
			case statusInt >= 200 && statusInt < 300:
				coloredStatus = logStatus2xxStyle.Render(val)
			case statusInt >= 300 && statusInt < 400:
				coloredStatus = logStatus3xxStyle.Render(val)
			case statusInt >= 400 && statusInt < 500:
				coloredStatus = logStatus4xxStyle.Render(val)
			case statusInt >= 500 && statusInt < 600:
				coloredStatus = logStatus5xxStyle.Render(val)
			default:
				coloredStatus = logNumStyle.Render(val)
			}
			line = strings.Replace(line, "status="+val, "status="+coloredStatus, 1)
		}
	}

	// Numeric values: duration_ms=, response_bytes=
	for _, key := range []string{"duration_ms=", "response_bytes="} {
		if idx := strings.Index(line, key); idx >= 0 {
			rest := line[idx+len(key):]
			end := strings.Index(rest, " ")
			if end < 0 {
				end = len(rest)
			}
			val := rest[:end]
			line = strings.Replace(line, key+val, key+logNumStyle.Render(val), 1)
		}
	}

	return line
}
