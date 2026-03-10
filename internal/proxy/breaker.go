package proxy

import (
	"errors"
	"net"
	"net/url"
	"syscall"
)

// isProviderFatalError evaluates if an error or HTTP status code represents a fatal
// issue with the provider's infrastructure or configuration, as opposed to a client-side error.
func isProviderFatalError(statusCode int, err error) bool {
	if err != nil {
		// Network errors are considered fatal (e.g., connection refused, timeout)
		var netErr net.Error
		if errors.As(err, &netErr) {
			return true
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return true
		}
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
			return true
		}
	}

	// 5xx errors are server-side failures on the provider's end
	if statusCode >= 500 {
		return true
	}

	// Specific 4xx errors indicating provider-level blocks or misconfigurations:
	// 401: Invalid API Key configured
	// 403: Blocked / Forbidden (e.g., geographical block or banned)
	// 429: Rate limited or Quota Exceeded
	if statusCode == 401 || statusCode == 403 || statusCode == 429 {
		return true
	}

	// Everything else (e.g., 400 Bad Request, 404 Not Found, 422 Unprocessable Entity)
	// is likely a client issue (bad prompt, invalid model name request, etc.)
	return false
}
