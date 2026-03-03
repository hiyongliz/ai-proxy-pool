package proxy

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/metrics"
	"github.com/hiyongliz/ai-proxy-pool/internal/router"
)

var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// Server is an HTTP proxy that routes requests to configured providers.
type Server struct {
	cfg      config.Config
	selector *router.Selector
	clients  map[string]*http.Client
}

// NewServer creates a proxy server from configuration.
func NewServer(cfg config.Config) (*Server, error) {
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

	return &Server{
		cfg:      cfg,
		selector: selector,
		clients:  clients,
	}, nil
}

// Handler returns the HTTP handler with health, metrics, and proxy endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.Handle("/metrics", promhttp.Handler())
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
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
	var lastStatusCode int

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

		statusCode, upstreamErr := s.doUpstreamRequest(w, r, body, model, selected, attempt, maxAttempts)
		if upstreamErr == nil {
			return // 成功，直接返回
		}

		lastErr = upstreamErr
		lastStatusCode = statusCode
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

	// 所有重试均失败
	if lastStatusCode >= 500 {
		writeJSON(w, lastStatusCode, map[string]string{
			"error": fmt.Sprintf("upstream returned %d after %d attempts", lastStatusCode, maxAttempts),
		})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{
		"error": fmt.Sprintf("upstream request failed after %d attempts: %v", maxAttempts, lastErr),
	})
}

// doUpstreamRequest 执行上游请求，返回 (HTTP状态码, 错误)。
// 如果成功写入响应，错误为 nil；否则返回错误供重试决策。
func (s *Server) doUpstreamRequest(w http.ResponseWriter, r *http.Request, body []byte, model string, selected config.ProviderConfig, attempt, maxAttempts int) (int, error) {
	// 模型映射
	upstreamModel := model
	upstreamBody := body
	if model != "" && len(selected.ModelMapping) > 0 {
		if mapped, ok := selected.ModelMapping[model]; ok {
			upstreamModel = mapped
			upstreamBody = replaceModel(body, mapped)
			slog.Info("model mapped", "provider", selected.Name, "from", model, "to", mapped)
		}
	}

	upstreamReq, err := s.buildUpstreamRequest(r, upstreamBody, selected)
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
		metrics.ProviderRequestsTotal.WithLabelValues(selected.Name, "error", upstreamModel).Inc()
		metrics.ProviderRequestDuration.WithLabelValues(selected.Name, upstreamModel).Observe(time.Since(upstreamStart).Seconds())
		return 0, err
	}
	defer resp.Body.Close()

	statusStr := strconv.Itoa(resp.StatusCode)
	metrics.ProviderRequestsTotal.WithLabelValues(selected.Name, statusStr, upstreamModel).Inc()
	metrics.ProviderRequestDuration.WithLabelValues(selected.Name, upstreamModel).Observe(time.Since(upstreamStart).Seconds())

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

	slog.Info("upstream response",
		"provider", selected.Name,
		"status", resp.StatusCode,
		"duration_ms", time.Since(upstreamStart).Milliseconds(),
		"response_bytes", written,
		"attempt", attempt,
	)

	return resp.StatusCode, nil
}

func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	// 检查常见网络错误类型
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// URL 错误通常包装了底层网络错误
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return false
}

func (s *Server) buildUpstreamRequest(inReq *http.Request, body []byte, provider config.ProviderConfig) (*http.Request, error) {
	targetURL, err := buildUpstreamURL(provider, inReq.URL.Path, inReq.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("build upstream url: %w", err)
	}

	outReq, err := http.NewRequestWithContext(inReq.Context(), inReq.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new upstream request: %w", err)
	}

	copyRequestHeaders(outReq.Header, inReq.Header)
	outReq.Host = mustHost(provider.BaseURL)
	addForwardHeaders(outReq, inReq)
	applyProviderAuth(outReq, provider)
	applyProviderStaticHeaders(outReq, provider)
	outReq.Header.Del(s.cfg.Router.HeaderProviderKey)

	return outReq, nil
}

func buildUpstreamURL(provider config.ProviderConfig, inPath, rawQuery string) (string, error) {
	base, err := url.Parse(provider.BaseURL)
	if err != nil {
		return "", fmt.Errorf("parse base url: %w", err)
	}

	pathPrefix := provider.PathPrefix
	if pathPrefix != "" && strings.HasPrefix(inPath, pathPrefix) {
		pathPrefix = ""
	}

	base.Path = joinURLPath(base.Path, pathPrefix, inPath)
	base.RawQuery = rawQuery
	return base.String(), nil
}

func mustHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func joinURLPath(parts ...string) string {
	out := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		trimmed := strings.Trim(part, "/")
		if trimmed == "" {
			continue
		}
		if out == "" {
			out = "/" + trimmed
			continue
		}
		out = strings.TrimRight(out, "/") + "/" + trimmed
	}
	if out == "" {
		return "/"
	}
	return out
}

func addForwardHeaders(outReq, inReq *http.Request) {
	outReq.Header.Set("X-Forwarded-Proto", "http")
	if inReq.TLS != nil {
		outReq.Header.Set("X-Forwarded-Proto", "https")
	}

	host := inReq.Host
	if host == "" {
		host = inReq.URL.Host
	}
	if host != "" {
		outReq.Header.Set("X-Forwarded-Host", host)
	}

	if ip, _, err := net.SplitHostPort(inReq.RemoteAddr); err == nil {
		if prior := outReq.Header.Get("X-Forwarded-For"); prior != "" {
			outReq.Header.Set("X-Forwarded-For", prior+", "+ip)
		} else {
			outReq.Header.Set("X-Forwarded-For", ip)
		}
	}
}

func applyProviderAuth(outReq *http.Request, provider config.ProviderConfig) {
	if provider.APIKey == "" {
		return
	}

	switch strings.ToLower(provider.AuthType) {
	case "", "x-api-key":
		headerName := provider.AuthHeader
		if headerName == "" {
			headerName = "x-api-key"
		}
		outReq.Header.Set(headerName, provider.APIKey)
	case "bearer", "auth_token", "auth-token":
		outReq.Header.Set("Authorization", "Bearer "+provider.APIKey)
	case "none":
		return
	default:
		headerName := provider.AuthHeader
		if headerName == "" {
			headerName = "Authorization"
		}
		outReq.Header.Set(headerName, provider.APIKey)
	}
}

func applyProviderStaticHeaders(outReq *http.Request, provider config.ProviderConfig) {
	for k, v := range provider.StaticHeaders {
		outReq.Header.Set(k, v)
	}
}

func copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := hopByHopHeaders[key]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := hopByHopHeaders[key]; skip {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func extractModel(body []byte, contentType string) string {
	if len(body) == 0 {
		return ""
	}
	if contentType != "" && !strings.Contains(strings.ToLower(contentType), "application/json") {
		return ""
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	modelValue, ok := payload["model"]
	if !ok {
		return ""
	}
	modelString, ok := modelValue.(string)
	if !ok {
		return ""
	}
	return modelString
}

func replaceModel(body []byte, to string) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	payload["model"] = to
	replaced, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return replaced
}

func writeJSON(w http.ResponseWriter, statusCode int, payload map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
}

func (s *Server) isAuthorized(r *http.Request) bool {
	authCfg := s.cfg.Server.Auth
	if !authCfg.Enabled {
		return true
	}

	headerValue := strings.TrimSpace(r.Header.Get(authCfg.Header))
	token := strings.TrimSpace(authCfg.Token)
	if token == "" || headerValue == "" {
		return false
	}

	scheme := strings.TrimSpace(authCfg.Scheme)
	if scheme == "" {
		return secureEqual(headerValue, token)
	}

	parts := strings.SplitN(headerValue, " ", 2)
	if len(parts) != 2 {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(parts[0]), scheme) {
		return false
	}

	return secureEqual(strings.TrimSpace(parts[1]), token)
}

func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if requestID == "" {
			requestID = strings.TrimSpace(r.Header.Get("X-Request-ID"))
		}
		slog.Info("http request",
			"phase", "start",
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", requestID,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		)

		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		duration := time.Since(start)
		statusStr := strconv.Itoa(recorder.StatusCode())

		metrics.HTTPRequestsTotal.WithLabelValues(r.Method, r.URL.Path, statusStr).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(r.Method, r.URL.Path).Observe(duration.Seconds())

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
	})
}

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
