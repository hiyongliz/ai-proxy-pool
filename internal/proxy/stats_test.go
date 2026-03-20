package proxy

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
)

func TestProviderHealthWindowSnapshot(t *testing.T) {
	t.Parallel()

	window := newProviderHealthWindow(3)
	window.record(100*time.Millisecond, false)
	window.record(200*time.Millisecond, true)
	window.record(300*time.Millisecond, false)

	enough, samples, failureRate, avg := window.snapshot(2)
	if !enough {
		t.Fatal("expected enough samples")
	}
	if samples != 3 {
		t.Fatalf("unexpected samples: got=%d", samples)
	}
	if failureRate != 1.0/3.0 {
		t.Fatalf("unexpected failure rate: got=%v", failureRate)
	}
	if avg != 200*time.Millisecond {
		t.Fatalf("unexpected avg duration: got=%s", avg)
	}
}

func TestProviderHealthWindowOverwriteOldSamples(t *testing.T) {
	t.Parallel()

	window := newProviderHealthWindow(2)
	window.record(100*time.Millisecond, false)
	window.record(300*time.Millisecond, true)
	window.record(500*time.Millisecond, false)

	enough, samples, failureRate, avg := window.snapshot(2)
	if !enough {
		t.Fatal("expected enough samples")
	}
	if samples != 2 {
		t.Fatalf("unexpected samples: got=%d", samples)
	}
	if failureRate != 0.5 {
		t.Fatalf("unexpected failure rate: got=%v", failureRate)
	}
	if avg != 400*time.Millisecond {
		t.Fatalf("unexpected avg duration: got=%s", avg)
	}
}

func TestProviderHealthWindowInsufficientSamples(t *testing.T) {
	t.Parallel()

	window := newProviderHealthWindow(3)
	window.record(100*time.Millisecond, false)

	enough, samples, failureRate, avg := window.snapshot(2)
	if enough {
		t.Fatal("expected insufficient samples")
	}
	if samples != 1 {
		t.Fatalf("unexpected samples: got=%d", samples)
	}
	if failureRate != 0 {
		t.Fatalf("unexpected failure rate: got=%v", failureRate)
	}
	if avg != 100*time.Millisecond {
		t.Fatalf("unexpected avg duration: got=%s", avg)
	}
}

func TestNewServerUsesIsolatedStatsByDefault(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Router: config.RouterConfig{Strategy: config.StrategyRoundRobin},
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: "https://example.com"}},
	}

	serverA, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server A: %v", err)
	}
	serverB, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server B: %v", err)
	}

	if serverA.stats == GetGlobalStats() {
		t.Fatal("expected serverA to use isolated stats by default")
	}
	if serverB.stats == GetGlobalStats() {
		t.Fatal("expected serverB to use isolated stats by default")
	}
	if serverA.stats == serverB.stats {
		t.Fatal("expected each server to get its own stats instance by default")
	}
}

func TestNewServerWithStatsUsesInjectedStats(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Router: config.RouterConfig{Strategy: config.StrategyRoundRobin},
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: "https://example.com"}},
	}
	injectedStats := &GlobalStats{}

	server, err := NewServerWithStats(cfg, injectedStats)
	if err != nil {
		t.Fatalf("new server with stats: %v", err)
	}

	if server.stats != injectedStats {
		t.Fatal("expected server to use injected stats instance")
	}
}

func TestGlobalStatsLoadFromDiskRestoresBreakerState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	persisted := &GlobalStats{}
	persistedProvider := persisted.GetOrCreate("provider-a")
	openUntil := time.Now().Add(10 * time.Minute).Unix()
	atomic.StoreInt32(&persistedProvider.ConsecutiveErrors, 5)
	atomic.StoreInt64(&persistedProvider.CircuitOpenUntil, openUntil)
	persisted.Persist()

	loaded := &GlobalStats{}
	loaded.LoadFromDisk()
	loadedProvider := loaded.GetOrCreate("provider-a")

	if got := atomic.LoadInt32(&loadedProvider.ConsecutiveErrors); got != 5 {
		t.Fatalf("ConsecutiveErrors = %d, want %d", got, 5)
	}
	if got := atomic.LoadInt64(&loadedProvider.CircuitOpenUntil); got != openUntil {
		t.Fatalf("CircuitOpenUntil = %d, want %d", got, openUntil)
	}
}

func TestGlobalStatsResetAllCounters(t *testing.T) {
	t.Parallel()

	g := &GlobalStats{}
	ps := g.GetOrCreate("provider-a")

	// 初始化计数字段为非 0
	atomic.StoreInt64(&ps.ActiveConnections, 3)
	atomic.StoreInt64(&ps.TotalRequests, 10)
	atomic.StoreInt64(&ps.SuccessRequests, 7)
	atomic.StoreInt64(&ps.ErrorRequests, 3)
	atomic.StoreInt64(&ps.TotalDurationMs, 1234)
	atomic.StoreInt64(&ps.TotalBytes, 5678)
	atomic.StoreInt64(&ps.PromptTokens, 111)
	atomic.StoreInt64(&ps.CompletionTokens, 222)

	// 熔断相关字段设置为非 0
	atomic.StoreInt32(&ps.ConsecutiveErrors, 5)
	openUntil := time.Now().Add(10 * time.Minute).Unix()
	atomic.StoreInt64(&ps.CircuitOpenUntil, openUntil)

	g.ResetAllCounters()

	if got := atomic.LoadInt64(&ps.ActiveConnections); got != 0 {
		t.Errorf("ActiveConnections = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&ps.TotalRequests); got != 0 {
		t.Errorf("TotalRequests = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&ps.SuccessRequests); got != 0 {
		t.Errorf("SuccessRequests = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&ps.ErrorRequests); got != 0 {
		t.Errorf("ErrorRequests = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&ps.TotalDurationMs); got != 0 {
		t.Errorf("TotalDurationMs = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&ps.TotalBytes); got != 0 {
		t.Errorf("TotalBytes = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&ps.PromptTokens); got != 0 {
		t.Errorf("PromptTokens = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&ps.CompletionTokens); got != 0 {
		t.Errorf("CompletionTokens = %d, want 0", got)
	}

	if got := atomic.LoadInt32(&ps.ConsecutiveErrors); got != 5 {
		t.Errorf("ConsecutiveErrors = %d, want %d", got, 5)
	}
	if got := atomic.LoadInt64(&ps.CircuitOpenUntil); got != openUntil {
		t.Errorf("CircuitOpenUntil = %d, want %d", got, openUntil)
	}
}
