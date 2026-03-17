package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

	upstreamCodex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unexpected path"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "resp_1",
			"type":  "response",
			"model": "gpt-5-codex",
			"usage": map[string]any{"input_tokens": 42, "output_tokens": 0},
			"output": []any{},
		})
	}))
	defer upstreamCodex.Close()

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
			{
				Name:             "codex",
				BaseURL:          upstreamCodex.URL,
				RequestTranslate: config.TranslateClaudeToCodex,
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

	t.Run("translate count_tokens", func(t *testing.T) {
		body := []byte(`{
			"model":"claude-4-sonnet",
			"stream":true,
			"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
		}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-AI-Provider", "codex")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("X-Selected-Provider") != "codex" {
			t.Fatalf("unexpected selected provider %q", rr.Header().Get("X-Selected-Provider"))
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if payload["input_tokens"] != float64(42) {
			t.Fatalf("unexpected input_tokens: %#v", payload["input_tokens"])
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

	t.Run("valid x-api-key token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-4-sonnet"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "proxy-secret-token")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestProxyAuthCompatibilityWithConfiguredXAPIKey(t *testing.T) {
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
				Header:  "X-Api-Key",
				Scheme:  "",
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

	t.Run("x-api-key accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-4-sonnet"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "proxy-secret-token")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("authorization bearer also accepted", func(t *testing.T) {
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
		if got := r.Header.Get("X-Api-Key"); got != "upstream-token" {
			http.Error(w, "invalid x-api-key header", http.StatusUnauthorized)
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
	req.Header.Set("X-Api-Key", "client-token")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout:     30 * time.Second,
			MaxRequestBodyBytes: 32,
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

	body := `{"model":"` + strings.Repeat("x", 100) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rr.Code, rr.Body.String())
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("expected upstream not called, got %d", upstreamCalls.Load())
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
					"claude-opus-4-6":   "claude-opus-4-5",
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

func TestClaudeToCodexRequestTranslate(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_1",
			"type":        "response",
			"model":       "gpt-5-codex",
			"stop_reason": "stop",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 4,
				"input_tokens_details": map[string]any{
					"cached_tokens": 3,
				},
			},
			"output": []any{
				map[string]any{
					"type": "message",
					"content": []any{
						map[string]any{"type": "output_text", "text": "hello"},
					},
				},
			},
		})
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: "codex-1",
		},
		Providers: []config.ProviderConfig{
			{
				Name:             "codex-1",
				BaseURL:          upstream.URL,
				TargetAPI:        "codex",
				RequestTranslate: "claude_to_codex",
				AuthType:         "none",
			},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{
		"model":"gpt-5-codex",
		"stream":false,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("unexpected upstream path: %q", gotPath)
	}
	if gotBody["messages"] != nil {
		t.Fatalf("expected translated body without messages, got=%v", gotBody["messages"])
	}
	if gotBody["input"] == nil {
		t.Fatalf("expected translated body with input, got=%v", gotBody)
	}

	var clientResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &clientResp); err != nil {
		t.Fatalf("decode client response: %v", err)
	}
	if clientResp["type"] != "message" {
		t.Fatalf("unexpected client response type: %v", clientResp["type"])
	}
	if clientResp["stop_reason"] != "stop" {
		t.Fatalf("unexpected stop_reason: %v", clientResp["stop_reason"])
	}
	usage := clientResp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(7) {
		t.Fatalf("unexpected input_tokens: %v", usage["input_tokens"])
	}
}

func TestClaudeToCodexResponseTranslateStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		stream := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5-codex"}}`,
			"",
			`data: {"type":"response.content_part.added"}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			"",
			`data: {"type":"response.content_part.done"}`,
			"",
			`data: {"type":"response.completed","response":{"stop_reason":"stop","usage":{"input_tokens":9,"output_tokens":5}}}`,
			"",
		}, "\n")
		_, _ = io.WriteString(w, stream)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: "codex-1",
		},
		Providers: []config.ProviderConfig{
			{
				Name:             "codex-1",
				BaseURL:          upstream.URL,
				TargetAPI:        "codex",
				RequestTranslate: "claude_to_codex",
				AuthType:         "none",
			},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{
		"model":"gpt-5-codex",
		"stream":true,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		`"type":"text_delta"`,
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected stream response to contain %q, got=%s", want, body)
		}
	}
}

func TestClaudeToCodexErrorResponsePassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_api_key","message":"bad key"}}`)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			UpstreamTimeout: 30 * time.Second,
		},
		Router: config.RouterConfig{
			Strategy:        "round_robin",
			DefaultProvider: "codex-1",
		},
		Providers: []config.ProviderConfig{
			{
				Name:             "codex-1",
				BaseURL:          upstream.URL,
				TargetAPI:        "codex",
				RequestTranslate: "claude_to_codex",
				AuthType:         "none",
			},
		},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{
		"model":"gpt-5-codex",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"type":"message"`) {
		t.Fatalf("error response should not be translated: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"invalid_api_key"`) {
		t.Fatalf("error response should be passed through: %s", rr.Body.String())
	}
}

func TestClaudeToCodexTranslateFailureReturnsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{`)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router: config.RouterConfig{Strategy: "round_robin", DefaultProvider: "codex-1"},
		Providers: []config.ProviderConfig{{
			Name:             "codex-1",
			BaseURL:          upstream.URL,
			TargetAPI:        "codex",
			RequestTranslate: "claude_to_codex",
			AuthType:         "none",
		}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{
		"model":"gpt-5-codex",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "upstream request failed") {
		t.Fatalf("expected upstream failure message, got %s", rr.Body.String())
	}
}

func TestUpstreamNon2xxLogsRequestAndResponse(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad"}`)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router: config.RouterConfig{Strategy: "round_robin", DefaultProvider: "p1"},
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: upstream.URL}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	body := `{"model":"test","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "bad") {
		t.Fatalf("expected upstream body passthrough, got=%s", rr.Body.String())
	}

	logs := logged.String()
	if !strings.Contains(logs, "upstream non-2xx response") {
		t.Fatalf("expected log marker, got=%s", logs)
	}
	if !strings.Contains(logs, strconv.Quote(body)) {
		t.Fatalf("expected request body in logs, got=%s", logs)
	}
	if !strings.Contains(logs, strconv.Quote(`{"error":"bad"}`)) {
		t.Fatalf("expected response body in logs, got=%s", logs)
	}
}

func TestUpstreamNon2xxStreamLogsWithoutReadingBody(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "data: partial")
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router: config.RouterConfig{Strategy: "round_robin", DefaultProvider: "p1"},
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: upstream.URL}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	body := `{"model":"test","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	logs := logged.String()
	if !strings.Contains(logs, "upstream non-2xx response") {
		t.Fatalf("expected log marker, got=%s", logs)
	}
	if strings.Contains(logs, "response_body") {
		t.Fatalf("did not expect response_body logged for stream, got=%s", logs)
	}
}
