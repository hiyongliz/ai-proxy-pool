package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
)

func TestProxyRouting(t *testing.T) {
	stats := GetGlobalStats()
	promptBefore := stats.Snapshot()["codex"].PromptTokens
	completionBefore := stats.Snapshot()["codex"].CompletionTokens

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

		afterCountTokens := stats.Snapshot()["codex"]
		if got := afterCountTokens.PromptTokens - promptBefore; got != 42 {
			t.Fatalf("count_tokens should accumulate real prompt tokens, got delta=%d", got)
		}
		if got := afterCountTokens.CompletionTokens - completionBefore; got != 0 {
			t.Fatalf("count_tokens should not accumulate completion tokens, got delta=%d", got)
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
	t.Run("exact model mapping", func(t *testing.T) {
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
	})

	t.Run("regex model mapping fallback", func(t *testing.T) {
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
					ModelRegexMapping: []config.ModelRegexMapping{
						{
							Regex:       ".*",
							Replacement: "gpt-5.3-codex",
							Compiled:    regexp.MustCompile(".*"),
						},
					},
				},
			},
		}

		server, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("new server: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-4-6"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if receivedModel != "gpt-5.3-codex" {
			t.Fatalf("expected upstream model %q, got %q", "gpt-5.3-codex", receivedModel)
		}
	})

	t.Run("exact mapping wins over regex fallback", func(t *testing.T) {
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
						"claude-opus-4-6": "claude-opus-4-5",
					},
					ModelRegexMapping: []config.ModelRegexMapping{
						{
							Regex:       ".*",
							Replacement: "gpt-5.3-codex",
							Compiled:    regexp.MustCompile(".*"),
						},
					},
				},
			},
		}

		server, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("new server: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-4-6"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if receivedModel != "claude-opus-4-5" {
			t.Fatalf("expected upstream model %q, got %q", "claude-opus-4-5", receivedModel)
		}
	})
}
