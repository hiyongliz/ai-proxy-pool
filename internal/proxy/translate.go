package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hiyongliz/ai-proxy-pool/internal/config"
	"github.com/hiyongliz/ai-proxy-pool/internal/translator"
	codexclaude "github.com/hiyongliz/ai-proxy-pool/internal/translator/codex/claude"
)

func translateRequestForProvider(provider config.ProviderConfig, body []byte, model, path string) ([]byte, string, string, error) {
	if provider.RequestTranslate == "" || provider.RequestTranslate == config.TranslateNone {
		return body, path, model, nil
	}

	stream := requestStreamOrDefault(body, false)

	switch provider.RequestTranslate {
	case config.TranslateClaudeToCodex:
		out, ok, err := translator.Translate(
			translator.APIClaude,
			translator.APICodex,
			translator.TranslateRequest{
				Model:  model,
				Path:   path,
				Body:   body,
				Stream: stream,
			},
		)
		if err != nil {
			return nil, "", "", err
		}
		if !ok {
			return nil, "", "", fmt.Errorf("translator not registered for %s", provider.RequestTranslate)
		}

		nextBody := body
		if len(out.Body) > 0 {
			nextBody = out.Body
		}
		nextPath := path
		if out.Path != "" {
			nextPath = out.Path
		}
		nextModel := model
		if out.Model != "" {
			nextModel = out.Model
		} else if m := extractModel(nextBody, "application/json"); m != "" {
			nextModel = m
		}

		return nextBody, nextPath, nextModel, nil
	default:
		return nil, "", "", fmt.Errorf("unsupported request_translate %q", provider.RequestTranslate)
	}
}

func shouldTranslateResponse(provider config.ProviderConfig) bool {
	return provider.RequestTranslate == config.TranslateClaudeToCodex
}

func translateNonStreamResponseForProvider(provider config.ProviderConfig, originalPath string, originalRequestBody, raw []byte) ([]byte, error) {
	if !shouldTranslateResponse(provider) {
		return raw, nil
	}

	switch provider.RequestTranslate {
	case config.TranslateClaudeToCodex:
		if originalPath == "/v1/messages/count_tokens" {
			return codexclaude.ConvertCodexCountTokensToClaude(originalRequestBody, raw)
		}
		return codexclaude.ConvertCodexResponseToClaudeNonStream(originalRequestBody, raw)
	default:
		return nil, fmt.Errorf("unsupported response translate mode %q", provider.RequestTranslate)
	}
}

type tokenUsage struct {
	Input     int64
	Output    int64
	HasInput  bool
	HasOutput bool
}

func translateStreamResponseForProvider(provider config.ProviderConfig, originalRequestBody []byte, w io.Writer, r io.Reader) (int64, tokenUsage, error) {
	if !shouldTranslateResponse(provider) {
		n, err := io.Copy(w, r)
		return n, tokenUsage{}, err
	}

	switch provider.RequestTranslate {
	case config.TranslateClaudeToCodex:
		return translateCodexToClaudeStreamWithUsage(originalRequestBody, w, r)
	default:
		return 0, tokenUsage{}, fmt.Errorf("unsupported response translate mode %q", provider.RequestTranslate)
	}
}

func extractUsageFromCodexResponseBody(body []byte) tokenUsage {
	if len(body) == 0 {
		return tokenUsage{}
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return tokenUsage{}
	}

	response := root
	if typ, _ := root["type"].(string); typ == "response.completed" {
		if resp, ok := root["response"].(map[string]any); ok && len(resp) > 0 {
			response = resp
		}
	}

	usage, ok := response["usage"].(map[string]any)
	if !ok {
		return tokenUsage{}
	}

	out := tokenUsage{}
	if v, ok := asInt64FromAny(usage["input_tokens"]); ok {
		out.Input = v
		out.HasInput = true
	}
	if v, ok := asInt64FromAny(usage["output_tokens"]); ok {
		out.Output = v
		out.HasOutput = true
	}
	return out
}

func extractUsageFromResponseBody(path string, body []byte) tokenUsage {
	if len(body) == 0 {
		return tokenUsage{}
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return tokenUsage{}
	}

	if path == "/v1/messages/count_tokens" {
		v, ok := asInt64FromAny(root["input_tokens"])
		if !ok {
			return tokenUsage{}
		}
		return tokenUsage{Input: v, HasInput: true}
	}

	if usage := extractUsageFromCodexResponseBody(body); usage.HasInput || usage.HasOutput {
		return usage
	}

	usage, ok := root["usage"].(map[string]any)
	if !ok {
		return tokenUsage{}
	}

	out := tokenUsage{}
	if v, ok := asInt64FromAny(usage["input_tokens"]); ok {
		out.Input = v
		out.HasInput = true
	}
	if v, ok := asInt64FromAny(usage["output_tokens"]); ok {
		out.Output = v
		out.HasOutput = true
	}
	return out
}

func translateCodexToClaudeStreamWithUsage(originalRequestBody []byte, w io.Writer, r io.Reader) (int64, tokenUsage, error) {
	capture := &codexStreamUsageCapture{}
	tee := io.TeeReader(r, capture)

	written, err := codexclaude.ConvertCodexResponseToClaudeStream(originalRequestBody, w, tee)
	capture.Finish()
	return written, capture.Usage(), err
}

type codexStreamUsageCapture struct {
	lineBuf   bytes.Buffer
	dataLines []string
	usage     tokenUsage
}

func (c *codexStreamUsageCapture) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			line := strings.TrimRight(c.lineBuf.String(), "\r")
			c.lineBuf.Reset()
			c.consumeLine(line)
			continue
		}
		c.lineBuf.WriteByte(b)
	}
	return len(p), nil
}

func (c *codexStreamUsageCapture) Finish() {
	if c.lineBuf.Len() > 0 {
		line := strings.TrimRight(c.lineBuf.String(), "\r")
		c.lineBuf.Reset()
		c.consumeLine(line)
	}
	c.flushEvent()
}

func (c *codexStreamUsageCapture) Usage() tokenUsage {
	return c.usage
}

func (c *codexStreamUsageCapture) consumeLine(line string) {
	if strings.TrimSpace(line) == "" {
		c.flushEvent()
		return
	}
	if strings.HasPrefix(line, ":") {
		return
	}
	if rest, ok := strings.CutPrefix(line, "data:"); ok {
		c.dataLines = append(c.dataLines, strings.TrimSpace(rest))
	}
}

func (c *codexStreamUsageCapture) flushEvent() {
	if len(c.dataLines) == 0 {
		return
	}

	joined := strings.Join(c.dataLines, "\n")
	c.dataLines = c.dataLines[:0]
	if strings.TrimSpace(joined) == "" || strings.TrimSpace(joined) == "[DONE]" {
		return
	}

	var event map[string]any
	if err := json.Unmarshal([]byte(joined), &event); err != nil {
		return
	}
	if typ, _ := event["type"].(string); typ != "response.completed" {
		return
	}

	resp, _ := event["response"].(map[string]any)
	usageMap, _ := resp["usage"].(map[string]any)
	if v, ok := asInt64FromAny(usageMap["input_tokens"]); ok {
		c.usage.Input = v
		c.usage.HasInput = true
	}
	if v, ok := asInt64FromAny(usageMap["output_tokens"]); ok {
		c.usage.Output = v
		c.usage.HasOutput = true
	}
}

func asInt64FromAny(v any) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		return int64(t), true
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		var num json.Number = json.Number(s)
		n, err := num.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func requestStreamOrDefault(body []byte, def bool) bool {
	if len(body) == 0 {
		return def
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return def
	}
	v, ok := payload["stream"]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

