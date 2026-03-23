package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
)

func boolPtr(v bool) *bool {
	return &v
}

func TestRetryOn5xx(t *testing.T) {
	var callCountA, callCountB atomic.Int32

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCountA.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCountB.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstreamB.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Retry: config.RetryConfig{
				MaxAttempts:    3,
				RetryOn5xx:     boolPtr(true),
				RetryOnNetwork: boolPtr(true),
			},
		},
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "a", BaseURL: upstreamA.URL},
			{Name: "b", BaseURL: upstreamB.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// A 应该被调用 1 次（失败后排除），B 被调用 1 次（成功）
	if callCountA.Load() != 1 {
		t.Fatalf("expected provider A called 1 time, got %d", callCountA.Load())
	}
	if callCountB.Load() != 1 {
		t.Fatalf("expected provider B called 1 time, got %d", callCountB.Load())
	}
	if rr.Header().Get("X-Selected-Provider") != "b" {
		t.Fatalf("expected X-Selected-Provider=b, got %q", rr.Header().Get("X-Selected-Provider"))
	}

	stats := server.stats.Snapshot()
	if got := stats["a"].TotalRequests; got != 1 {
		t.Fatalf("provider A total requests: got %d want 1", got)
	}
	if got := stats["a"].ErrorRequests; got != 1 {
		t.Fatalf("provider A error requests: got %d want 1", got)
	}
	if got := stats["a"].SuccessRequests; got != 0 {
		t.Fatalf("provider A success requests: got %d want 0", got)
	}
	if got := stats["b"].TotalRequests; got != 1 {
		t.Fatalf("provider B total requests: got %d want 1", got)
	}
	if got := stats["b"].ErrorRequests; got != 0 {
		t.Fatalf("provider B error requests: got %d want 0", got)
	}
	if got := stats["b"].SuccessRequests; got != 1 {
		t.Fatalf("provider B success requests: got %d want 1", got)
	}
}

func TestPassthrough4xxCountsAsError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Retry: config.RetryConfig{
				MaxAttempts:    1,
				RetryOn5xx:     boolPtr(false),
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "a", BaseURL: upstream.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	stats := server.stats.Snapshot()
	if got := stats["a"].TotalRequests; got != 1 {
		t.Fatalf("provider total requests: got %d want 1", got)
	}
	if got := stats["a"].ErrorRequests; got != 1 {
		t.Fatalf("provider error requests: got %d want 1", got)
	}
	if got := stats["a"].SuccessRequests; got != 0 {
		t.Fatalf("provider success requests: got %d want 0", got)
	}
}

func TestPassthrough5xxCountsAsErrorWhenRetryDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Retry: config.RetryConfig{
				MaxAttempts:    1,
				RetryOn5xx:     boolPtr(false),
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "a", BaseURL: upstream.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rr.Code, rr.Body.String())
	}

	stats := server.stats.Snapshot()
	if got := stats["a"].TotalRequests; got != 1 {
		t.Fatalf("provider total requests: got %d want 1", got)
	}
	if got := stats["a"].ErrorRequests; got != 1 {
		t.Fatalf("provider error requests: got %d want 1", got)
	}
	if got := stats["a"].SuccessRequests; got != 0 {
		t.Fatalf("provider success requests: got %d want 0", got)
	}
}

func TestRetryAllProvidersExhausted(t *testing.T) {
	var callCount atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Retry: config.RetryConfig{
				MaxAttempts: 3,
				RetryOn5xx:  boolPtr(true),
			},
		},
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "a", BaseURL: upstream.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	// 只有一个 provider，尝试一次后就用尽了，返回 502 Bad Gateway
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rr.Code, rr.Body.String())
	}
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", callCount.Load())
	}
}

func TestNoRetryWithForcedProvider(t *testing.T) {
	var callCount atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Retry: config.RetryConfig{
				MaxAttempts: 3,
				RetryOn5xx:  boolPtr(true),
			},
		},
		Router: config.RouterConfig{
			Strategy:          "round_robin",
			HeaderProviderKey: "X-AI-Provider",
		},
		Providers: []config.ProviderConfig{
			{Name: "a", BaseURL: upstream.URL},
			{Name: "b", BaseURL: upstream.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AI-Provider", "a") // 强制使用 a
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	// 强制指定 provider 时不重试
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 call (no retry with forced provider), got %d", callCount.Load())
	}
}

func TestRetryDisabled(t *testing.T) {
	var callCount atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Retry: config.RetryConfig{
				MaxAttempts:    3,
				RetryOn5xx:     boolPtr(false), // 禁用 5xx 重试
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "a", BaseURL: upstream.URL},
			{Name: "b", BaseURL: upstream.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	// 重试被禁用，应该只调用 1 次
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 call (retry disabled), got %d", callCount.Load())
	}
}

func TestCircuitBreakerTripsWhenFailureRateTooHigh(t *testing.T) {
	providerName := "cb-score-failure"
	fallbackName := "cb-score-fallback"

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fatal error", http.StatusServiceUnavailable)
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstreamB.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			CircuitBreaker: config.CircuitBreakerConfig{
				Threshold:            99,
				OpenDuration:         60 * time.Second,
				WindowSize:           4,
				MinSamples:           4,
				FailureRateThreshold: 0.5,
				LatencyThreshold:     30 * time.Second,
			},
			Retry: config.RetryConfig{
				MaxAttempts:    1,
				RetryOn5xx:     boolPtr(false),
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: providerName,
		},
		Providers: []config.ProviderConfig{{
			Name:    providerName,
			BaseURL: upstreamA.URL,
		}, {
			Name:    fallbackName,
			BaseURL: upstreamB.URL,
		}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	stats := server.stats

	handler := server.Handler()
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}

	view := stats.Snapshot()[providerName]
	if view.CircuitOpenUntil == 0 {
		t.Fatalf("expected provider to trip on failure rate, got open_until=0")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Header().Get("X-Selected-Provider") != fallbackName {
		t.Fatalf("expected fallback provider after score breaker trip, got %q", rr.Header().Get("X-Selected-Provider"))
	}
}

func TestCircuitBreakerTripsWhenLatencyTooHigh(t *testing.T) {
	providerName := "cb-score-latency"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			CircuitBreaker: config.CircuitBreakerConfig{
				Threshold:            99,
				OpenDuration:         60 * time.Second,
				WindowSize:           3,
				MinSamples:           3,
				FailureRateThreshold: 1,
				LatencyThreshold:     10 * time.Millisecond,
			},
			Retry: config.RetryConfig{
				MaxAttempts:    1,
				RetryOn5xx:     boolPtr(false),
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: providerName,
		},
		Providers: []config.ProviderConfig{{
			Name:    providerName,
			BaseURL: upstream.URL,
		}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	stats := server.stats

	handler := server.Handler()
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}

	view := stats.Snapshot()[providerName]
	if view.CircuitOpenUntil == 0 {
		t.Fatalf("expected provider to trip on latency, got open_until=0")
	}
}

func TestCircuitBreakerDoesNotTripBeforeMinSamples(t *testing.T) {
	providerName := "cb-score-min-samples"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fatal error", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			CircuitBreaker: config.CircuitBreakerConfig{
				Threshold:            99,
				OpenDuration:         60 * time.Second,
				WindowSize:           4,
				MinSamples:           4,
				FailureRateThreshold: 0.25,
				LatencyThreshold:     30 * time.Second,
			},
			Retry: config.RetryConfig{
				MaxAttempts:    1,
				RetryOn5xx:     boolPtr(false),
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: providerName,
		},
		Providers: []config.ProviderConfig{{
			Name:    providerName,
			BaseURL: upstream.URL,
		}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	stats := server.stats

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	view := stats.Snapshot()[providerName]
	if view.CircuitOpenUntil != 0 {
		t.Fatalf("expected provider to stay closed before min samples, got %d", view.CircuitOpenUntil)
	}
}

func TestCircuitBreakerUsesConfiguredThreshold(t *testing.T) {
	providerName := "cb-config-threshold"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fatal error", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			CircuitBreaker: config.CircuitBreakerConfig{
				Threshold:            99,
				OpenDuration:         60 * time.Second,
				WindowSize:           2,
				MinSamples:           2,
				FailureRateThreshold: 1,
				LatencyThreshold:     30 * time.Second,
			},
			Retry: config.RetryConfig{
				MaxAttempts:    1,
				RetryOn5xx:     boolPtr(false),
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: providerName,
		},
		Providers: []config.ProviderConfig{{
			Name:    providerName,
			BaseURL: upstream.URL,
		}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	stats := server.stats

	handler := server.Handler()
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}

	view := stats.Snapshot()[providerName]
	if view.CircuitOpenUntil == 0 {
		t.Fatalf("expected circuit breaker to be open after reaching configured scoring threshold, got 0")
	}
	if view.CircuitOpenUntil <= time.Now().Unix() {
		t.Fatalf("expected circuit to remain open in the future, got %d", view.CircuitOpenUntil)
	}
}

func TestCircuitBreakerUsesConfiguredOpenDuration(t *testing.T) {
	providerName := "cb-config-open-duration"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fatal error", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			CircuitBreaker: config.CircuitBreakerConfig{
				Threshold:            99,
				OpenDuration:         10 * time.Second,
				WindowSize:           1,
				MinSamples:           1,
				FailureRateThreshold: 1,
				LatencyThreshold:     30 * time.Second,
			},
			Retry: config.RetryConfig{
				MaxAttempts:    1,
				RetryOn5xx:     boolPtr(false),
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: providerName,
		},
		Providers: []config.ProviderConfig{{
			Name:    providerName,
			BaseURL: upstream.URL,
		}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	stats := server.stats

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	view := stats.Snapshot()[providerName]
	if view.CircuitOpenUntil == 0 {
		t.Fatalf("expected circuit to open after scoring breaker trigger, got 0")
	}
	now := time.Now().Unix()
	delta := view.CircuitOpenUntil - now
	if delta < 8 || delta > 12 {
		t.Fatalf("expected circuit open duration around 10s, got delta=%d (open_until=%d now=%d)", delta, view.CircuitOpenUntil, now)
	}
}

func TestCircuitBreakerRecoveryDoesNotImmediatelyRetripFromStaleWindow(t *testing.T) {
	providerName := "cb-recovery"
	var shouldFail atomic.Bool
	shouldFail.Store(true)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shouldFail.Load() {
			http.Error(w, "fatal error", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			CircuitBreaker: config.CircuitBreakerConfig{
				Threshold:            99,
				OpenDuration:         1 * time.Second,
				WindowSize:           4,
				MinSamples:           4,
				FailureRateThreshold: 0.5,
				LatencyThreshold:     30 * time.Second,
			},
			Retry: config.RetryConfig{
				MaxAttempts:    1,
				RetryOn5xx:     boolPtr(false),
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: providerName,
		},
		Providers: []config.ProviderConfig{{
			Name:    providerName,
			BaseURL: upstream.URL,
		}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	stats := server.stats
	handler := server.Handler()

	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}

	view := stats.Snapshot()[providerName]
	if view.CircuitOpenUntil == 0 {
		t.Fatalf("expected provider to trip before recovery test")
	}

	shouldFail.Store(false)
	time.Sleep(1100 * time.Millisecond)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected recovery request to succeed, got status=%d body=%s", rr.Code, rr.Body.String())
	}

	view = stats.Snapshot()[providerName]
	if view.CircuitOpenUntil > time.Now().Unix() {
		t.Fatalf("expected provider to stay closed after recovery success, got open_until=%d", view.CircuitOpenUntil)
	}
}

func TestCircuitBreakerExcludesOpenProviderFromSelection(t *testing.T) {
	var callCountA, callCountB atomic.Int32

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCountA.Add(1)
		http.Error(w, "fatal error", http.StatusServiceUnavailable)
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCountB.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstreamB.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			CircuitBreaker: config.CircuitBreakerConfig{
				Threshold:    1,
				OpenDuration: 60 * time.Second,
			},
			Retry: config.RetryConfig{
				MaxAttempts:    1,
				RetryOn5xx:     boolPtr(false),
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "cb-a", BaseURL: upstreamA.URL},
			{Name: "cb-b", BaseURL: upstreamB.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	handler := server.Handler()

	// 第一次请求命中 cb-a，失败并触发熔断
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req1.Header.Set("Content-Type", "application/json")
	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, req1)

	// 第二次请求，应当绕过已熔断的 cb-a，路由到 cb-b
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)

	if callCountA.Load() != 1 {
		t.Fatalf("expected provider cb-a called once, got %d", callCountA.Load())
	}
	if callCountB.Load() != 1 {
		t.Fatalf("expected provider cb-b called once, got %d", callCountB.Load())
	}
	if rr2.Header().Get("X-Selected-Provider") != "cb-b" {
		t.Fatalf("expected second request to select cb-b, got %q", rr2.Header().Get("X-Selected-Provider"))
	}
}
