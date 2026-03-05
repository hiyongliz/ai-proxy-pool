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

// Server is an HTTP proxy that routes requests to configured providers.
type Server struct {
	cfg      config.Config
	selector *router.Selector
	clients  map[string]*http.Client
	handler  http.Handler
}

// NewServer creates a proxy server from configuration.
func NewServer(cfg config.Config) (*Server, error) {
	if cfg.Server.MaxRequestBodyBytes <= 0 {
		cfg.Server.MaxRequestBodyBytes = 8 * 1024 * 1024
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
			timeout = 300 * time.Second
		}
		clients[provider.Name] = &http.Client{
			Timeout: timeout,
		}
	}

	server := &Server{
		cfg:      cfg,
		selector: selector,
		clients:  clients,
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

		pStats := defaultStats.GetOrCreate(selected.Name)
		atomic.AddInt64(&pStats.ActiveConnections, 1)

		statusCode, upstreamErr := s.doUpstreamRequest(w, r, body, model, selected, attempt, maxAttempts)

		atomic.AddInt64(&pStats.ActiveConnections, -1)
		atomic.AddInt64(&pStats.TotalRequests, 1)

		if upstreamErr == nil {
			atomic.AddInt64(&pStats.SuccessRequests, 1)
			return // 成功，直接返回
		}

		atomic.AddInt64(&pStats.ErrorRequests, 1)

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

// doUpstreamRequest 执行上游请求，返回 (HTTP状态码, 错误)。
// 如果成功写入响应，错误为 nil；否则返回错误供重试决策。
func (s *Server) doUpstreamRequest(w http.ResponseWriter, r *http.Request, body []byte, model string, selected config.ProviderConfig, attempt, maxAttempts int) (int, error) {
	pStats := defaultStats.GetOrCreate(selected.Name)

	// 模型映射
	upstreamModel := model
	modelLabel := modelMetricLabel(model)
	upstreamBody := body
	if model != "" && len(selected.ModelMapping) > 0 {
		if mapped, ok := selected.ModelMapping[model]; ok {
			upstreamModel = mapped
			modelLabel = modelMetricLabel(mapped)
			upstreamBody = replaceModel(body, mapped)
			slog.Info("model mapped", "provider", selected.Name, "from", model, "to", mapped)
		}
	}

	upstreamReq, err := s.buildUpstreamRequest(r, upstreamBody, selected)
	if err != nil {
		return 0, fmt.Errorf("build upstream request: %w", err)
	}

	// 简单解析请求体算一下 Input Token 估值 (按经验 1 token = 4 字节的粗略估算，如果 JSON 里有明确的 tokens 可提取更准)
	if len(body) > 0 {
		atomic.AddInt64(&pStats.PromptTokens, int64(len(body)/4))
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

	// 如果是 5xx 且需要重试，不写响应直接返回错误
	if resp.StatusCode >= 500 && attempt < maxAttempts && s.cfg.Server.Retry.RetryOn5xxOrDefault() {
		// 读取并丢弃响应体以允许连接复用
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("X-Selected-Provider", selected.Name)
	w.WriteHeader(resp.StatusCode)
	written, err := io.Copy(w, resp.Body)
	if err != nil {
		slog.Error("proxy write response failed",
			"provider", selected.Name,
			"status", resp.StatusCode,
			"error", err,
		)
		// 响应已部分写入，无法重试
		return resp.StatusCode, nil
	}

	atomic.AddInt64(&pStats.TotalBytes, written)
	if written > 0 {
		atomic.AddInt64(&pStats.CompletionTokens, written/4)
	}

	slog.Info("upstream response",
		"provider", selected.Name,
		"status", resp.StatusCode,
		"duration_ms", time.Since(upstreamStart).Milliseconds(),
		"response_bytes", written,
		"attempt", attempt,
	)

	return resp.StatusCode, nil
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
	_ = encoder.Encode(payload)
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
