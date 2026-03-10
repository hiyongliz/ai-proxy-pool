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

	// 熔断器：连续致命错误计数器（遇顺畅包即归0）
	ConsecutiveErrors int32

	// 熔断器：熔断禁闭结束的时间戳(Unix 秒级)。如果当前时间 < 此值，拒绝路由
	CircuitOpenUntil int64
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
	CircuitOpenUntil  int64  `json:"circuit_open_until"`
}

// GlobalStats maintains statistics mapped by provider name.
type GlobalStats struct {
	providers sync.Map // string -> *ProviderStats
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
			CircuitOpenUntil:  openUntil,
		}
		return true
	})
	return m
}
