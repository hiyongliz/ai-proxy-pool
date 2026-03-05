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
	renderStatusDashboard(&buf, map[string]any{}, map[string]proxy.ProviderStatView{}, cfg)
	out := buf.String()
	if !strings.Contains(out, "Strategy:") {
		t.Fatalf("unexpected output: %q", out)
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
	for _, want := range []string{"AI Proxy Pool Status", "p1", "round_robin"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got=%q", want, out)
		}
	}
}
