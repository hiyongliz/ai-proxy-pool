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
