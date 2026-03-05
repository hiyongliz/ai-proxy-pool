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
