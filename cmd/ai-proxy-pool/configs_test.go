package main

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestListConfigFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "b.yaml"))
	mustWriteFile(t, filepath.Join(dir, "a.yml"))
	mustWriteFile(t, filepath.Join(dir, "ignore.txt"))
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	got, err := listConfigFiles(dir)
	if err != nil {
		t.Fatalf("listConfigFiles: %v", err)
	}

	want := []string{
		filepath.Join(dir, "a.yml"),
		filepath.Join(dir, "b.yaml"),
	}

	if len(got) != len(want) {
		t.Fatalf("unexpected file count: got=%d want=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected file at index %d: got=%q want=%q", i, got[i], want[i])
		}
	}
}

func TestAppendLegacyConfigIfExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	existing := filepath.Join(dir, "default.yaml")
	legacy := filepath.Join(dir, "config.yaml")
	mustWriteFile(t, existing)
	mustWriteFile(t, legacy)

	files := []string{existing}
	got := appendLegacyConfigIfExists(files, legacy)
	if len(got) != 2 {
		t.Fatalf("unexpected file count: got=%d want=2", len(got))
	}
	if got[0] != legacy || got[1] != existing {
		t.Fatalf("unexpected files order/content: %#v", got)
	}
}

func TestAppendLegacyConfigIfExistsNoopWhenMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	existing := filepath.Join(dir, "default.yaml")
	mustWriteFile(t, existing)

	missing := filepath.Join(dir, "config.yaml")
	got := appendLegacyConfigIfExists([]string{existing}, missing)
	if len(got) != 1 || got[0] != existing {
		t.Fatalf("unexpected files: %#v", got)
	}
}

func TestActivateConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "source.yaml")
	target := filepath.Join(dir, "active.yaml")

	sourceBody := []byte("server:\n  listen_addr: \":19090\"\n")
	if err := os.WriteFile(source, sourceBody, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	if err := activateConfig(source, target); err != nil {
		t.Fatalf("activateConfig: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != string(sourceBody) {
		t.Fatalf("unexpected target body: %q", string(got))
	}
}

func TestNewSwitchConfigModelUsesActiveIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	model := newSwitchConfigModel(
		[]string{a, b},
		normalizedPath(b),
		filepath.Join(dir, "config.yaml"),
	)
	if model.cursor != 1 {
		t.Fatalf("unexpected cursor: got=%d want=1", model.cursor)
	}
}

func TestSwitchConfigModelSelectsOnEnter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	target := filepath.Join(home, ".ai_proxy_pool", "config.yaml")
	sourceA := filepath.Join(home, ".ai_proxy_pool", "configs", "a.yaml")
	sourceB := filepath.Join(home, ".ai_proxy_pool", "configs", "b.yaml")
	if err := os.MkdirAll(filepath.Dir(sourceA), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(sourceA, []byte("name: a\n"), 0o644); err != nil {
		t.Fatalf("write sourceA: %v", err)
	}
	if err := os.WriteFile(sourceB, []byte("name: b\n"), 0o644); err != nil {
		t.Fatalf("write sourceB: %v", err)
	}
	if err := os.WriteFile(target, []byte("name: a\n"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	model := switchConfigModel{
		files:            []string{sourceA, sourceB},
		activeConfig:     normalizedPath(sourceA),
		activeTargetPath: target,
		cursor:           1,
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got, ok := next.(switchConfigModel)
	if !ok {
		t.Fatalf("unexpected model type")
	}
	if got.activeConfig != normalizedPath(sourceB) {
		t.Fatalf("unexpected active config: got=%q want=%q", got.activeConfig, normalizedPath(sourceB))
	}
	if cmd != nil {
		t.Fatalf("did not expect quit command")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(content) != "name: b\n" {
		t.Fatalf("unexpected target content: %q", string(content))
	}
}

func TestSwitchConfigModelCancelsOnQ(t *testing.T) {
	t.Parallel()

	model := switchConfigModel{
		files: []string{"a.yaml"},
	}
	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	got, ok := next.(switchConfigModel)
	if !ok {
		t.Fatalf("unexpected model type")
	}
	if !got.cancelled {
		t.Fatalf("expected cancelled=true")
	}
	if cmd == nil {
		t.Fatalf("expected quit command")
	}
}

func TestResolveActiveConfigForDisplayUsesSelectedPathWhenInSync(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, ".ai_proxy_pool")
	configPath := filepath.Join(root, "config.yaml")
	selectedPath := filepath.Join(root, "configs", "modelscope.yaml")
	if err := os.MkdirAll(filepath.Dir(selectedPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	body := []byte("server:\n  listen_addr: ':18080'\n")
	if err := os.WriteFile(configPath, body, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(selectedPath, body, 0o644); err != nil {
		t.Fatalf("write selected: %v", err)
	}
	if err := writeSelectedConfigPath(selectedPath); err != nil {
		t.Fatalf("writeSelectedConfigPath: %v", err)
	}

	got := resolveActiveConfigForDisplay(configPath)
	if got != normalizedPath(selectedPath) {
		t.Fatalf("unexpected active display path: got=%q want=%q", got, normalizedPath(selectedPath))
	}
}

func TestResolveActiveConfigForDisplayFallsBackWhenOutOfSync(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, ".ai_proxy_pool")
	configPath := filepath.Join(root, "config.yaml")
	selectedPath := filepath.Join(root, "configs", "modelscope.yaml")
	if err := os.MkdirAll(filepath.Dir(selectedPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(configPath, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(selectedPath, []byte("a: 2\n"), 0o644); err != nil {
		t.Fatalf("write selected: %v", err)
	}
	if err := writeSelectedConfigPath(selectedPath); err != nil {
		t.Fatalf("writeSelectedConfigPath: %v", err)
	}

	got := resolveActiveConfigForDisplay(configPath)
	if got != normalizedPath(configPath) {
		t.Fatalf("unexpected active display path: got=%q want=%q", got, normalizedPath(configPath))
	}
}

func TestInferActiveConfigFromCandidatesSingleMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	active := filepath.Join(dir, "config.yaml")
	candidate := filepath.Join(dir, "configs", "modelscope.yaml")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	body := []byte("name: modelscope\n")
	if err := os.WriteFile(active, body, 0o644); err != nil {
		t.Fatalf("write active: %v", err)
	}
	if err := os.WriteFile(candidate, body, 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	got := inferActiveConfigFromCandidates(active, []string{candidate, active})
	if got != normalizedPath(candidate) {
		t.Fatalf("unexpected inferred path: got=%q want=%q", got, normalizedPath(candidate))
	}
}

func TestInferActiveConfigFromCandidatesMultipleMatchesFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	active := filepath.Join(dir, "config.yaml")
	candidateA := filepath.Join(dir, "configs", "a.yaml")
	candidateB := filepath.Join(dir, "configs", "b.yaml")
	if err := os.MkdirAll(filepath.Dir(candidateA), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	body := []byte("name: same\n")
	if err := os.WriteFile(active, body, 0o644); err != nil {
		t.Fatalf("write active: %v", err)
	}
	if err := os.WriteFile(candidateA, body, 0o644); err != nil {
		t.Fatalf("write candidateA: %v", err)
	}
	if err := os.WriteFile(candidateB, body, 0o644); err != nil {
		t.Fatalf("write candidateB: %v", err)
	}

	got := inferActiveConfigFromCandidates(active, []string{candidateA, candidateB})
	if got != normalizedPath(active) {
		t.Fatalf("unexpected inferred path: got=%q want=%q", got, normalizedPath(active))
	}
}

func TestShouldInferActiveConfigFromContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if !shouldInferActiveConfigFromContent() {
		t.Fatalf("expected infer=true when no selection state exists")
	}
	if err := writeSelectedConfigPath(filepath.Join(home, ".ai_proxy_pool", "config.yaml")); err != nil {
		t.Fatalf("writeSelectedConfigPath: %v", err)
	}
	if shouldInferActiveConfigFromContent() {
		t.Fatalf("expected infer=false when selection state exists")
	}
}

func TestExplicitSelectionConfigYAMLMustNotBeOverriddenByInference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, ".ai_proxy_pool")
	active := filepath.Join(root, "config.yaml")
	candidate := filepath.Join(root, "configs", "modelscope.yaml")
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	body := []byte("name: same\n")
	if err := os.WriteFile(active, body, 0o644); err != nil {
		t.Fatalf("write active: %v", err)
	}
	if err := os.WriteFile(candidate, body, 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if err := writeSelectedConfigPath(active); err != nil {
		t.Fatalf("writeSelectedConfigPath: %v", err)
	}

	activeConfig := resolveActiveConfigForDisplay(active)
	files := []string{active, candidate}
	if shouldInferActiveConfigFromContent() {
		activeConfig = inferActiveConfigFromCandidates(activeConfig, files)
	}
	if activeConfig != normalizedPath(active) {
		t.Fatalf("unexpected active path: got=%q want=%q", activeConfig, normalizedPath(active))
	}
}

func mustWriteFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func TestParseConfigSummary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "test.yaml")
	content := `server:
  listen_addr: ":8081"
router:
  strategy: "weighted_random"
  default_provider: "primary"
providers:
  - name: primary
    enabled: true
  - name: backup
  - name: disabled
    enabled: false
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got := parseConfigSummary(configPath)
	if got.ParseError != "" {
		t.Fatalf("unexpected parse error: %s", got.ParseError)
	}
	if got.ProviderCount != 3 {
		t.Errorf("ProviderCount: got=%d want=3", got.ProviderCount)
	}
	if got.EnabledCount != 2 {
		t.Errorf("EnabledCount: got=%d want=2", got.EnabledCount)
	}
	if got.ListenAddr != ":8081" {
		t.Errorf("ListenAddr: got=%q want=:8081", got.ListenAddr)
	}
	if got.Strategy != "weighted_random" {
		t.Errorf("Strategy: got=%q want=weighted_random", got.Strategy)
	}
	if got.DefaultProvider != "primary" {
		t.Errorf("DefaultProvider: got=%q want=primary", got.DefaultProvider)
	}
}

func TestParseConfigSummaryInvalidYAML(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(configPath, []byte("invalid: yaml: content:"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got := parseConfigSummary(configPath)
	if got.ParseError == "" {
		t.Fatalf("expected parse error for invalid YAML")
	}
}

func TestParseConfigSummaryMissingFile(t *testing.T) {
	t.Parallel()

	got := parseConfigSummary("/nonexistent/path/config.yaml")
	if got.ParseError == "" {
		t.Fatalf("expected parse error for missing file")
	}
}
