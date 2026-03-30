package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/metrics"
)

// hopByHopHeaders are headers that should not be forwarded to upstream.
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

// buildUpstreamRequest constructs an HTTP request for the upstream provider.
func (s *Server) buildUpstreamRequest(inReq *http.Request, body []byte, provider config.ProviderConfig, upstreamPath string) (*http.Request, error) {
	targetPath := inReq.URL.Path
	if provider.UpstreamPath != "" {
		targetPath = provider.UpstreamPath
	}
	if upstreamPath != "" {
		targetPath = upstreamPath
	}

	targetURL, err := buildUpstreamURL(provider, targetPath, inReq.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("build upstream url: %w", err)
	}

	outReq, err := http.NewRequestWithContext(inReq.Context(), inReq.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new upstream request: %w", err)
	}

	copyRequestHeaders(outReq.Header, inReq.Header)

	// Prevent leaking proxy credentials to upstream
	outReq.Header.Del("Authorization")
	outReq.Header.Del("X-Api-Key")

	outReq.Host = hostFromURL(provider.BaseURL)
	addForwardHeaders(outReq, inReq)
	applyProviderAuth(outReq, provider)
	applyProviderStaticHeaders(outReq, provider)
	outReq.Header.Del(s.cfg.Router.HeaderProviderKey)
	outReq.Header.Del("Accept-Encoding")

	return outReq, nil
}

// buildUpstreamURL constructs the target URL for the upstream request.
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

// hostFromURL extracts the host from a URL string, returning empty string on error.
func hostFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// joinURLPath joins multiple path segments into a single URL path.
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

// addForwardHeaders adds X-Forwarded-* headers to the outgoing request.
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

// applyProviderAuth sets the appropriate authentication headers for the provider.
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
	case "bearer":
		outReq.Header.Set("Authorization", "Bearer "+provider.APIKey)
	case "auth_token", "auth-token":
		outReq.Header.Set("Authorization", "Bearer "+provider.APIKey)
		outReq.Header.Set("X-Api-Key", provider.APIKey)
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

// applyProviderStaticHeaders adds configured static headers to the request.
func applyProviderStaticHeaders(outReq *http.Request, provider config.ProviderConfig) {
	for k, v := range provider.StaticHeaders {
		outReq.Header.Set(k, v)
	}
}

// copyRequestHeaders copies headers from source to destination, excluding hop-by-hop headers.
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

// copyResponseHeaders copies response headers, excluding hop-by-hop headers.
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

// isNetworkError checks if the error is a network-related error.
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

func responseShouldStream(requestBody []byte, requestPath string, statusCode int, contentType string) bool {
	if requestPath == "/v1/messages/count_tokens" {
		return false
	}

	contentType = strings.ToLower(contentType)
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}

	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return false
	}

	return requestStreamOrDefault(requestBody, false)
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

	exactModelMapped := false
	if upstreamModel != "" && len(selected.ModelMapping) > 0 {
		if mapped, ok := selected.ModelMapping[upstreamModel]; ok {
			sourceModel := upstreamModel
			upstreamModel = mapped
			exactModelMapped = true
			modelLabel = modelMetricLabel(mapped)
			upstreamBody = replaceModel(upstreamBody, mapped)
			slog.Info("model mapped", "provider", selected.Name, "from", sourceModel, "to", mapped)
		}
	}
	if !exactModelMapped && upstreamModel != "" && len(selected.ModelRegexMapping) > 0 {
		mapped := applyModelRegexMapping(upstreamModel, selected.ModelRegexMapping)
		if mapped != upstreamModel {
			sourceModel := upstreamModel
			upstreamModel = mapped
			modelLabel = modelMetricLabel(mapped)
			upstreamBody = replaceModel(upstreamBody, mapped)
			slog.Info("model regex mapped", "provider", selected.Name, "from", sourceModel, "to", mapped)
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
		isStream := responseShouldStream(body, r.URL.Path, resp.StatusCode, resp.Header.Get("Content-Type"))

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
		isStream := responseShouldStream(body, r.URL.Path, resp.StatusCode, resp.Header.Get("Content-Type"))

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

			usage := extractUsageFromCodexResponseBody(rawBody)
			if !usage.HasInput && !usage.HasOutput {
				usage = extractUsageFromResponseBody(r.URL.Path, translatedBody)
			}
			if usage.HasInput {
				atomic.AddInt64(&pStats.PromptTokens, usage.Input)
			}
			if usage.HasOutput {
				atomic.AddInt64(&pStats.CompletionTokens, usage.Output)
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

	isStream := responseShouldStream(body, r.URL.Path, resp.StatusCode, resp.Header.Get("Content-Type"))

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
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Encoding")
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
