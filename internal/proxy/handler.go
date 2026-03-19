package proxy

import (
	"bufio"
	"bytes"
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
	circuitBreakerThreshold    = 3
	circuitBreakerDurationSec  = 120
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
	if cfg.Server.MaxRequestBodyBytes <= 0 {
		cfg.Server.MaxRequestBodyBytes = defaultMaxRequestBodyBytes
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
		stats:    defaultStats,
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
		atomic.AddInt64(&pStats.ActiveConnections, 1)

		statusCode, upstreamErr := s.doUpstreamRequest(w, r, body, model, selected, attempt, maxAttempts)

		atomic.AddInt64(&pStats.ActiveConnections, -1)
		atomic.AddInt64(&pStats.TotalRequests, 1)

		if upstreamErr == nil {
			atomic.AddInt64(&pStats.SuccessRequests, 1)
			atomic.StoreInt32(&pStats.ConsecutiveErrors, 0)
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

		// 判断连贯失误是否触及致命级熔断
		if isProviderFatalError(statusCode, upstreamErr) {
			fails := atomic.AddInt32(&pStats.ConsecutiveErrors, 1)
			if fails >= circuitBreakerThreshold {
				// 触发熔断：隔离
				atomic.StoreInt64(&pStats.CircuitOpenUntil, time.Now().Unix()+circuitBreakerDurationSec)
				slog.Warn("provider circuit breaker triggered", "provider", selected.Name, "duration_sec", circuitBreakerDurationSec)
			}
		} else {
			// 非致命错误（比如 400 Bad Request），以及成功的 2xx/3xx，清零错误池
			atomic.StoreInt32(&pStats.ConsecutiveErrors, 0)
		}

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

// doUpstreamRequest 执行上游请求，返回 (HTTP状态码, 错误)。
// 如果成功写入响应，错误为 nil；否则返回错误供重试决策。
func (s *Server) doUpstreamRequest(w http.ResponseWriter, r *http.Request, body []byte, model string, selected config.ProviderConfig, attempt, maxAttempts int) (int, error) {
	pStats := s.stats.GetOrCreate(selected.Name)

	// 模型映射
	upstreamModel := model
	modelLabel := modelMetricLabel(model)
	upstreamBody := body
	upstreamPath := r.URL.Path

	translatedBody, translatedPath, translatedModel, err := translateRequestForProvider(selected, upstreamBody, upstreamModel, upstreamPath)
	if err != nil {
		return 0, fmt.Errorf("translate request for provider %q: %w", selected.Name, err)
	}
	upstreamBody = translatedBody
	upstreamPath = translatedPath
	upstreamModel = translatedModel
	modelLabel = modelMetricLabel(upstreamModel)

	if upstreamModel != "" && len(selected.ModelMapping) > 0 {
		if mapped, ok := selected.ModelMapping[upstreamModel]; ok {
			sourceModel := upstreamModel
			upstreamModel = mapped
			modelLabel = modelMetricLabel(mapped)
			upstreamBody = replaceModel(upstreamBody, mapped)
			slog.Info("model mapped", "provider", selected.Name, "from", sourceModel, "to", mapped)
		}
	}

	upstreamReq, err := s.buildUpstreamRequest(r, upstreamBody, selected, upstreamPath)
	if err != nil {
		return 0, fmt.Errorf("build upstream request: %w", err)
	}

	client, ok := s.clients[selected.Name]
	if !ok {
		return 0, fmt.Errorf("selected provider %q client missing", selected.Name)
	}

	upstreamStart := time.Now()
	slog.Info("upstream request",
		"provider", selected.Name,
		"method", upstreamReq.Method,
		"upstream_host", upstreamReq.URL.Host,
		"upstream_path", upstreamReq.URL.Path,
		"model", upstreamModel,
		"attempt", attempt,
		"max_attempts", maxAttempts,
	)

	resp, err := client.Do(upstreamReq)
	if err != nil {
		slog.Error("upstream request failed",
			"provider", selected.Name,
			"method", upstreamReq.Method,
			"upstream_host", upstreamReq.URL.Host,
			"upstream_path", upstreamReq.URL.Path,
			"duration_ms", time.Since(upstreamStart).Milliseconds(),
			"attempt", attempt,
			"error", err,
		)
		metrics.ProviderRequestsTotal.WithLabelValues(selected.Name, "error", modelLabel).Inc()
		metrics.ProviderRequestDuration.WithLabelValues(selected.Name, modelLabel).Observe(time.Since(upstreamStart).Seconds())
		return 0, err
	}
	defer resp.Body.Close()

	atomic.AddInt64(&pStats.TotalDurationMs, time.Since(upstreamStart).Milliseconds())

	statusStr := strconv.Itoa(resp.StatusCode)
	metrics.ProviderRequestsTotal.WithLabelValues(selected.Name, statusStr, modelLabel).Inc()
	metrics.ProviderRequestDuration.WithLabelValues(selected.Name, modelLabel).Observe(time.Since(upstreamStart).Seconds())

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		isStream := requestStreamOrDefault(body, false)
		if r.URL.Path == "/v1/messages/count_tokens" {
			isStream = false
		}
		if contentType := strings.ToLower(resp.Header.Get("Content-Type")); strings.Contains(contentType, "text/event-stream") {
			isStream = true
		}

		if !isStream {
			rawBody, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				attrs := []any{
					"provider", selected.Name,
					"status", resp.StatusCode,
					"method", upstreamReq.Method,
					"upstream_host", upstreamReq.URL.Host,
					"upstream_path", upstreamReq.URL.Path,
					"attempt", attempt,
					"max_attempts", maxAttempts,
					"read_error", readErr,
				}
				attrs = appendHeaderLogAttrs(attrs, "request_", r.Header)
				attrs = appendHeaderLogAttrs(attrs, "upstream_", resp.Header)
				slog.Warn("upstream non-2xx response", attrs...)
				return resp.StatusCode, fmt.Errorf("read upstream body: %w", readErr)
			}
			attrs := []any{
				"provider", selected.Name,
				"status", resp.StatusCode,
				"method", upstreamReq.Method,
				"upstream_host", upstreamReq.URL.Host,
				"upstream_path", upstreamReq.URL.Path,
				"attempt", attempt,
				"max_attempts", maxAttempts,
			}
			attrs = appendHeaderLogAttrs(attrs, "request_", r.Header)
			attrs = appendHeaderLogAttrs(attrs, "upstream_", resp.Header)
			slog.Warn("upstream non-2xx response", attrs...)
			resp.Body = io.NopCloser(bytes.NewReader(rawBody))
		} else {
			attrs := []any{
				"provider", selected.Name,
				"status", resp.StatusCode,
				"method", upstreamReq.Method,
				"upstream_host", upstreamReq.URL.Host,
				"upstream_path", upstreamReq.URL.Path,
				"attempt", attempt,
				"max_attempts", maxAttempts,
			}
			attrs = appendHeaderLogAttrs(attrs, "request_", r.Header)
			attrs = appendHeaderLogAttrs(attrs, "upstream_", resp.Header)
			slog.Warn("upstream non-2xx response", attrs...)
		}
	}

	// 如果是 5xx 且需要重试，不写响应直接返回错误
	if resp.StatusCode >= 500 && attempt < maxAttempts && s.cfg.Server.Retry.RetryOn5xxOrDefault() {
		// 读取并丢弃响应体以允许连接复用
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	if shouldTranslateResponse(selected) {
		copyResponseHeaders(w.Header(), resp.Header)
		if s.cfg.Server.ExposeProviderOrDefault() {
			w.Header().Set("X-Selected-Provider", selected.Name)
		}
		w.Header().Del("Content-Encoding")
		w.Header().Del("Content-Length")

		if r.Method == http.MethodHead || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
			w.WriteHeader(resp.StatusCode)
			s.recordUpstreamResponse(pStats, selected.Name, resp.StatusCode, upstreamStart, 0, attempt)
			return resp.StatusCode, nil
		}
	}

	if shouldTranslateResponse(selected) && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		isStream := requestStreamOrDefault(body, false)
		if r.URL.Path == "/v1/messages/count_tokens" {
			isStream = false
		}
		if contentType := strings.ToLower(resp.Header.Get("Content-Type")); strings.Contains(contentType, "text/event-stream") {
			isStream = true
		}

		var written int64
		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(resp.StatusCode)
			var tokenUsage tokenUsage
			written, tokenUsage, err = translateStreamResponseForProvider(selected, body, w, resp.Body)
			if tokenUsage.HasInput {
				atomic.AddInt64(&pStats.PromptTokens, tokenUsage.Input)
			}
			if tokenUsage.HasOutput {
				atomic.AddInt64(&pStats.CompletionTokens, tokenUsage.Output)
			}
		} else {
			rawBody, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				return resp.StatusCode, fmt.Errorf("read upstream body: %w", readErr)
			}
			translatedBody, translateErr := translateNonStreamResponseForProvider(selected, r.URL.Path, body, rawBody)
			if translateErr != nil {
				return resp.StatusCode, fmt.Errorf("translate upstream response: %w", translateErr)
			}

			tokenUsage := extractUsageFromCodexResponseBody(rawBody)
			if !tokenUsage.HasInput && !tokenUsage.HasOutput {
				tokenUsage = extractUsageFromResponseBody(r.URL.Path, translatedBody)
			}
			if tokenUsage.HasInput {
				atomic.AddInt64(&pStats.PromptTokens, tokenUsage.Input)
			}
			if tokenUsage.HasOutput {
				atomic.AddInt64(&pStats.CompletionTokens, tokenUsage.Output)
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			var n int
			n, err = w.Write(translatedBody)
			written = int64(n)
		}
		if err != nil {
			slog.Error("proxy write translated response failed",
				"provider", selected.Name,
				"status", resp.StatusCode,
				"error", err,
			)
			// 响应已部分写入，无法重试
			return resp.StatusCode, responseWriteError{err: err}
		}

		logResponseHeaderSummary(selected.Name, resp.StatusCode, r.Header, resp.Header, w.Header())
		s.recordUpstreamResponse(pStats, selected.Name, resp.StatusCode, upstreamStart, written, attempt)
		return resp.StatusCode, nil
	}

	copyResponseHeaders(w.Header(), resp.Header)
	if s.cfg.Server.ExposeProviderOrDefault() {
		w.Header().Set("X-Selected-Provider", selected.Name)
	}

	isStream := requestStreamOrDefault(body, false)
	if r.URL.Path == "/v1/messages/count_tokens" {
		isStream = false
	}
	if contentType := strings.ToLower(resp.Header.Get("Content-Type")); strings.Contains(contentType, "text/event-stream") {
		isStream = true
	}

	if r.Method == http.MethodHead || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		w.WriteHeader(resp.StatusCode)
		s.recordUpstreamResponse(pStats, selected.Name, resp.StatusCode, upstreamStart, 0, attempt)
		return resp.StatusCode, nil
	}

	if !isStream {
		rawBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			w.Header().Del("Content-Length")
			w.Header().Del("Content-Encoding")
			return resp.StatusCode, fmt.Errorf("read upstream body: %w", readErr)
		}
		w.WriteHeader(resp.StatusCode)
		var n int
		n, err = w.Write(rawBody)
		written := int64(n)
		if err != nil {
			slog.Error("proxy write response failed",
				"provider", selected.Name,
				"status", resp.StatusCode,
				"error", err,
			)
			return resp.StatusCode, responseWriteError{err: err}
		}

		logResponseHeaderSummary(selected.Name, resp.StatusCode, r.Header, resp.Header, w.Header())
		s.recordUpstreamResponse(pStats, selected.Name, resp.StatusCode, upstreamStart, written, attempt)
		return resp.StatusCode, nil
	}

	w.WriteHeader(resp.StatusCode)
	written, err := io.Copy(w, resp.Body)
	if err != nil {
		slog.Error("proxy write response failed",
			"provider", selected.Name,
			"status", resp.StatusCode,
			"error", err,
		)
		// 响应已部分写入，无法重试
		return resp.StatusCode, responseWriteError{err: err}
	}

	s.recordUpstreamResponse(pStats, selected.Name, resp.StatusCode, upstreamStart, written, attempt)

	return resp.StatusCode, nil
}

// recordUpstreamResponse updates stats and logs the upstream response.
func (s *Server) recordUpstreamResponse(pStats *ProviderStats, providerName string, statusCode int, start time.Time, written int64, attempt int) {
	atomic.AddInt64(&pStats.TotalBytes, written)

	slog.Info("upstream response",
		"provider", providerName,
		"status", statusCode,
		"duration_ms", time.Since(start).Milliseconds(),
		"response_bytes", written,
		"attempt", attempt,
	)
}

func appendHeaderLogAttrs(attrs []any, prefix string, h http.Header) []any {
	return append(attrs,
		prefix+"accept_encoding", h.Get("Accept-Encoding"),
		prefix+"content_encoding", h.Get("Content-Encoding"),
		prefix+"content_type", h.Get("Content-Type"),
		prefix+"transfer_encoding", h.Get("Transfer-Encoding"),
		prefix+"content_length", h.Get("Content-Length"),
	)
}

func logResponseHeaderSummary(providerName string, statusCode int, requestHeader, upstreamHeader, outHeader http.Header) {
	attrs := []any{
		"provider", providerName,
		"status", statusCode,
	}
	attrs = appendHeaderLogAttrs(attrs, "request_", requestHeader)
	attrs = appendHeaderLogAttrs(attrs, "upstream_", upstreamHeader)
	attrs = appendHeaderLogAttrs(attrs, "out_", outHeader)
	slog.Info("response_header_summary", attrs...)
}

type responseWriteError struct {
	err error
}

func (e responseWriteError) Error() string {
	if e.err == nil {
		return "response write failed"
	}
	return e.err.Error()
}

func (e responseWriteError) Unwrap() error {
	return e.err
}

func (e responseWriteError) Is(target error) bool {
	_, ok := target.(responseWriteError)
	return ok
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
