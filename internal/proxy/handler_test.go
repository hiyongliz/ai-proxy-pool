package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
)

func TestProxyRouting(t *testing.T) {
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"provider": "a",
			"path":     r.URL.Path,
		})
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"provider": "b",
			"path":     r.URL.Path,
		})
	}))
	defer upstreamB.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
		},
		Router: config.RouterConfig{
			Strategy:          "round_robin",
			DefaultProvider:   "b",
			HeaderProviderKey: "X-AI-Provider",
			RouteRules: []config.RouteRule{
				{
					ModelPrefix: "claude-4",
					Providers:   []string{"a"},
				},
			},
		},
		Providers: []config.ProviderConfig{
			{
				Name:          "a",
				BaseURL:       upstreamA.URL,
				StaticHeaders: map[string]string{"anthropic-version": "2023-06-01"},
			},
			{
				Name:    "b",
				BaseURL: upstreamB.URL,
			},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	handler := server.Handler()

	t.Run("route by model rule", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-4-sonnet"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("X-Selected-Provider") != "a" {
			t.Fatalf("unexpected selected provider %q", rr.Header().Get("X-Selected-Provider"))
		}
		body, _ := io.ReadAll(rr.Body)
		if !bytes.Contains(body, []byte(`"provider":"a"`)) {
			t.Fatalf("unexpected upstream response %s", string(body))
		}
	})

	t.Run("route by forced header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-4-sonnet"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-AI-Provider", "b")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("X-Selected-Provider") != "b" {
			t.Fatalf("unexpected selected provider %q", rr.Header().Get("X-Selected-Provider"))
		}
		body, _ := io.ReadAll(rr.Body)
		if !bytes.Contains(body, []byte(`"provider":"b"`)) {
			t.Fatalf("unexpected upstream response %s", string(body))
		}
	})
}

func TestProxyAuthToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"ok": "true",
		})
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Auth: config.AuthConfig{
				Enabled: true,
				Header:  "Authorization",
				Scheme:  "Bearer",
				Token:   "proxy-secret-token",
			},
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: "p1",
		},
		Providers: []config.ProviderConfig{
			{
				Name:    "p1",
				BaseURL: upstream.URL,
			},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	handler := server.Handler()

	t.Run("missing token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-4-sonnet"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-4-sonnet"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer wrong-token")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("valid token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-4-sonnet"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer proxy-secret-token")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestProviderAuthTokenToUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-token" {
			http.Error(w, "invalid auth header", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: "p1",
		},
		Providers: []config.ProviderConfig{
			{
				Name:       "p1",
				BaseURL:    upstream.URL,
				APIKey:     "upstream-token",
				AuthType:   "auth_token",
				AuthHeader: "Authorization",
			},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-4-sonnet"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestModelMapping(t *testing.T) {
	var receivedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedModel = extractModel(body, r.Header.Get("Content-Type"))
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: "backup",
		},
		Providers: []config.ProviderConfig{
			{
				Name:    "backup",
				BaseURL: upstream.URL,
				ModelMapping: map[string]string{
					"claude-opus-4-6":  "claude-opus-4-5",
					"claude-sonnet-4-6": "claude-sonnet-4-5",
				},
			},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	handler := server.Handler()

	t.Run("mapped model is rewritten", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-4-6"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if receivedModel != "claude-opus-4-5" {
			t.Fatalf("expected upstream model %q, got %q", "claude-opus-4-5", receivedModel)
		}
	})

	t.Run("unmapped model is unchanged", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-haiku-4-5"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if receivedModel != "claude-haiku-4-5" {
			t.Fatalf("expected upstream model %q, got %q", "claude-haiku-4-5", receivedModel)
		}
	})
}

func boolPtr(v bool) *bool {
	return &v
}

func TestRetryOn5xx(t *testing.T) {
	var callCountA, callCountB atomic.Int32

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCountA.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer upstreamA.Close()

	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCountB.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstreamB.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Retry: config.RetryConfig{
				MaxAttempts:    3,
				RetryOn5xx:     boolPtr(true),
				RetryOnNetwork: boolPtr(true),
			},
		},
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "a", BaseURL: upstreamA.URL},
			{Name: "b", BaseURL: upstreamB.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// A 应该被调用 1 次（失败后排除），B 被调用 1 次（成功）
	if callCountA.Load() != 1 {
		t.Fatalf("expected provider A called 1 time, got %d", callCountA.Load())
	}
	if callCountB.Load() != 1 {
		t.Fatalf("expected provider B called 1 time, got %d", callCountB.Load())
	}
	if rr.Header().Get("X-Selected-Provider") != "b" {
		t.Fatalf("expected X-Selected-Provider=b, got %q", rr.Header().Get("X-Selected-Provider"))
	}
}

func TestRetryAllProvidersExhausted(t *testing.T) {
	var callCount atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Retry: config.RetryConfig{
				MaxAttempts: 3,
				RetryOn5xx:  boolPtr(true),
			},
		},
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "a", BaseURL: upstream.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	// 只有一个 provider，尝试一次后就用尽了，返回 502 Bad Gateway
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rr.Code, rr.Body.String())
	}
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", callCount.Load())
	}
}

func TestNoRetryWithForcedProvider(t *testing.T) {
	var callCount atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Retry: config.RetryConfig{
				MaxAttempts: 3,
				RetryOn5xx:  boolPtr(true),
			},
		},
		Router: config.RouterConfig{
			Strategy:          "round_robin",
			HeaderProviderKey: "X-AI-Provider",
		},
		Providers: []config.ProviderConfig{
			{Name: "a", BaseURL: upstream.URL},
			{Name: "b", BaseURL: upstream.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AI-Provider", "a") // 强制使用 a
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	// 强制指定 provider 时不重试
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 call (no retry with forced provider), got %d", callCount.Load())
	}
}

func TestRetryDisabled(t *testing.T) {
	var callCount atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
			Retry: config.RetryConfig{
				MaxAttempts:    3,
				RetryOn5xx:     boolPtr(false), // 禁用 5xx 重试
				RetryOnNetwork: boolPtr(false),
			},
		},
		Router: config.RouterConfig{
			Strategy: "round_robin",
		},
		Providers: []config.ProviderConfig{
			{Name: "a", BaseURL: upstream.URL},
			{Name: "b", BaseURL: upstream.URL},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	// 重试被禁用，应该只调用 1 次
	if callCount.Load() != 1 {
		t.Fatalf("expected 1 call (retry disabled), got %d", callCount.Load())
	}
}
