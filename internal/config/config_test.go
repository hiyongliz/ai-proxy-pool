package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadDefaultsMaxRequestBodyBytes(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
server: {}
router: {}
providers:
  - name: "p1"
    base_url: "https://example.com"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Server.MaxRequestBodyBytes != 8*1024*1024 {
		t.Fatalf("unexpected max request body bytes: got=%d", cfg.Server.MaxRequestBodyBytes)
	}
	if cfg.Server.CircuitBreaker.Threshold != 3 {
		t.Fatalf("unexpected circuit breaker threshold: got=%d", cfg.Server.CircuitBreaker.Threshold)
	}
	if cfg.Server.CircuitBreaker.OpenDuration != 120*time.Second {
		t.Fatalf("unexpected circuit breaker open duration: got=%s", cfg.Server.CircuitBreaker.OpenDuration)
	}
}

func TestLoadRejectsNonPositiveMaxRequestBodyBytes(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
server:
  max_request_body_bytes: -1
router: {}
providers:
  - name: "p1"
    base_url: "https://example.com"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "server.max_request_body_bytes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadRejectsNonPositiveCircuitBreaker(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		yaml   string
		error  string
	}{
		{
			name: "non-positive threshold",
			yaml: `
server:
  circuit_breaker:
    threshold: 0
    open_duration: 120s
router: {}
providers:
  - name: "p1"
    base_url: "https://example.com"
`,
			error: "server.circuit_breaker.threshold",
		},
		{
			name: "non-positive open_duration",
			yaml: `
server:
  circuit_breaker:
    threshold: 3
    open_duration: 0s
router: {}
providers:
  - name: "p1"
    base_url: "https://example.com"
`,
			error: "server.circuit_breaker.open_duration",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfigFile(t, tc.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.error) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadAcceptsCustomCircuitBreaker(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
server:
  circuit_breaker:
    threshold: 5
    open_duration: 30s
router: {}
providers:
  - name: "p1"
    base_url: "https://example.com"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Server.CircuitBreaker.Threshold != 5 {
		t.Fatalf("unexpected circuit breaker threshold: got=%d", cfg.Server.CircuitBreaker.Threshold)
	}
	if cfg.Server.CircuitBreaker.OpenDuration != 30*time.Second {
		t.Fatalf("unexpected circuit breaker open duration: got=%s", cfg.Server.CircuitBreaker.OpenDuration)
	}
}

func TestLoadRejectsClaudeToCodexWithoutCodexTarget(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
server: {}
router: {}
providers:
  - name: "p1"
    base_url: "https://example.com"
    request_translate: "claude_to_codex"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires target_api=codex") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadAcceptsClaudeToCodexWithCodexTarget(t *testing.T) {
	t.Parallel()

	path := writeConfigFile(t, `
server: {}
router: {}
providers:
  - name: "p1"
    base_url: "https://example.com"
    target_api: "codex"
    request_translate: "claude_to_codex"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := cfg.Providers[0].TargetAPI; got != "codex" {
		t.Fatalf("unexpected target api: %q", got)
	}
	if got := cfg.Providers[0].RequestTranslate; got != "claude_to_codex" {
		t.Fatalf("unexpected request_translate: %q", got)
	}
}

func TestValidateModelRegexMapping(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "valid regex",
			cfg: Config{
				Router: RouterConfig{Strategy: StrategyRoundRobin},
				Providers: []ProviderConfig{{
					Name:      "test",
					BaseURL:   "http://localhost",
					TargetAPI: TargetAPIClaude,
					ModelRegexMapping: []ModelRegexMapping{
						{Regex: "^claude-(.*)$", Replacement: "claude-$1-123"},
					},
				}},
			},
			wantErr: false,
		},
		{
			name: "empty regex",
			cfg: Config{
				Router: RouterConfig{Strategy: StrategyRoundRobin},
				Providers: []ProviderConfig{{
					Name:      "test",
					BaseURL:   "http://localhost",
					TargetAPI: TargetAPIClaude,
					ModelRegexMapping: []ModelRegexMapping{
						{Regex: "", Replacement: "test"},
					},
				}},
			},
			wantErr: true,
		},
		{
			name: "invalid regex",
			cfg: Config{
				Router: RouterConfig{Strategy: StrategyRoundRobin},
				Providers: []ProviderConfig{{
					Name:      "test",
					BaseURL:   "http://localhost",
					TargetAPI: TargetAPIClaude,
					ModelRegexMapping: []ModelRegexMapping{
						{Regex: "[invalid", Replacement: "test"},
					},
				}},
			},
			wantErr: true,
		},
	}

	for i := range tests {
		tt := &tests[i]
		t.Run(tt.name, func(t *testing.T) {
			applyDefaults(&tt.cfg)
			err := validate(&tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			// Verify compilation happened for valid regex
			if !tt.wantErr && len(tt.cfg.Providers[0].ModelRegexMapping) > 0 {
				if tt.cfg.Providers[0].ModelRegexMapping[0].Compiled == nil {
					t.Errorf("expected compiled regex to be populated")
				}
			}
		})
	}
}
