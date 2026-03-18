package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
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

func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
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

func convertTools(root map[string]any, toolShortNameMap map[string]string) []any {
	toolsRaw := asSlice(root["tools"])
	if len(toolsRaw) == 0 {
		return nil
	}

	out := make([]any, 0, len(toolsRaw))
	for _, rawTool := range toolsRaw {
		tool := asMap(rawTool)
		if asString(tool["type"]) == "web_search_20250305" {
			out = append(out, map[string]any{"type": "web_search"})
			continue
		}

		name := asString(tool["name"])
		if name == "" {
			continue
		}
		if short, ok := toolShortNameMap[name]; ok {
			name = short
		} else {
			name = shortenToolNameIfNeeded(name)
		}

		fn := map[string]any{
			"type":       "function",
			"name":       name,
			"parameters": normalizeToolParameters(tool["input_schema"]),
			"strict":     false,
		}
		if desc := asString(tool["description"]); desc != "" {
			fn["description"] = desc
		}
		out = append(out, fn)
	}
	return out
}

func normalizeToolParameters(v any) map[string]any {
	schema := asMapCopy(v)
	if len(schema) == 0 {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	if asString(schema["type"]) == "" {
		schema["type"] = "object"
	}
	if asString(schema["type"]) == "object" {
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]any{}
		}
	}
	delete(schema, "$schema")
	return schema
}

func reasoningEffort(root map[string]any) string {
	effort := "medium"

	thinking := asMap(root["thinking"])
	if len(thinking) > 0 {
		switch strings.ToLower(asString(thinking["type"])) {
		case "disabled":
			effort = "minimal"
		case "adaptive", "auto":
			if outputCfg := asMap(root["output_config"]); len(outputCfg) > 0 {
				if v := strings.ToLower(strings.TrimSpace(asString(outputCfg["effort"]))); v != "" {
					return v
				}
			}
			effort = "high"
		case "enabled":
			budget := asInt64(thinking["budget_tokens"])
			switch {
			case budget <= 0:
				effort = "minimal"
			case budget < 2048:
				effort = "low"
			case budget < 8192:
				effort = "medium"
			default:
				effort = "high"
			}
		}
	}

	return effort
}

func normalizeReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "xhigh":
		return effort
	case "minimal", "none", "":
		return "low"
	default:
		return "low"
	}
}

func shouldIncludeReasoningSummary(root map[string]any) bool {
	thinking := asMap(root["thinking"])
	if len(thinking) == 0 {
		return false
	}

	switch strings.ToLower(asString(thinking["type"])) {
	case "enabled", "adaptive", "auto":
		return true
	default:
		return false
	}
}

func buildToolShortNameMap(root map[string]any) map[string]string {
	const toolNameLimit = 64
	names := make([]string, 0)
	for _, raw := range asSlice(root["tools"]) {
		name := asString(asMap(raw)["name"])
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return map[string]string{}
	}

	used := map[string]struct{}{}
	out := make(map[string]string, len(names))
	for _, name := range names {
		candidate := shortenToolNameToLimit(name, toolNameLimit)
		if _, ok := used[candidate]; ok {
			base := candidate
			for i := 1; ; i++ {
				suffix := "_" + strconv.Itoa(i)
				allowed := toolNameLimit - len(suffix)
				if allowed < 0 {
					allowed = 0
				}
				candidate = base
				if len(candidate) > allowed {
					candidate = candidate[:allowed]
				}
				candidate += suffix
				if _, exists := used[candidate]; !exists {
					break
				}
			}
		}
		used[candidate] = struct{}{}
		out[name] = candidate
	}
	return out
}

func shortenToolNameIfNeeded(name string) string {
	return shortenToolNameToLimit(name, 64)
}

func shortenToolNameToLimit(name string, limit int) string {
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		if idx := strings.LastIndex(name, "__"); idx > 0 && idx+2 < len(name) {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) <= limit {
				return candidate
			}
			return candidate[:limit]
		}
	}
	return name[:limit]
}

// asMap returns the map directly without copying. Safe for read-only access.
func asMap(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

// asMapCopy returns a shallow copy of the map. Use when the caller needs to mutate the result.
func asMapCopy(v any) map[string]any {
	m := asMap(v)
	if len(m) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		out[k] = val
	}
	return out
}

func asSlice(v any) []any {
	if v == nil {
		return nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	return a
}

func asString(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func asRawString(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}
