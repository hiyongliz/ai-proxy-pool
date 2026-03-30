package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/proxy"
)

func TestResolveStatsPathUsesSelectedConfigIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, ".ai_proxy_pool")
	activePath := filepath.Join(root, "config.yaml")
	selectedPath := filepath.Join(root, "configs", "provider-b.yaml")
	if err := os.MkdirAll(filepath.Dir(selectedPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	body := []byte(sampleConfigYAML("https://example.com"))
	if err := os.WriteFile(activePath, body, 0o644); err != nil {
		t.Fatalf("write active: %v", err)
	}
	if err := os.WriteFile(selectedPath, body, 0o644); err != nil {
		t.Fatalf("write selected: %v", err)
	}
	if err := writeSelectedConfigPath(selectedPath); err != nil {
		t.Fatalf("writeSelectedConfigPath: %v", err)
	}

	if got, want := resolveStatsPath(activePath), resolveStatsPath(selectedPath); got != want {
		t.Fatalf("stats path should use selected config identity: got=%q want=%q", got, want)
	}
}

func TestReloadConfigSwitchesStatsBucketWhenConfigIdentityChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, ".ai_proxy_pool")
	activePath := filepath.Join(root, "config.yaml")
	selectedA := filepath.Join(root, "configs", "provider-a.yaml")
	selectedB := filepath.Join(root, "configs", "provider-b.yaml")
	if err := os.MkdirAll(filepath.Dir(selectedA), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}

	configA := []byte(sampleConfigYAML("https://provider-a.example.com"))
	configB := []byte(sampleConfigYAML("https://provider-b.example.com"))
	if err := os.WriteFile(selectedA, configA, 0o644); err != nil {
		t.Fatalf("write selectedA: %v", err)
	}
	if err := os.WriteFile(selectedB, configB, 0o644); err != nil {
		t.Fatalf("write selectedB: %v", err)
	}
	if err := os.WriteFile(activePath, configA, 0o644); err != nil {
		t.Fatalf("write active: %v", err)
	}
	if err := writeSelectedConfigPath(selectedA); err != nil {
		t.Fatalf("writeSelectedConfigPath selectedA: %v", err)
	}

	statsPathA := resolveStatsPath(activePath)
	statsA := &proxy.GlobalStats{}
	atomic.StoreInt64(&statsA.GetOrCreate("p1").TotalRequests, 11)
	statsA.PersistTo(statsPathA)

	cfg, err := config.Load(activePath)
	if err != nil {
		t.Fatalf("load config A: %v", err)
	}
	loadedA := &proxy.GlobalStats{}
	loadedA.LoadFromDiskAt(statsPathA)
	server, err := proxy.NewServerWithStats(cfg, loadedA)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	runtime := newDaemonRuntime(statsPathA)
	handler := proxy.NewReloadableHandler(server)

	if err := os.WriteFile(activePath, configB, 0o644); err != nil {
		t.Fatalf("write active config B: %v", err)
	}
	if err := writeSelectedConfigPath(selectedB); err != nil {
		t.Fatalf("writeSelectedConfigPath selectedB: %v", err)
	}

	statsPathB := resolveStatsPath(activePath)
	statsB := &proxy.GlobalStats{}
	atomic.StoreInt64(&statsB.GetOrCreate("p1").TotalRequests, 22)
	statsB.PersistTo(statsPathB)

	reloadConfig(activePath, &cfg, runtime, handler, "test")

	current := handler.Current()
	if current == nil {
		t.Fatal("expected current server after reload")
	}
	if got := current.Stats().Snapshot()["p1"].TotalRequests; got != 22 {
		t.Fatalf("expected stats bucket B after reload, got total_requests=%d", got)
	}
	if got := runtime.StatsPath(); got != statsPathB {
		t.Fatalf("expected runtime stats path to switch: got=%q want=%q", got, statsPathB)
	}
}

func TestReloadConfigKeepsStatsBucketWhenConfigIdentityUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, ".ai_proxy_pool")
	activePath := filepath.Join(root, "config.yaml")
	selectedPath := filepath.Join(root, "configs", "provider-a.yaml")
	if err := os.MkdirAll(filepath.Dir(selectedPath), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}

	configBody := []byte(sampleConfigYAML("https://provider-a.example.com"))
	if err := os.WriteFile(activePath, configBody, 0o644); err != nil {
		t.Fatalf("write active: %v", err)
	}
	if err := os.WriteFile(selectedPath, configBody, 0o644); err != nil {
		t.Fatalf("write selected: %v", err)
	}
	if err := writeSelectedConfigPath(selectedPath); err != nil {
		t.Fatalf("writeSelectedConfigPath: %v", err)
	}

	statsPath := resolveStatsPath(activePath)
	stats := &proxy.GlobalStats{}
	atomic.StoreInt64(&stats.GetOrCreate("p1").TotalRequests, 11)

	cfg, err := config.Load(activePath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	server, err := proxy.NewServerWithStats(cfg, stats)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	runtime := newDaemonRuntime(statsPath)
	handler := proxy.NewReloadableHandler(server)

	reloadConfig(activePath, &cfg, runtime, handler, "test")

	current := handler.Current()
	if current == nil {
		t.Fatal("expected current server after reload")
	}
	if current.Stats() != stats {
		t.Fatal("expected reload to reuse current stats when identity is unchanged")
	}
	if got := current.Stats().Snapshot()["p1"].TotalRequests; got != 11 {
		t.Fatalf("expected existing stats to be preserved, got total_requests=%d", got)
	}
	if got := runtime.StatsPath(); got != statsPath {
		t.Fatalf("expected runtime stats path to remain unchanged: got=%q want=%q", got, statsPath)
	}
}

func TestReloadConfigUsesEmptyStatsWhenNewBucketDoesNotExist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, ".ai_proxy_pool")
	activePath := filepath.Join(root, "config.yaml")
	selectedA := filepath.Join(root, "configs", "provider-a.yaml")
	selectedB := filepath.Join(root, "configs", "provider-b.yaml")
	if err := os.MkdirAll(filepath.Dir(selectedA), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}

	configA := []byte(sampleConfigYAML("https://provider-a.example.com"))
	configB := []byte(sampleConfigYAML("https://provider-b.example.com"))
	if err := os.WriteFile(selectedA, configA, 0o644); err != nil {
		t.Fatalf("write selectedA: %v", err)
	}
	if err := os.WriteFile(selectedB, configB, 0o644); err != nil {
		t.Fatalf("write selectedB: %v", err)
	}
	if err := os.WriteFile(activePath, configA, 0o644); err != nil {
		t.Fatalf("write active: %v", err)
	}
	if err := writeSelectedConfigPath(selectedA); err != nil {
		t.Fatalf("writeSelectedConfigPath selectedA: %v", err)
	}

	statsPathA := resolveStatsPath(activePath)
	stats := &proxy.GlobalStats{}
	atomic.StoreInt64(&stats.GetOrCreate("p1").TotalRequests, 11)

	cfg, err := config.Load(activePath)
	if err != nil {
		t.Fatalf("load config A: %v", err)
	}
	server, err := proxy.NewServerWithStats(cfg, stats)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	runtime := newDaemonRuntime(statsPathA)
	handler := proxy.NewReloadableHandler(server)

	if err := os.WriteFile(activePath, configB, 0o644); err != nil {
		t.Fatalf("write active config B: %v", err)
	}
	if err := writeSelectedConfigPath(selectedB); err != nil {
		t.Fatalf("writeSelectedConfigPath selectedB: %v", err)
	}

	statsPathB := resolveStatsPath(activePath)
	if _, err := os.Stat(statsPathB); !os.IsNotExist(err) {
		t.Fatalf("expected new stats bucket to not exist yet, stat err=%v", err)
	}

	reloadConfig(activePath, &cfg, runtime, handler, "test")

	current := handler.Current()
	if current == nil {
		t.Fatal("expected current server after reload")
	}
	if got := current.Stats().Snapshot()["p1"].TotalRequests; got != 0 {
		t.Fatalf("expected empty stats in new bucket, got total_requests=%d", got)
	}
	if got := runtime.StatsPath(); got != statsPathB {
		t.Fatalf("expected runtime stats path to switch: got=%q want=%q", got, statsPathB)
	}
}

func TestReloadConfigRestoresStatsWhenSwitchingBackToPreviousBucket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, ".ai_proxy_pool")
	activePath := filepath.Join(root, "config.yaml")
	selectedA := filepath.Join(root, "configs", "provider-a.yaml")
	selectedB := filepath.Join(root, "configs", "provider-b.yaml")
	if err := os.MkdirAll(filepath.Dir(selectedA), 0o755); err != nil {
		t.Fatalf("mkdir configs: %v", err)
	}

	configA := []byte(sampleConfigYAML("https://provider-a.example.com"))
	configB := []byte(sampleConfigYAML("https://provider-b.example.com"))
	if err := os.WriteFile(selectedA, configA, 0o644); err != nil {
		t.Fatalf("write selectedA: %v", err)
	}
	if err := os.WriteFile(selectedB, configB, 0o644); err != nil {
		t.Fatalf("write selectedB: %v", err)
	}
	if err := os.WriteFile(activePath, configA, 0o644); err != nil {
		t.Fatalf("write active: %v", err)
	}
	if err := writeSelectedConfigPath(selectedA); err != nil {
		t.Fatalf("writeSelectedConfigPath selectedA: %v", err)
	}

	statsPathA := resolveStatsPath(activePath)
	statsA := &proxy.GlobalStats{}
	atomic.StoreInt64(&statsA.GetOrCreate("p1").TotalRequests, 11)
	statsA.PersistTo(statsPathA)

	cfg, err := config.Load(activePath)
	if err != nil {
		t.Fatalf("load config A: %v", err)
	}
	loadedA := &proxy.GlobalStats{}
	loadedA.LoadFromDiskAt(statsPathA)
	server, err := proxy.NewServerWithStats(cfg, loadedA)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	runtime := newDaemonRuntime(statsPathA)
	handler := proxy.NewReloadableHandler(server)

	if err := os.WriteFile(activePath, configB, 0o644); err != nil {
		t.Fatalf("write active config B: %v", err)
	}
	if err := writeSelectedConfigPath(selectedB); err != nil {
		t.Fatalf("writeSelectedConfigPath selectedB: %v", err)
	}

	statsPathB := resolveStatsPath(activePath)
	statsB := &proxy.GlobalStats{}
	atomic.StoreInt64(&statsB.GetOrCreate("p1").TotalRequests, 22)
	statsB.PersistTo(statsPathB)

	reloadConfig(activePath, &cfg, runtime, handler, "to-b")

	currentB := handler.Current()
	if currentB == nil {
		t.Fatal("expected current server after reload to B")
	}
	if got := currentB.Stats().Snapshot()["p1"].TotalRequests; got != 22 {
		t.Fatalf("expected stats bucket B after reload, got total_requests=%d", got)
	}

	if err := os.WriteFile(activePath, configA, 0o644); err != nil {
		t.Fatalf("write active config A: %v", err)
	}
	if err := writeSelectedConfigPath(selectedA); err != nil {
		t.Fatalf("writeSelectedConfigPath selectedA again: %v", err)
	}

	reloadConfig(activePath, &cfg, runtime, handler, "to-a")

	currentA := handler.Current()
	if currentA == nil {
		t.Fatal("expected current server after reload back to A")
	}
	if got := currentA.Stats().Snapshot()["p1"].TotalRequests; got != 11 {
		t.Fatalf("expected stats bucket A to be restored, got total_requests=%d", got)
	}
	if got := runtime.StatsPath(); got != statsPathA {
		t.Fatalf("expected runtime stats path to switch back: got=%q want=%q", got, statsPathA)
	}
}

func sampleConfigYAML(baseURL string) string {
	return `router:
  strategy: "round_robin"
providers:
  - name: "p1"
    base_url: "` + baseURL + `"
`
}
