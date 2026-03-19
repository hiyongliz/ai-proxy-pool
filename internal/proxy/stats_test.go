package proxy

import (
	"sync/atomic"
	"testing"
	"time"
)

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
