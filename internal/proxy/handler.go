package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/metrics"
	"github.com/hiyongliz/ai-proxy-pool/internal/router"
)

const (
	defaultMaxRequestBodyBytes = 8 * 1024 * 1024
	defaultUpstreamTimeout     = 300 * time.Second
)

// Server is an HTTP proxy that routes requests to configured providers.
type Server struct {
	cfg      config.Config
	selector *router.Selector
	clients  map[string]*http.Client
	stats    *GlobalStats
	handler  http.Handler
}

// NewServer creates a proxy server from configuration.
func NewServer(cfg config.Config) (*Server, error) {
	return NewServerWithStats(cfg, &GlobalStats{})
}

// NewServerWithStats creates a proxy server with the provided stats store.
func NewServerWithStats(cfg config.Config, stats *GlobalStats) (*Server, error) {
	if cfg.Server.MaxRequestBodyBytes <= 0 {
		cfg.Server.MaxRequestBodyBytes = defaultMaxRequestBodyBytes
	}
	if stats == nil {
		stats = &GlobalStats{}
	}

	selector, err := router.NewSelector(cfg.Router, cfg.Providers)
	if err != nil {
		return nil, fmt.Errorf("new selector: %w", err)
	}

	clients := map[string]*http.Client{}
	for _, provider := range cfg.Providers {
		if !provider.EnabledOrDefault() {
			continue
		}
		timeout := provider.Timeout
		if timeout == 0 {
			timeout = cfg.Server.UpstreamTimeout
		}
		if timeout == 0 {
			timeout = defaultUpstreamTimeout
		}
		clients[provider.Name] = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}

	server := &Server{
		cfg:      cfg,
		selector: selector,
		clients:  clients,
		stats:    stats,
	}
	server.handler = server.buildHandler()
	return server, nil
}

// Handler returns the HTTP handler with health, metrics, and proxy endpoints.
func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/api/internal/status", s.handleStatus) // 新增纯本地管理的私有接口
	mux.HandleFunc("/api/internal/status/reset", s.handleStatusReset)
	mux.HandleFunc("/", s.handleProxy)
	return s.loggingMiddleware(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if !s.isAuthorized(r) {
		if strings.EqualFold(s.cfg.Server.Auth.Scheme, "bearer") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ai-proxy-pool"`)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	body, err := readRequestBodyWithLimit(w, r, s.cfg.Server.MaxRequestBodyBytes)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": fmt.Sprintf("request body exceeds limit (%d bytes)", s.cfg.Server.MaxRequestBodyBytes),
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}

	model := extractModel(body, r.Header.Get("Content-Type"))
	forcedProvider := r.Header.Get(s.cfg.Router.HeaderProviderKey)

	retryCfg := s.cfg.Server.Retry
	maxAttempts := retryCfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	// 强制指定 provider 时不重试
	if forcedProvider != "" {
		maxAttempts = 1
	}

	cbCfg := s.cfg.Server.CircuitBreaker
	openDuration := cbCfg.OpenDuration

	var excludedProviders []string
	var lastErr error

	// 第一阶段：筛选目前正在全局熔断期的 Provider，直接将其强行计入 `excludedProviders`
	for _, p := range s.cfg.Providers {
		if stat := s.stats.GetOrCreate(p.Name); stat != nil {
			openUntil := atomic.LoadInt64(&stat.CircuitOpenUntil)
			if openUntil > 0 && time.Now().Unix() < openUntil {
				excludedProviders = append(excludedProviders, p.Name)
			}
		}
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		selected, err := s.selector.Select(router.SelectionInput{
			Path:              r.URL.Path,
			Model:             model,
			ForcedProvider:    forcedProvider,
			ExcludedProviders: excludedProviders,
		})
		if err != nil {
			if lastErr != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{
					"error": fmt.Sprintf("all providers exhausted, last error: %v", lastErr),
				})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}

		pStats := s.stats.GetOrCreate(selected.Name)
		pStats.EnsureHealthWindow(cbCfg.WindowSize)
		atomic.AddInt64(&pStats.ActiveConnections, 1)

		requestStart := time.Now()
		statusCode, upstreamErr := s.doUpstreamRequest(w, r, body, model, selected, attempt, maxAttempts)
		requestDuration := time.Since(requestStart)

		atomic.AddInt64(&pStats.ActiveConnections, -1)
		atomic.AddInt64(&pStats.TotalRequests, 1)

		fatal := isProviderFatalError(statusCode, upstreamErr)
		if pStats.HealthWindow != nil {
			pStats.HealthWindow.record(requestDuration, fatal)
			enough, samples, failureRate, avgDuration := pStats.HealthWindow.snapshot(cbCfg.MinSamples)
			if enough && openDuration > 0 && (failureRate >= cbCfg.FailureRateThreshold || avgDuration >= cbCfg.LatencyThreshold) {
				openUntil := time.Now().Add(openDuration).Unix()
				atomic.StoreInt64(&pStats.CircuitOpenUntil, openUntil)
				pStats.ResetHealthWindow(cbCfg.WindowSize)
				slog.Warn("provider score circuit breaker triggered",
					"provider", selected.Name,
					"sample_count", samples,
					"failure_rate", failureRate,
					"failure_rate_threshold", cbCfg.FailureRateThreshold,
					"avg_duration_ms", avgDuration.Milliseconds(),
					"latency_threshold", cbCfg.LatencyThreshold.String(),
					"open_duration", openDuration.String(),
				)
			}
		}

		// 保留连续错误计数字段仅做兼容状态，不再用于触发熔断。
		if fatal {
			atomic.AddInt32(&pStats.ConsecutiveErrors, 1)
		} else {
			atomic.StoreInt32(&pStats.ConsecutiveErrors, 0)
		}

		if upstreamErr == nil && isSuccessfulUpstreamStatus(statusCode) {
			atomic.AddInt64(&pStats.SuccessRequests, 1)
			return // 成功，直接返回
		}

		atomic.AddInt64(&pStats.ErrorRequests, 1)

		var writeErr responseWriteError
		if errors.As(upstreamErr, &writeErr) {
			// 响应已开始写入且发生写失败，无法重试也不能再写 502
			return
		}

		if r.Context().Err() != nil {
			// 客户端已主动断开，放弃重试
			return
		}

		lastErr = upstreamErr
		excludedProviders = append(excludedProviders, selected.Name)

		// 判断是否需要重试
		shouldRetry := false

		if isNetworkError(upstreamErr) && retryCfg.RetryOnNetworkOrDefault() {
			shouldRetry = true
			metrics.ProviderRetriesTotal.WithLabelValues(selected.Name, "network_error").Inc()
			slog.Warn("retrying due to network error",
				"provider", selected.Name,
				"attempt", attempt,
				"max_attempts", maxAttempts,
				"error", upstreamErr,
			)
		} else if statusCode >= 500 && retryCfg.RetryOn5xxOrDefault() {
			shouldRetry = true
			metrics.ProviderRetriesTotal.WithLabelValues(selected.Name, "5xx").Inc()
			slog.Warn("retrying due to 5xx response",
				"provider", selected.Name,
				"attempt", attempt,
				"max_attempts", maxAttempts,
				"status", statusCode,
			)
		}

		if !shouldRetry || attempt >= maxAttempts {
			break
		}
	}

	// 所有重试均失败，且最后一次不为 5xx (5xx 已在 doUpstreamRequest 中直接推流返回)
	writeJSON(w, http.StatusBadGateway, map[string]string{
		"error": fmt.Sprintf("upstream request failed after %d attempts: %v", maxAttempts, lastErr),
	})
}

func readRequestBodyWithLimit(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	limitedBody := http.MaxBytesReader(w, r.Body, maxBytes)
	defer limitedBody.Close()
	return io.ReadAll(limitedBody)
}

func writeJSON(w http.ResponseWriter, statusCode int, payload map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		slog.Error("writeJSON encode failed", "error", err)
	}
}

func isSuccessfulUpstreamStatus(statusCode int) bool {
	return statusCode >= http.StatusOK && statusCode < http.StatusBadRequest
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if requestID == "" {
			requestID = strings.TrimSpace(r.Header.Get("X-Request-ID"))
		}

		// 过滤内部状态查询的日志
		isInternalStatus := r.URL.Path == "/api/internal/status"

		if !isInternalStatus {
			slog.Info("http request",
				"phase", "start",
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", requestID,
				"remote_addr", r.RemoteAddr,
				"user_agent", r.UserAgent(),
			)
		}

		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		duration := time.Since(start)
		if !isInternalStatus {
			statusStr := strconv.Itoa(recorder.StatusCode())
			pathLabel := pathMetricLabel(r.URL.Path)
			metrics.HTTPRequestsTotal.WithLabelValues(r.Method, pathLabel, statusStr).Inc()
			metrics.HTTPRequestDuration.WithLabelValues(r.Method, pathLabel).Observe(duration.Seconds())
		}

		if !isInternalStatus {
			slog.Info("http request",
				"phase", "finish",
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", requestID,
				"status", recorder.StatusCode(),
				"duration_ms", duration.Milliseconds(),
				"response_bytes", recorder.bytesWritten,
				"selected_provider", recorder.Header().Get("X-Selected-Provider"),
			)
		}
	})
}

func pathMetricLabel(path string) string {
	switch path {
	case "/healthz":
		return "/healthz"
	case "/metrics":
		return "/metrics"
	case "/api/internal/status":
		return "/api/internal/status"
	default:
		return "/proxy"
	}
}

// statusRecorder wraps http.ResponseWriter to capture status code and bytes written.
type statusRecorder struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(data)
	r.bytesWritten += int64(n)
	return n, err
}

func (r *statusRecorder) StatusCode() int {
	if r.statusCode == 0 {
		return http.StatusOK
	}
	return r.statusCode
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (r *statusRecorder) Push(target string, opts *http.PushOptions) error {
	pusher, ok := r.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}
