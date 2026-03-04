package proxy

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// isAuthorized checks if the request is authorized based on the server's auth configuration.
func (s *Server) isAuthorized(r *http.Request) bool {
	authCfg := s.cfg.Server.Auth
	if !authCfg.Enabled {
		return true
	}

	token := strings.TrimSpace(authCfg.Token)
	if token == "" {
		return false
	}

	// Primary mode: use configured header/scheme.
	if authHeaderMatches(r.Header.Get(authCfg.Header), authCfg.Scheme, token) {
		return true
	}

	// Compatibility mode: always accept common auth styles.
	if authHeaderMatches(r.Header.Get("Authorization"), "Bearer", token) {
		return true
	}
	if authHeaderMatches(r.Header.Get("X-Api-Key"), "", token) {
		return true
	}

	return false
}

// authHeaderMatches checks if the header value matches the expected scheme and token.
func authHeaderMatches(headerValue, scheme, token string) bool {
	headerValue = strings.TrimSpace(headerValue)
	if headerValue == "" {
		return false
	}

	scheme = strings.TrimSpace(scheme)
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

// secureEqual performs a constant-time comparison of two strings.
func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
