package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
)

func TestStatsTokenUseRealUsageForTranslatedNonStream(t *testing.T) {
	stats := GetGlobalStats()
	providerName := "codex-usage-nonstream"
	before := stats.Snapshot()[providerName]

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_1",
			"type":        "response",
			"model":       "gpt-5-codex",
			"stop_reason": "stop",
			"usage": map[string]any{
				"input_tokens":  11,
				"output_tokens": 7,
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
		Server: config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router: config.RouterConfig{Strategy: "round_robin", DefaultProvider: providerName},
		Providers: []config.ProviderConfig{{
			Name:             providerName,
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
		"stream":false,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	after := stats.Snapshot()[providerName]
	if got := after.PromptTokens - before.PromptTokens; got != 11 {
		t.Fatalf("prompt token delta mismatch: got=%d want=11", got)
	}
	if got := after.CompletionTokens - before.CompletionTokens; got != 7 {
		t.Fatalf("completion token delta mismatch: got=%d want=7", got)
	}
}

type streamProbeResponseWriter struct {
	header       http.Header
	statusCode   int
	buf          bytes.Buffer
	firstWriteCh chan struct{}
	notified     bool
}

func newStreamProbeResponseWriter() *streamProbeResponseWriter {
	return &streamProbeResponseWriter{
		header:       make(http.Header),
		statusCode:   http.StatusOK,
		firstWriteCh: make(chan struct{}, 1),
	}
}

func (w *streamProbeResponseWriter) Header() http.Header {
	return w.header
}

func (w *streamProbeResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *streamProbeResponseWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if !w.notified && n > 0 {
		w.notified = true
		select {
		case w.firstWriteCh <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (w *streamProbeResponseWriter) Flush() {}

func (w *streamProbeResponseWriter) StatusCode() int {
	return w.statusCode
}

func (w *streamProbeResponseWriter) BodyString() string {
	return w.buf.String()
}

func TestStatsTokenUseRealUsageForTranslatedStream(t *testing.T) {
	stats := GetGlobalStats()
	providerName := "codex-usage-stream"
	before := stats.Snapshot()[providerName]
	firstChunkSent := make(chan struct{}, 1)
	allowComplete := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\"}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case firstChunkSent <- struct{}{}:
		default:
		}
		<-allowComplete
		stream := strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			"",
			`data: {"type":"response.completed","response":{"stop_reason":"stop","usage":{"input_tokens":9,"output_tokens":5}}}`,
			"",
		}, "\n")
		_, _ = io.WriteString(w, stream)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router: config.RouterConfig{Strategy: "round_robin", DefaultProvider: providerName},
		Providers: []config.ProviderConfig{{
			Name:             providerName,
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
		"stream":true,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	probe := newStreamProbeResponseWriter()

	done := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(probe, req)
		close(done)
	}()

	select {
	case <-firstChunkSent:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting first upstream chunk")
	}

	select {
	case <-probe.firstWriteCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting first downstream write after upstream first chunk")
	}

	select {
	case <-done:
		t.Fatal("stream finished before completion event, looks buffered instead of streaming")
	default:
	}

	close(allowComplete)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting stream response finish")
	}

	if probe.StatusCode() != http.StatusOK {
		t.Fatalf("status=%d body=%s", probe.StatusCode(), probe.BodyString())
	}

	after := stats.Snapshot()[providerName]
	if got := after.PromptTokens - before.PromptTokens; got != 9 {
		t.Fatalf("prompt token delta mismatch: got=%d want=9", got)
	}
	if got := after.CompletionTokens - before.CompletionTokens; got != 5 {
		t.Fatalf("completion token delta mismatch: got=%d want=5", got)
	}
}

func TestStatsTokenMissingUsageDoesNotAccumulate(t *testing.T) {
	stats := GetGlobalStats()
	providerName := "codex-usage-missing"
	before := stats.Snapshot()[providerName]

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_1",
			"type":        "response",
			"model":       "gpt-5-codex",
			"stop_reason": "stop",
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
		Server: config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router: config.RouterConfig{Strategy: "round_robin", DefaultProvider: providerName},
		Providers: []config.ProviderConfig{{
			Name:             providerName,
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
		"stream":false,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	after := stats.Snapshot()[providerName]
	if got := after.PromptTokens - before.PromptTokens; got != 0 {
		t.Fatalf("prompt token should not change when usage missing, got delta=%d", got)
	}
	if got := after.CompletionTokens - before.CompletionTokens; got != 0 {
		t.Fatalf("completion token should not change when usage missing, got delta=%d", got)
	}
}

func TestStatsTokenParsesStringUsageValues(t *testing.T) {
	stats := GetGlobalStats()
	providerName := "codex-usage-string"
	before := stats.Snapshot()[providerName]

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_1",
			"type":        "response",
			"model":       "gpt-5-codex",
			"stop_reason": "stop",
			"usage": map[string]any{
				"input_tokens":  "12",
				"output_tokens": "3",
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
		Server: config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router: config.RouterConfig{Strategy: "round_robin", DefaultProvider: providerName},
		Providers: []config.ProviderConfig{{
			Name:             providerName,
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
		"stream":false,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	after := stats.Snapshot()[providerName]
	if got := after.PromptTokens - before.PromptTokens; got != 12 {
		t.Fatalf("prompt token delta mismatch for string usage: got=%d want=12", got)
	}
	if got := after.CompletionTokens - before.CompletionTokens; got != 3 {
		t.Fatalf("completion token delta mismatch for string usage: got=%d want=3", got)
	}
}

func TestStatsTokenMalformedUsageDoesNotPanicOrAccumulate(t *testing.T) {
	stats := GetGlobalStats()
	providerName := "codex-usage-malformed"
	before := stats.Snapshot()[providerName]

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_1",
			"type":        "response",
			"model":       "gpt-5-codex",
			"stop_reason": "stop",
			"usage":       "bad-usage-structure",
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
		Server: config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router: config.RouterConfig{Strategy: "round_robin", DefaultProvider: providerName},
		Providers: []config.ProviderConfig{{
			Name:             providerName,
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
		"stream":false,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	after := stats.Snapshot()[providerName]
	if got := after.PromptTokens - before.PromptTokens; got != 0 {
		t.Fatalf("prompt token should not change when usage malformed, got delta=%d", got)
	}
	if got := after.CompletionTokens - before.CompletionTokens; got != 0 {
		t.Fatalf("completion token should not change when usage malformed, got delta=%d", got)
	}
}

func TestStatsTokenParsesStringUsageValuesInStream(t *testing.T) {
	stats := GetGlobalStats()
	providerName := "codex-usage-stream-string"
	before := stats.Snapshot()[providerName]
	allowComplete := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5-codex\"}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-allowComplete
		stream := strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			"",
			`data: {"type":"response.completed","response":{"stop_reason":"stop","usage":{"input_tokens":"13","output_tokens":"8"}}}`,
			"",
		}, "\n")
		_, _ = io.WriteString(w, stream)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router: config.RouterConfig{Strategy: "round_robin", DefaultProvider: providerName},
		Providers: []config.ProviderConfig{{
			Name:             providerName,
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
		"stream":true,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	probe := newStreamProbeResponseWriter()

	done := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(probe, req)
		close(done)
	}()

	select {
	case <-probe.firstWriteCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting first downstream write")
	}

	close(allowComplete)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting stream response finish")
	}

	after := stats.Snapshot()[providerName]
	if got := after.PromptTokens - before.PromptTokens; got != 13 {
		t.Fatalf("prompt token delta mismatch for stream string usage: got=%d want=13", got)
	}
	if got := after.CompletionTokens - before.CompletionTokens; got != 8 {
		t.Fatalf("completion token delta mismatch for stream string usage: got=%d want=8", got)
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

func TestClaudeToCodexTranslatedResponseRemovesContentEncoding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "zstd")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_1",
			"type":        "response",
			"model":       "gpt-5-codex",
			"stop_reason": "stop",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 4,
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
		"stream":false,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("expected translated response to remove Content-Encoding, got %q", got)
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("expected application/json content type, got %q", got)
	}
}

func TestClaudeToCodexResponseTranslateStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "zstd")
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
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("expected translated stream response to remove Content-Encoding, got %q", got)
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected text/event-stream content type, got %q", got)
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

func TestClaudeToCodexNoContentSkipsBodyTranslation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "zstd")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusNoContent)
		_, _ = w.Write([]byte("{}"))
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
		"stream":false,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("expected empty body for 204 response, got %q", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("expected 204 translated response to remove Content-Encoding, got %q", got)
	}
	if got := rr.Header().Get("Content-Length"); got != "" {
		t.Fatalf("expected 204 translated response to remove Content-Length, got %q", got)
	}
}

func TestClaudeToCodexNotModifiedSkipsBodyTranslation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "zstd")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusNotModified)
		_, _ = w.Write([]byte("{}"))
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
		"stream":false,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotModified {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("expected empty body for 304 response, got %q", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("expected 304 translated response to remove Content-Encoding, got %q", got)
	}
	if got := rr.Header().Get("Content-Length"); got != "" {
		t.Fatalf("expected 304 translated response to remove Content-Length, got %q", got)
	}
}

func TestClaudeToCodexHeadResponseSkipsBodyTranslation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Fatalf("expected HEAD request, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "zstd")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
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

	req := httptest.NewRequest(http.MethodHead, "/v1/messages", bytes.NewBufferString(`{
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
	if rr.Body.Len() != 0 {
		t.Fatalf("expected empty body for HEAD response, got %q", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("expected HEAD translated response to remove Content-Encoding, got %q", got)
	}
	if got := rr.Header().Get("Content-Length"); got != "" {
		t.Fatalf("expected HEAD translated response to remove Content-Length, got %q", got)
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
	if !strings.Contains(rr.Body.String(), "\"invalid_api_key\"") {
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

func TestNonTranslatedResponseReadFailureReturnsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijacking")
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()

		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n")
		_, _ = buf.WriteString("Content-Type: application/json\r\n")
		_, _ = buf.WriteString("Content-Length: 20\r\n")
		_, _ = buf.WriteString("\r\n")
		_, _ = buf.WriteString(`{"partial":true}`)
		_ = buf.Flush()
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server:    config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router:    config.RouterConfig{Strategy: "round_robin", DefaultProvider: "p1"},
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: upstream.URL}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `{"partial":true}`) {
		t.Fatalf("expected truncated upstream body not to be passed through, got=%s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "upstream request failed") {
		t.Fatalf("expected upstream failure message, got=%s", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("expected local 502 json content type, got %q", got)
	}
	if got := rr.Header().Get("Content-Length"); got != "" {
		t.Fatalf("expected 502 to remove upstream Content-Length, got %q", got)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("expected 502 to remove upstream Content-Encoding, got %q", got)
	}
}

func TestNonTranslatedResponsePassthroughAfterSuccessfulRead(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "ok")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server:    config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router:    config.RouterConfig{Strategy: "round_robin", DefaultProvider: "p1"},
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: upstream.URL}},
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
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != `{"ok":true}` {
		t.Fatalf("expected passthrough body, got=%s", rr.Body.String())
	}
	if got := rr.Header().Get("X-Upstream"); got != "ok" {
		t.Fatalf("expected passthrough header, got %q", got)
	}
}

func TestTranslatedResponseLogsMinimalHeaders(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "zstd")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_1",
			"type":        "response",
			"model":       "gpt-5-codex",
			"stop_reason": "stop",
			"usage": map[string]any{
				"input_tokens":  1,
				"output_tokens": 1,
			},
			"output": []any{
				map[string]any{
					"type": "message",
					"content": []any{
						map[string]any{"type": "output_text", "text": "ok"},
					},
				},
			},
		})
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

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"gpt-5-codex","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, zstd")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	logs := logged.String()
	if !strings.Contains(logs, "response_header_summary") {
		t.Fatalf("expected response header summary log, got=%s", logs)
	}
	for _, want := range []string{"request_accept_encoding", "upstream_content_encoding", "upstream_content_type", "upstream_transfer_encoding", "upstream_content_length", "out_content_encoding", "out_content_type", "out_transfer_encoding", "out_content_length"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("expected %s in logs, got=%s", want, logs)
		}
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
		Server:    config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router:    config.RouterConfig{Strategy: "round_robin", DefaultProvider: "p1"},
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: upstream.URL}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	body := `{"model":"test","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, zstd")
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
	if strings.Contains(logs, body) {
		t.Fatalf("did not expect request body in logs, got=%s", logs)
	}
	if strings.Contains(logs, `{"error":"bad"}`) {
		t.Fatalf("did not expect response body in logs, got=%s", logs)
	}
	for _, want := range []string{"request_accept_encoding", "upstream_content_encoding", "upstream_content_type", "upstream_transfer_encoding", "upstream_content_length"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("expected %s in logs, got=%s", want, logs)
		}
	}
}

func TestNonTranslatedResponseLogsMinimalHeaders(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "ok")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server:    config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router:    config.RouterConfig{Strategy: "round_robin", DefaultProvider: "p1"},
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: upstream.URL}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, zstd")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	logs := logged.String()
	if !strings.Contains(logs, "response_header_summary") {
		t.Fatalf("expected response header summary log, got=%s", logs)
	}
	for _, want := range []string{"request_accept_encoding", "upstream_content_encoding", "upstream_content_type", "upstream_transfer_encoding", "upstream_content_length", "out_content_encoding", "out_content_type", "out_transfer_encoding", "out_content_length"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("expected %s in logs, got=%s", want, logs)
		}
	}
	if strings.Contains(logs, `{"ok":true}`) {
		t.Fatalf("did not expect response body in logs, got=%s", logs)
	}
}

func TestTrueStreamPassthroughSuccessDoesNotLogHeaderSummary(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"ok\":true}\n\n")
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server:    config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router:    config.RouterConfig{Strategy: "round_robin", DefaultProvider: "p1"},
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: upstream.URL}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"test","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, zstd")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `data: {"ok":true}`) {
		t.Fatalf("expected stream passthrough body, got=%s", rr.Body.String())
	}

	logs := logged.String()
	if strings.Contains(logs, "response_header_summary") {
		t.Fatalf("did not expect success diagnostic log for true stream, got=%s", logs)
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
		Server:    config.ServerConfig{UpstreamTimeout: 30 * time.Second},
		Router:    config.RouterConfig{Strategy: "round_robin", DefaultProvider: "p1"},
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: upstream.URL}},
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	body := `{"model":"test","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, zstd")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	logs := logged.String()
	if !strings.Contains(logs, "upstream non-2xx response") {
		t.Fatalf("expected log marker, got=%s", logs)
	}
	if strings.Contains(logs, body) {
		t.Fatalf("did not expect request body in logs, got=%s", logs)
	}
	if strings.Contains(logs, "response_body") {
		t.Fatalf("did not expect response_body logged for stream, got=%s", logs)
	}
	for _, want := range []string{"request_accept_encoding", "upstream_content_encoding", "upstream_content_type", "upstream_transfer_encoding", "upstream_content_length"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("expected %s in logs, got=%s", want, logs)
		}
	}
}
