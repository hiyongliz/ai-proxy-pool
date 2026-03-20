package proxy

import (
	"sync"
	"sync/atomic"
	"time"
)

var (
	// serverStartTime records when the proxy pool process was started.
	serverStartTime = time.Now()

	// defaultStats tracking all providers globally across config reloads.
	defaultStats = &GlobalStats{}
)

// GetGlobalStats exports the global stats instance.
func GetGlobalStats() *GlobalStats {
	return defaultStats
}

type healthSample struct {
	failed     bool
	durationMs int64
}

type providerHealthWindow struct {
	mu              sync.Mutex
	entries         []healthSample
	next            int
	count           int
	failureCount    int
	totalDurationMs int64
}

func newProviderHealthWindow(size int) *providerHealthWindow {
	if size <= 0 {
		return &providerHealthWindow{}
	}
	return &providerHealthWindow{entries: make([]healthSample, size)}
}

func (w *providerHealthWindow) record(duration time.Duration, failed bool) {
	if w == nil || len(w.entries) == 0 {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	sample := healthSample{failed: failed, durationMs: duration.Milliseconds()}
	if w.count < len(w.entries) {
		w.entries[w.next] = sample
		w.count++
	} else {
		old := w.entries[w.next]
		if old.failed {
			w.failureCount--
		}
		w.totalDurationMs -= old.durationMs
		w.entries[w.next] = sample
	}

	if sample.failed {
		w.failureCount++
	}
	w.totalDurationMs += sample.durationMs
	w.next = (w.next + 1) % len(w.entries)
}

func (w *providerHealthWindow) snapshot(minSamples int) (bool, int, float64, time.Duration) {
	if w == nil {
		return false, 0, 0, 0
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.count == 0 {
		return false, 0, 0, 0
	}

	failureRate := float64(w.failureCount) / float64(w.count)
	avg := time.Duration(w.totalDurationMs/int64(w.count)) * time.Millisecond
	return w.count >= minSamples, w.count, failureRate, avg
}

// ProviderStats holds real-time atomic counters for a single provider.
type ProviderStats struct {
	ActiveConnections int64
	TotalRequests     int64
	SuccessRequests   int64
	ErrorRequests     int64
	TotalDurationMs   int64
	TotalBytes        int64
	PromptTokens      int64
	CompletionTokens  int64
	HealthWindow      *providerHealthWindow
	windowMu          sync.Mutex

	// 熔断器：连续致命错误计数器（遇顺畅包即归0）
	ConsecutiveErrors int32

	// 熔断器：熔断禁闭结束的时间戳(Unix 秒级)。如果当前时间 < 此值，拒绝路由
	CircuitOpenUntil int64
}

func (ps *ProviderStats) EnsureHealthWindow(size int) *providerHealthWindow {
	if size <= 0 {
		return nil
	}
	ps.windowMu.Lock()
	defer ps.windowMu.Unlock()
	if ps.HealthWindow == nil || len(ps.HealthWindow.entries) != size {
		ps.HealthWindow = newProviderHealthWindow(size)
	}
	return ps.HealthWindow
}

func (ps *ProviderStats) ResetHealthWindow(size int) {
	ps.windowMu.Lock()
	defer ps.windowMu.Unlock()
	if size <= 0 {
		ps.HealthWindow = nil
		return
	}
	ps.HealthWindow = newProviderHealthWindow(size)
}

// ProviderStatView is the JSON representation of provider stats.
type ProviderStatView struct {
	Name              string `json:"name"`
	ActiveConnections int64  `json:"active_connections"`
	TotalRequests     int64  `json:"total_requests"`
	SuccessRequests   int64  `json:"success_requests"`
	ErrorRequests     int64  `json:"error_requests"`
	AvgDurationMs     int64  `json:"avg_duration_ms"`
	TotalBytes        int64  `json:"total_bytes"`
	PromptTokens      int64  `json:"prompt_tokens"`
	CompletionTokens  int64  `json:"completion_tokens"`
	ConsecutiveErrors int32  `json:"consecutive_errors"`
	CircuitOpenUntil  int64  `json:"circuit_open_until"`
}

// GlobalStats maintains statistics mapped by provider name.
type GlobalStats struct {
	providers sync.Map // string -> *ProviderStats
}

// ResetAllCounters clears all counter fields for every provider, while preserving
// circuit breaker state such as ConsecutiveErrors and CircuitOpenUntil.
func (g *GlobalStats) ResetAllCounters() {
	g.providers.Range(func(key, value any) bool {
		ps := value.(*ProviderStats)
		atomic.StoreInt64(&ps.ActiveConnections, 0)
		atomic.StoreInt64(&ps.TotalRequests, 0)
		atomic.StoreInt64(&ps.SuccessRequests, 0)
		atomic.StoreInt64(&ps.ErrorRequests, 0)
		atomic.StoreInt64(&ps.TotalDurationMs, 0)
		atomic.StoreInt64(&ps.TotalBytes, 0)
		atomic.StoreInt64(&ps.PromptTokens, 0)
		atomic.StoreInt64(&ps.CompletionTokens, 0)
		ps.windowMu.Lock()
		if ps.HealthWindow != nil {
			ps.HealthWindow = newProviderHealthWindow(len(ps.HealthWindow.entries))
		}
		ps.windowMu.Unlock()
		return true
	})
}

// GetOrCreate returns the stats counter for a provider, thread-safe.
func (g *GlobalStats) GetOrCreate(provider string) *ProviderStats {
	if val, ok := g.providers.Load(provider); ok {
		return val.(*ProviderStats)
	}
	ps := &ProviderStats{}
	val, _ := g.providers.LoadOrStore(provider, ps)
	return val.(*ProviderStats)
}

// Snapshot returns a point-in-time copy of all provider stats for the dashboard.
func (g *GlobalStats) Snapshot() map[string]ProviderStatView {
	m := make(map[string]ProviderStatView)
	g.providers.Range(func(key, value any) bool {
		name := key.(string)
		ps := value.(*ProviderStats)

		total := atomic.LoadInt64(&ps.TotalRequests)
		success := atomic.LoadInt64(&ps.SuccessRequests)
		errs := atomic.LoadInt64(&ps.ErrorRequests)
		duration := atomic.LoadInt64(&ps.TotalDurationMs)
		bytes := atomic.LoadInt64(&ps.TotalBytes)
		active := atomic.LoadInt64(&ps.ActiveConnections)
		prompt := atomic.LoadInt64(&ps.PromptTokens)
		completion := atomic.LoadInt64(&ps.CompletionTokens)

		avg := int64(0)
		if total > 0 {
			avg = duration / total
		}

		openUntil := atomic.LoadInt64(&ps.CircuitOpenUntil)
		consecutiveErrors := atomic.LoadInt32(&ps.ConsecutiveErrors)

		m[name] = ProviderStatView{
			Name:              name,
			ActiveConnections: active,
			TotalRequests:     total,
			SuccessRequests:   success,
			ErrorRequests:     errs,
			AvgDurationMs:     avg,
			TotalBytes:        bytes,
			PromptTokens:      prompt,
			CompletionTokens:  completion,
			ConsecutiveErrors: consecutiveErrors,
			CircuitOpenUntil:  openUntil,
		}
		return true
	})
	return m
}
