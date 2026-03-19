package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/proxy"
)

func TestNormalizeStatusAddr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"", "127.0.0.1:8080"},
		{":8081", "127.0.0.1:8081"},
		{"127.0.0.1:19090", "127.0.0.1:19090"},
	}

	for _, tc := range cases {
		if got := normalizeStatusAddr(tc.in); got != tc.want {
			t.Fatalf("normalizeStatusAddr(%q): got=%q want=%q", tc.in, got, tc.want)
		}
	}
}

func TestRenderStatusDashboardNoPanicOnMissingFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: "https://example.com"},
		},
	}
	renderStatusDashboard(&buf, map[string]any{}, map[string]proxy.ProviderStatView{}, cfg, "1.2.3", false, 120)
	out := buf.String()
	if !strings.Contains(out, "Strategy:") {
		t.Fatalf("unexpected output: %q", out)
	}
	if !strings.Contains(out, "Version: cli=1.2.3 | daemon=unknown") {
		t.Fatalf("expected version line with unknown daemon, got=%q", out)
	}
	if strings.Contains(out, "Press q to quit") {
		t.Fatalf("unexpected watch hint in non-watch mode: %q", out)
	}
}

func TestRenderStatusDashboardWatchHint(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: "https://example.com"},
		},
	}
	renderStatusDashboard(&buf, map[string]any{"strategy": "round_robin", "version": "2.0.0"}, map[string]proxy.ProviderStatView{}, cfg, "1.2.3", true, 120)
	if !strings.Contains(buf.String(), "Press q to quit") {
		t.Fatalf("expected watch hint, got=%q", buf.String())
	}
	if !strings.Contains(buf.String(), "Version: cli=1.2.3 | daemon=2.0.0") {
		t.Fatalf("expected dual version line, got=%q", buf.String())
	}
}

func TestRenderStatusDashboardWatchHintDoesNotPadToTableWidth(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: "https://example.com"},
		},
	}

	renderStatusDashboard(&buf, map[string]any{"strategy": "round_robin"}, map[string]proxy.ProviderStatView{}, cfg, "1.2.3", true, 80)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) == 0 {
		t.Fatal("expected output lines")
	}
	if got := lines[len(lines)-1]; got != "Press q to quit" {
		t.Fatalf("expected unpadded watch hint, got=%q full=%q", got, buf.String())
	}
}

func TestRenderStatusDashboardUsesCompactTableOnMediumWidth(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := config.Config{
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: "https://example.com"}},
	}
	stats := map[string]proxy.ProviderStatView{
		"p1": {
			ActiveConnections: 1,
			TotalRequests:     10,
			ErrorRequests:     1,
			AvgDurationMs:     120,
			PromptTokens:      20,
			CompletionTokens:  40,
		},
	}

	renderStatusDashboard(&buf, map[string]any{"strategy": "round_robin"}, stats, cfg, "1.2.3", false, 85)
	out := buf.String()
	if !strings.Contains(out, "Provider") || !strings.Contains(out, "Req") || !strings.Contains(out, "Tokens") {
		t.Fatalf("expected compact table headers, got=%q", out)
	}
	if strings.Contains(out, "Requests (Tot/Err)") || strings.Contains(out, "Throughput/Tokens") {
		t.Fatalf("expected compact layout without wide headers, got=%q", out)
	}
}

func TestRenderStatusDashboardUsesBlockLayoutOnNarrowWidth(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := config.Config{
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: "https://example.com"}},
	}
	stats := map[string]proxy.ProviderStatView{
		"p1": {
			ActiveConnections: 1,
			TotalRequests:     10,
			ErrorRequests:     1,
			AvgDurationMs:     120,
			PromptTokens:      20,
			CompletionTokens:  40,
		},
	}

	renderStatusDashboard(&buf, map[string]any{"strategy": "round_robin"}, stats, cfg, "1.2.3", false, 60)
	out := buf.String()
	for _, want := range []string{"Provider: p1", "Status: Online", "Requests: 10 / 1", "Latency: 120ms", "Tokens: ↑20 / ↓40"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected narrow layout to contain %q, got=%q", want, out)
		}
	}
	if strings.Contains(out, "Requests (Tot/Err)") || strings.Contains(out, "Throughput/Tokens") {
		t.Fatalf("expected block layout without table headers, got=%q", out)
	}
}

func TestStatusColumnWidthsFitAvailableWidth(t *testing.T) {
	t.Parallel()

	widths := statusColumnWidths(80)
	if len(widths) != 6 {
		t.Fatalf("unexpected column count: %d", len(widths))
	}

	total := 0
	for _, width := range widths {
		if width < 8 {
			t.Fatalf("column width too small: %d", width)
		}
		total += width
	}

	if total > 80 {
		t.Fatalf("column widths overflow: total=%d widths=%v", total, widths)
	}
}

func TestStatusColumnWidthsExpandWithAvailableWidth(t *testing.T) {
	t.Parallel()

	narrow := statusColumnWidths(80)
	wide := statusColumnWidths(120)
	if wide[len(wide)-1] <= narrow[len(narrow)-1] {
		t.Fatalf("expected wider layout to expand last column: narrow=%v wide=%v", narrow, wide)
	}
}

func TestShouldQuitWatchKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  byte
		want bool
	}{
		{name: "lowercase q", key: 'q', want: true},
		{name: "uppercase q", key: 'Q', want: true},
		{name: "other key", key: 'x', want: false},
		{name: "ctrl c", key: 3, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldQuitWatchKey(tc.key); got != tc.want {
				t.Fatalf("shouldQuitWatchKey(%q): got=%v want=%v", tc.key, got, tc.want)
			}
		})
	}
}

func TestRunStatusOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	statusSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"server": map[string]any{
				"uptime_seconds": 12,
				"strategy":       "round_robin",
				"version":        "2.0.0",
			},
			"providers": map[string]any{
				"p1": map[string]any{
					"name":               "p1",
					"active_connections": 0,
					"total_requests":     10,
					"success_requests":   9,
					"error_requests":     1,
					"avg_duration_ms":    100,
					"total_bytes":        1024,
					"prompt_tokens":      20,
					"completion_tokens":  40,
				},
			},
		})
	}))
	defer statusSrv.Close()

	addr := strings.TrimPrefix(statusSrv.URL, "http://")
	cfgPath := filepath.Join(home, ".ai_proxy_pool", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfgBody := []byte("server:\n  listen_addr: \"" + addr + "\"\nrouter: {}\nproviders:\n  - name: \"p1\"\n    base_url: \"https://example.com\"\n")
	if err := os.WriteFile(cfgPath, cfgBody, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var buf bytes.Buffer
	if err := runStatus(&buf, false); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"AI Proxy Pool Status", "p1", "round_robin", "Version: cli=dev | daemon=2.0.0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got=%q", want, out)
		}
	}
}

func TestStatusResetCallsInternalAPI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var called int32

	statusSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/status/reset" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		called++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer statusSrv.Close()

	addr := strings.TrimPrefix(statusSrv.URL, "http://")
	cfgPath := filepath.Join(home, ".ai_proxy_pool", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfgBody := []byte("server:\n  listen_addr: \"" + addr + "\"\nrouter: {}\nproviders:\n  - name: \"p1\"\n    base_url: \"https://example.com\"\n")
	if err := os.WriteFile(cfgPath, cfgBody, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var buf bytes.Buffer
	if err := runStatusReset(&buf); err != nil {
		t.Fatalf("runStatusReset: %v", err)
	}

	if called != 1 {
		t.Fatalf("expected reset endpoint to be called once, got %d", called)
	}

	if !strings.Contains(buf.String(), "Status metrics reset successfully.") {
		t.Fatalf("unexpected output: %s", buf.String())
	}
}

func TestStatusResetErrorOnNon200(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	statusSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/status/reset" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer statusSrv.Close()

	addr := strings.TrimPrefix(statusSrv.URL, "http://")
	cfgPath := filepath.Join(home, ".ai_proxy_pool", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfgBody := []byte("server:\n  listen_addr: \"" + addr + "\"\nrouter: {}\nproviders:\n  - name: \"p1\"\n    base_url: \"https://example.com\"\n")
	if err := os.WriteFile(cfgPath, cfgBody, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var buf bytes.Buffer
	err := runStatusReset(&buf)
	if err == nil || !strings.Contains(err.Error(), "status reset failed") {
		t.Fatalf("expected status reset failed error, got: %v", err)
	}
}
