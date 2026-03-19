package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
)

func TestHandleStatusForbiddenForNonLoopback(t *testing.T) {
	t.Parallel()

	s, err := NewServer(config.Config{
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: "https://example.com"},
		},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/internal/status", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleStatusAllowsLoopback(t *testing.T) {
	t.Parallel()

	s, err := NewServer(config.Config{
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: "https://example.com"},
		},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/internal/status", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleStatusResetClearsStatsForLoopback(t *testing.T) {
	t.Parallel()

	s, err := NewServer(config.Config{
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: "https://example.com"},
		},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ps := s.stats.GetOrCreate("p1")
	ps.TotalRequests = 10
	ps.SuccessRequests = 7
	ps.ErrorRequests = 3
	ps.TotalDurationMs = 1234
	ps.TotalBytes = 5678
	ps.PromptTokens = 111
	ps.CompletionTokens = 222
	ps.ConsecutiveErrors = 5
	ps.CircuitOpenUntil = 123456789

	req := httptest.NewRequest(http.MethodPost, "/api/internal/status/reset", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	if ps.TotalRequests != 0 || ps.SuccessRequests != 0 || ps.ErrorRequests != 0 ||
		ps.TotalDurationMs != 0 || ps.TotalBytes != 0 || ps.PromptTokens != 0 || ps.CompletionTokens != 0 {
		t.Fatalf("expected counters to be reset, got %+v", ps)
	}

	if ps.ConsecutiveErrors != 5 {
		t.Fatalf("expected ConsecutiveErrors to be preserved, got %d", ps.ConsecutiveErrors)
	}
	if ps.CircuitOpenUntil != 123456789 {
		t.Fatalf("expected CircuitOpenUntil to be preserved, got %d", ps.CircuitOpenUntil)
	}
}

func TestHandleStatusResetRejectsNonLoopback(t *testing.T) {
	t.Parallel()

	s, err := NewServer(config.Config{
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: "https://example.com"},
		},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/internal/status/reset", nil)
	req.RemoteAddr = "198.51.100.10:12345"
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleStatusResetRejectsNonPost(t *testing.T) {
	t.Parallel()

	s, err := NewServer(config.Config{
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: "https://example.com"},
		},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/internal/status/reset", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d body=%s", rr.Code, rr.Body.String())
	}
}
