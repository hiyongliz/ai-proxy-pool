package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
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
	replaceForwardedAPIKeyHeader(outReq, provider)
	outReq.Host = mustHost(provider.BaseURL)
	addForwardHeaders(outReq, inReq)
	applyProviderAuth(outReq, provider)
	applyProviderStaticHeaders(outReq, provider)
	outReq.Header.Del(s.cfg.Router.HeaderProviderKey)

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

// mustHost extracts the host from a URL string, returning empty string on error.
func mustHost(rawURL string) string {
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

// replaceForwardedAPIKeyHeader handles the X-Api-Key header forwarding.
func replaceForwardedAPIKeyHeader(outReq *http.Request, provider config.ProviderConfig) {
	if outReq.Header.Get("X-Api-Key") == "" {
		return
	}
	if provider.APIKey == "" {
		outReq.Header.Del("X-Api-Key")
		return
	}
	outReq.Header.Set("X-Api-Key", provider.APIKey)
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
