package proxy

import (
	"encoding/json"
	"fmt"
	"io"

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

func translateStreamResponseForProvider(provider config.ProviderConfig, originalRequestBody []byte, w io.Writer, r io.Reader) (int64, error) {
	if !shouldTranslateResponse(provider) {
		n, err := io.Copy(w, r)
		return n, err
	}

	switch provider.RequestTranslate {
	case config.TranslateClaudeToCodex:
		return codexclaude.ConvertCodexResponseToClaudeStream(originalRequestBody, w, r)
	default:
		return 0, fmt.Errorf("unsupported response translate mode %q", provider.RequestTranslate)
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

