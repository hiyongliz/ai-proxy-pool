package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
