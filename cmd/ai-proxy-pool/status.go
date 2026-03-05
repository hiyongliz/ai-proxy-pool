package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/proxy"
	"github.com/spf13/cobra"
)

type statusPayload struct {
	Server    map[string]any                    `json:"server"`
	Providers map[string]proxy.ProviderStatView `json:"providers"`
}

var statusCmd = newStatusCommand()

func init() {
	rootCmd.AddCommand(statusCmd)
}

func newStatusCommand() *cobra.Command {
	var watch bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show proxy server and providers health status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd.OutOrStdout(), watch)
		},
	}

	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh every second until Ctrl+C")
	return cmd
}

func runStatus(out io.Writer, watch bool) error {
	cfgPath := resolveConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config failed: %w", err)
	}

	addr := normalizeStatusAddr(cfg.Server.ListenAddr)
	url := fmt.Sprintf("http://%s/api/internal/status", addr)
	client := &http.Client{Timeout: 5 * time.Second}

	if !watch {
		payload, err := fetchStatusPayload(client, url)
		if err != nil {
			return err
		}
		renderStatusDashboard(out, payload.Server, payload.Providers, cfg)
		return nil
	}

	fmt.Fprint(out, "\033[?25l") // hide cursor
	defer fmt.Fprint(out, "\033[?25h")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		payload, err := fetchStatusPayload(client, url)
		if err != nil {
			fmt.Fprintf(out, "\r\033[Kstatus fetch failed: %v\n", err)
		} else {
			// Clear screen and move cursor to top-left (top-like UI).
			fmt.Fprint(out, "\033[H\033[2J")
			renderStatusDashboard(out, payload.Server, payload.Providers, cfg)
		}

		select {
		case <-sigCh:
			return nil
		case <-ticker.C:
		}
	}
}

func fetchStatusPayload(client *http.Client, url string) (statusPayload, error) {
	resp, err := client.Get(url)
	if err != nil {
		return statusPayload{}, fmt.Errorf("could not connect to proxy daemon (is it running?): %w (url=%s)", err, url)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return statusPayload{}, fmt.Errorf("unexpected status from daemon: %s", resp.Status)
	}

	var payload statusPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return statusPayload{}, fmt.Errorf("decode status response failed: %w", err)
	}
	return payload, nil
}

func normalizeStatusAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "127.0.0.1:8080"
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

func renderStatusDashboard(out io.Writer, server map[string]any, stats map[string]proxy.ProviderStatView, cfg config.Config) {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42")).MarginBottom(1)
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).PaddingRight(2)
	cellStyle := lipgloss.NewStyle().PaddingRight(2)
	errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).PaddingRight(2)
	okStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).PaddingRight(2)
	warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("220")).PaddingRight(2)
	mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).PaddingRight(2)

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

	colWidths := []int{20, 15, 12, 22, 18, 20}
	headers := []string{"Provider", "Status", "Active", "Requests (Tot/Err)", "Avg Latency", "Throughput/Tokens"}
	for i, h := range headers {
		fmt.Fprint(out, padRight(headerStyle.Render(h), colWidths[i]))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, mutedStyle.Render("-----------------------------------------------------------------------------------------------------------"))

	var pNames []string
	for _, p := range cfg.Providers {
		pNames = append(pNames, p.Name)
	}
	sort.Strings(pNames)

	for _, name := range pNames {
		isEnabled := false
		for _, p := range cfg.Providers {
			if p.Name == name {
				isEnabled = p.EnabledOrDefault()
				break
			}
		}

		nameCell := cellStyle.Render(name)
		statusCell := mutedStyle.Render("Disabled")
		if isEnabled {
			statusCell = okStyle.Render("Online")
		}

		stat, ok := stats[name]
		if !ok || !isEnabled {
			fmt.Fprintf(out, "%s%s%s%s%s%s\n",
				padRight(nameCell, colWidths[0]),
				padRight(statusCell, colWidths[1]),
				padRight(mutedStyle.Render("-"), colWidths[2]),
				padRight(mutedStyle.Render("-"), colWidths[3]),
				padRight(mutedStyle.Render("-"), colWidths[4]),
				padRight(mutedStyle.Render("-"), colWidths[5]),
			)
			continue
		}

		activeCell := cellStyle.Render(fmt.Sprintf("%d", stat.ActiveConnections))

		reqFmt := fmt.Sprintf("%d / %d", stat.TotalRequests, stat.ErrorRequests)
		reqCell := cellStyle.Render(reqFmt)
		if stat.ErrorRequests > 0 {
			reqCell = errStyle.Render(reqFmt)
		}

		latencyStr := fmt.Sprintf("%dms", stat.AvgDurationMs)
		latCell := cellStyle.Render(latencyStr)
		if stat.AvgDurationMs > 3000 {
			latCell = warnStyle.Render(latencyStr)
		} else if stat.AvgDurationMs == 0 {
			latCell = mutedStyle.Render("-")
		}

		mb := float64(stat.TotalBytes) / 1024 / 1024
		tpCell := cellStyle.Render(fmt.Sprintf("%.2fMB ↑%d/↓%d", mb, stat.PromptTokens, stat.CompletionTokens))

		fmt.Fprintf(out, "%s%s%s%s%s%s\n",
			padRight(nameCell, colWidths[0]),
			padRight(statusCell, colWidths[1]),
			padRight(activeCell, colWidths[2]),
			padRight(reqCell, colWidths[3]),
			padRight(latCell, colWidths[4]),
			padRight(tpCell, colWidths[5]),
		)
	}
	fmt.Fprintln(out)
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

func padRight(s string, width int) string {
	padding := width - lipgloss.Width(s)
	if padding < 0 {
		return s
	}
	return s + fmt.Sprintf("%*s", padding, "")
}
