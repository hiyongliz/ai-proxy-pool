package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hiyongliz/ai-proxy-pool/internal/translator"
)

// ConvertClaudeRequestToCodex converts a Claude-style request body into a Codex responses payload.
func ConvertClaudeRequestToCodex(req translator.TranslateRequest) (translator.TranslateResult, error) {
	root, err := decodeObject(req.Body)
	if err != nil {
		return translator.TranslateResult{}, fmt.Errorf("decode claude request: %w", err)
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = asString(root["model"])
	}
	if model == "" {
		return translator.TranslateResult{}, fmt.Errorf("missing model")
	}

	stream := req.Stream
	if v, ok := root["stream"].(bool); ok {
		stream = v
	}
	if req.Path == "/v1/messages/count_tokens" {
		stream = false
	}

	out := map[string]any{
		"model":               model,
		"instructions":        "",
		"input":               []any{},
		"parallel_tool_calls": true,
		"reasoning": map[string]any{
			"effort": normalizeReasoningEffort("minimal"),
		},
		"stream": stream,
		"store":  false,
	}
	if shouldIncludeReasoningSummary(root) {
		out["reasoning"] = map[string]any{
			"effort":  normalizeReasoningEffort(reasoningEffort(root)),
			"summary": "auto",
		}
		out["include"] = []string{"reasoning.encrypted_content"}
	}

	input := make([]any, 0, 8)
	toolShortNameMap := buildToolShortNameMap(root)
	appendSystemAsDeveloperMessage(root, &input)
	appendMessages(root, &input, toolShortNameMap)
	out["input"] = input

	if tools := convertTools(root, toolShortNameMap); len(tools) > 0 {
		out["tools"] = tools
		out["tool_choice"] = "auto"
	}
	if req.Path == "/v1/messages/count_tokens" {
		out["max_output_tokens"] = 0
	}

	body, err := json.Marshal(out)
	if err != nil {
		return translator.TranslateResult{}, fmt.Errorf("encode codex request: %w", err)
	}

	return translator.TranslateResult{
		Model: model,
		Path:  "/v1/responses",
		Body:  body,
	}, nil
}

func appendSystemAsDeveloperMessage(root map[string]any, input *[]any) {
	sys := root["system"]
	if sys == nil {
		return
	}

	content := make([]any, 0, 4)
	appendText := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "x-anthropic-billing-header: ") {
			return
		}
		content = append(content, map[string]any{
			"type": "input_text",
			"text": text,
		})
	}

	switch v := sys.(type) {
	case string:
		appendText(v)
	case []any:
		for _, item := range v {
			m := asMap(item)
			if asString(m["type"]) == "text" {
				appendText(asString(m["text"]))
			}
		}
	}

	if len(content) == 0 {
		return
	}
	*input = append(*input, map[string]any{
		"type":    "message",
		"role":    "developer",
		"content": content,
	})
}

func appendMessages(root map[string]any, input *[]any, toolShortNameMap map[string]string) {
	for _, rawMessage := range asSlice(root["messages"]) {
		msg := asMap(rawMessage)
		role := asString(msg["role"])
		if role == "" {
			continue
		}

		parts := make([]any, 0, 4)
		flush := func() {
			if len(parts) == 0 {
				return
			}
			*input = append(*input, map[string]any{
				"type":    "message",
				"role":    role,
				"content": parts,
			})
			parts = make([]any, 0, 2)
		}

		appendText := func(text string) {
			text = strings.TrimSpace(text)
			if text == "" {
				return
			}
			partType := "input_text"
			if role == "assistant" {
				partType = "output_text"
			}
			parts = append(parts, map[string]any{
				"type": partType,
				"text": text,
			})
		}

		appendImage := func(dataURL string) {
			if dataURL == "" {
				return
			}
			parts = append(parts, map[string]any{
				"type":      "input_image",
				"image_url": dataURL,
			})
		}

		content := msg["content"]
		switch v := content.(type) {
		case string:
			appendText(v)
		case []any:
			for _, rawPart := range v {
				part := asMap(rawPart)
				switch asString(part["type"]) {
				case "text":
					appendText(asString(part["text"]))
				case "image":
					appendImage(claudeImageToDataURL(part))
				case "tool_use":
					flush()
					arguments := normalizeFunctionArguments(part["input"])
					name := asString(part["name"])
					if short, ok := toolShortNameMap[name]; ok {
						name = short
					} else {
						name = shortenToolNameIfNeeded(name)
					}
					*input = append(*input, map[string]any{
						"type":      "function_call",
						"call_id":   asString(part["id"]),
						"name":      name,
						"arguments": arguments,
					})
				case "tool_result":
					flush()
					*input = append(*input, map[string]any{
						"type":    "function_call_output",
						"call_id": asString(part["tool_use_id"]),
						"output":  normalizeToolOutput(part["content"]),
					})
				}
			}
		}
		flush()
	}
}

func claudeImageToDataURL(part map[string]any) string {
	source := asMap(part["source"])
	if len(source) == 0 {
		return ""
	}
	data := asString(source["data"])
	if data == "" {
		data = asString(source["base64"])
	}
	if data == "" {
		return ""
	}

	mediaType := asString(source["media_type"])
	if mediaType == "" {
		mediaType = asString(source["mime_type"])
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return fmt.Sprintf("data:%s;base64,%s", mediaType, data)
}

func normalizeToolOutput(v any) any {
	switch raw := v.(type) {
	case string:
		return raw
	case []any:
		out := make([]any, 0, len(raw))
		for _, item := range raw {
			part := asMap(item)
			switch asString(part["type"]) {
			case "text":
				text := asString(part["text"])
				if text == "" {
					continue
				}
				out = append(out, map[string]any{
					"type": "input_text",
					"text": text,
				})
			case "image":
				dataURL := claudeImageToDataURL(part)
				if dataURL == "" {
					continue
				}
				out = append(out, map[string]any{
					"type":      "input_image",
					"image_url": dataURL,
				})
			}
		}
		if len(out) > 0 {
			return out
		}
		data, err := json.Marshal(raw)
		if err == nil {
			return string(data)
		}
		return fmt.Sprintf("%v", raw)
	case map[string]any:
		data, err := json.Marshal(raw)
		if err == nil {
			return string(data)
		}
		return fmt.Sprintf("%v", raw)
	}
	return ""
}

func normalizeFunctionArguments(v any) string {
	if v == nil {
		return "{}"
	}
	if s, ok := v.(string); ok {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return "{}"
		}
		return trimmed
	}
	data, err := json.Marshal(v)
	if err != nil || len(data) == 0 {
		return "{}"
	}
	return string(data)
}
