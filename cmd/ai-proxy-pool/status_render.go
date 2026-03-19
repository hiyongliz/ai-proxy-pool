package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/proxy"
)

type statusRow struct {
	Name       string
	Status     string
	Active     string
	Req        string
	Latency    string
	Throughput string
	Tokens     string
}

func renderStatusDashboard(out io.Writer, server map[string]any, stats map[string]proxy.ProviderStatView, cfg config.Config, watch bool, width int) {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42")).MarginBottom(1)
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	cellStyle := lipgloss.NewStyle()
	mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	uptimeSeconds := asInt(server["uptime_seconds"])
	uptime := time.Duration(uptimeSeconds) * time.Second
	strategy := asString(server["strategy"])
	if strategy == "" {
		strategy = "unknown"
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, titleStyle.Render("AI Proxy Pool Status"))
	fmt.Fprintf(out, "  Uptime:   %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Render(uptime.String()))
	fmt.Fprintf(out, "  Strategy: %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Render(strategy))
	fmt.Fprintln(out)

	rows := buildStatusRows(stats, cfg.Providers)
	switch {
	case width < 70:
		renderStatusBlockLayout(out, rows, cellStyle, mutedStyle)
	case width < 100:
		renderStatusCompactTable(out, rows, width, headerStyle, mutedStyle)
	default:
		renderStatusFullTable(out, rows, width, headerStyle, mutedStyle)
	}

	fmt.Fprintln(out)
	if watch {
		fmt.Fprintln(out, mutedStyle.Render("Press q to quit"))
	}
}

func buildStatusRows(stats map[string]proxy.ProviderStatView, providers []config.ProviderConfig) []statusRow {
	var pNames []string
	for _, p := range providers {
		pNames = append(pNames, p.Name)
	}
	sort.Strings(pNames)

	rows := make([]statusRow, 0, len(pNames))
	for _, name := range pNames {
		isEnabled := false
		for _, p := range providers {
			if p.Name == name {
				isEnabled = p.EnabledOrDefault()
				break
			}
		}

		stat, ok := stats[name]
		row := statusRow{
			Name:    name,
			Status:  "Disabled",
			Active:  "-",
			Req:     "-",
			Latency: "-",
			Tokens:  "-",
		}
		if isEnabled {
			row.Status = "Online"
			if stat.CircuitOpenUntil > time.Now().Unix() {
				row.Status = "Melted"
			}
		}
		if !ok || !isEnabled {
			rows = append(rows, row)
			continue
		}

		row.Active = fmt.Sprintf("%d", stat.ActiveConnections)
		row.Req = fmt.Sprintf("%s / %s", formatCount(stat.TotalRequests), formatCount(stat.ErrorRequests))
		if stat.AvgDurationMs > 0 {
			row.Latency = fmt.Sprintf("%dms", stat.AvgDurationMs)
		}
		row.Throughput = fmt.Sprintf("%.2fMB", float64(stat.TotalBytes)/1024/1024)
		row.Tokens = fmt.Sprintf("↑%s / ↓%s", formatCount(stat.PromptTokens), formatCount(stat.CompletionTokens))
		rows = append(rows, row)
	}
	return rows
}

func renderStatusFullTable(out io.Writer, rows []statusRow, width int, headerStyle, mutedStyle lipgloss.Style) {
	colWidths := statusColumnWidths(width)
	headers := []string{"Provider", "Stat", "Act", "Req/Err", "Latency", "Thru/Tok"}
	tableWidth := sumInts(colWidths)

	for i, h := range headers {
		fmt.Fprint(out, renderTableCell(headerStyle.Render(h), colWidths[i]))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, mutedStyle.Render(strings.Repeat("-", tableWidth)))

	for _, row := range rows {
		throughputTokens := row.Throughput
		if throughputTokens == "-" {
			throughputTokens = "-"
		} else {
			throughputTokens = row.Throughput + " " + row.Tokens
		}
		fmt.Fprintf(out, "%s%s%s%s%s%s\n",
			renderTableCell(row.Name, colWidths[0]),
			renderTableCell(row.Status, colWidths[1]),
			renderTableCell(row.Active, colWidths[2]),
			renderTableCell(row.Req, colWidths[3]),
			renderTableCell(row.Latency, colWidths[4]),
			renderTableCell(throughputTokens, colWidths[5]),
		)
	}
}

func renderStatusCompactTable(out io.Writer, rows []statusRow, width int, headerStyle, mutedStyle lipgloss.Style) {
	colWidths := statusCompactColumnWidths(width)
	headers := []string{"Provider", "Stat", "Req", "Lat", "Tokens"}
	tableWidth := sumInts(colWidths)

	for i, h := range headers {
		fmt.Fprint(out, renderTableCell(headerStyle.Render(h), colWidths[i]))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, mutedStyle.Render(strings.Repeat("-", tableWidth)))

	for _, row := range rows {
		fmt.Fprintf(out, "%s%s%s%s%s\n",
			renderTableCell(row.Name, colWidths[0]),
			renderTableCell(row.Status, colWidths[1]),
			renderTableCell(row.Req, colWidths[2]),
			renderTableCell(row.Latency, colWidths[3]),
			renderTableCell(row.Tokens, colWidths[4]),
		)
	}
}

func renderStatusBlockLayout(out io.Writer, rows []statusRow, cellStyle, mutedStyle lipgloss.Style) {
	for i, row := range rows {
		fmt.Fprintf(out, "Provider: %s\n", cellStyle.Render(row.Name))
		fmt.Fprintf(out, "Status: %s\n", cellStyle.Render(row.Status))
		fmt.Fprintf(out, "Active: %s\n", cellStyle.Render(row.Active))
		fmt.Fprintf(out, "Requests: %s\n", cellStyle.Render(row.Req))
		fmt.Fprintf(out, "Latency: %s\n", cellStyle.Render(row.Latency))
		fmt.Fprintf(out, "Tokens: %s\n", cellStyle.Render(row.Tokens))
		if i < len(rows)-1 {
			fmt.Fprintln(out, mutedStyle.Render(strings.Repeat("-", 24)))
		}
	}
}

func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	default:
		return 0
	}
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return ""
	}
}

func statusColumnWidths(totalWidth int) []int {
	base := []int{18, 10, 8, 18, 12, 20}
	if totalWidth <= 0 {
		return append([]int(nil), base...)
	}

	minWidths := []int{12, 8, 8, 14, 10, 16}
	widths := append([]int(nil), base...)
	current := sumInts(widths)
	if current <= totalWidth {
		widths[len(widths)-1] += totalWidth - current
		return widths
	}

	for current > totalWidth {
		changed := false
		for index := len(widths) - 1; index >= 0 && current > totalWidth; index-- {
			if widths[index] > minWidths[index] {
				widths[index]--
				current--
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	return widths
}

func statusCompactColumnWidths(totalWidth int) []int {
	base := []int{18, 10, 12, 10, 20}
	if totalWidth <= 0 {
		return append([]int(nil), base...)
	}

	minWidths := []int{12, 8, 9, 8, 12}
	widths := append([]int(nil), base...)
	current := sumInts(widths)
	if current <= totalWidth {
		widths[len(widths)-1] += totalWidth - current
		return widths
	}

	for current > totalWidth {
		changed := false
		for index := len(widths) - 1; index >= 0 && current > totalWidth; index-- {
			if widths[index] > minWidths[index] {
				widths[index]--
				current--
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	return widths
}

func sumInts(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

// formatCount formats a number with k/m/b suffixes for readability.
func formatCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fb", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func renderTableCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	trimmed := lipgloss.NewStyle().MaxWidth(width).Render(s)
	return lipgloss.NewStyle().Width(width).Render(trimmed)
}
