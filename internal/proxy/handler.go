package proxy

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
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

// Handler returns the HTTP handler with health and proxy endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
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
	selected, err := s.selector.Select(router.SelectionInput{
		Path:           r.URL.Path,
		Model:          model,
		ForcedProvider: forcedProvider,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	upstreamReq, err := s.buildUpstreamRequest(r, body, selected)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	client, ok := s.clients[selected.Name]
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "selected provider client missing"})
		return
	}

	upstreamStart := time.Now()
	slog.Info("upstream request",
		"provider", selected.Name,
		"method", upstreamReq.Method,
		"upstream_host", upstreamReq.URL.Host,
		"upstream_path", upstreamReq.URL.Path,
		"model", model,
	)

	resp, err := client.Do(upstreamReq)
	if err != nil {
		slog.Error("upstream request failed",
			"provider", selected.Name,
			"method", upstreamReq.Method,
			"upstream_host", upstreamReq.URL.Host,
			"upstream_path", upstreamReq.URL.Path,
			"duration_ms", time.Since(upstreamStart).Milliseconds(),
			"error", err,
		)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("upstream request failed: %v", err)})
		return
	}
	defer resp.Body.Close()

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
		return
	}

	slog.Info("upstream response",
		"provider", selected.Name,
		"status", resp.StatusCode,
		"duration_ms", time.Since(upstreamStart).Milliseconds(),
		"response_bytes", written,
	)
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

		slog.Info("http request",
			"phase", "finish",
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", requestID,
			"status", recorder.StatusCode(),
			"duration_ms", time.Since(start).Milliseconds(),
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
