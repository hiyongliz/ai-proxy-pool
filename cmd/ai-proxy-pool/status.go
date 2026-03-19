package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/hiyongliz/ai-proxy-pool/internal/buildinfo"
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

	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh every second until q or Ctrl+C")

	resetCmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset status statistics on the proxy server",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatusReset(cmd.OutOrStdout())
		},
	}
	cmd.AddCommand(resetCmd)

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
		renderStatusDashboard(out, payload.Server, payload.Providers, cfg, buildinfo.Version, false, statusDashboardWidth(out))
		return nil
	}

	fmt.Fprint(out, "\033[?25l") // hide cursor
	defer fmt.Fprint(out, "\033[?25h")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	quitCh := make(chan struct{}, 1)
	if file, ok := out.(*os.File); ok && term.IsTerminal(file.Fd()) {
		state, err := term.MakeRaw(file.Fd())
		if err == nil {
			defer term.Restore(file.Fd(), state)
			out = &crlfWriter{w: out}
			go watchQuitKey(file, quitCh)
		}
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		payload, err := fetchStatusPayload(client, url)
		if err != nil {
			fmt.Fprintf(out, "\r\033[Kstatus fetch failed: %v\n", err)
		} else {
			fmt.Fprint(out, "\033[H\033[2J")
			renderStatusDashboard(out, payload.Server, payload.Providers, cfg, buildinfo.Version, true, statusDashboardWidth(out))
		}

		select {
		case <-sigCh:
			return nil
		case <-quitCh:
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

func watchQuitKey(in io.Reader, quitCh chan<- struct{}) {
	buf := make([]byte, 1)
	for {
		_, err := in.Read(buf)
		if err != nil {
			return
		}
		if shouldQuitWatchKey(buf[0]) {
			select {
			case quitCh <- struct{}{}:
			default:
			}
			return
		}
	}
}

func shouldQuitWatchKey(key byte) bool {
	return key == 'q' || key == 'Q'
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

func statusDashboardWidth(out io.Writer) int {
	if file, ok := out.(*os.File); ok && term.IsTerminal(file.Fd()) {
		width, _, err := term.GetSize(file.Fd())
		if err == nil && width > 0 {
			return width
		}
	}
	return 120
}

func runStatusReset(out io.Writer) error {
	cfgPath := resolveConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config failed: %w", err)
	}

	addr := normalizeStatusAddr(cfg.Server.ListenAddr)
	url := fmt.Sprintf("http://%s/api/internal/status/reset", addr)
	client := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("build status reset request failed: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not connect to proxy daemon (is it running?): %w (url=%s)", err, url)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status reset failed: %s", resp.Status)
	}

	fmt.Fprintln(out, "Status metrics reset successfully.")
	return nil
}

// crlfWriter wraps a writer and converts lone \n to \r\n,
// which is required when the terminal is in raw mode.
type crlfWriter struct {
	w io.Writer
}

func (c *crlfWriter) Write(p []byte) (int, error) {
	var buf bytes.Buffer
	for _, b := range p {
		if b == '\n' {
			buf.WriteByte('\r')
		}
		buf.WriteByte(b)
	}
	n, err := c.w.Write(buf.Bytes())
	if n > len(p) {
		n = len(p)
	}
	return n, err
}
