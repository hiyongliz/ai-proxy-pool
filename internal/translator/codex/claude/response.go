package claude

import (
	"encoding/json"
	"fmt"
	"strings"
)

// originalRequestInfo holds pre-parsed info from the original Claude request,
// avoiding redundant JSON decoding of the same body.
type originalRequestInfo struct {
	reverseToolNameMap map[string]string
	emitThinking       bool
}

func parseOriginalRequest(raw []byte) originalRequestInfo {
	root, err := decodeObject(raw)
	if err != nil {
		return originalRequestInfo{
			reverseToolNameMap: map[string]string{},
		}
	}
	shortMap := buildToolShortNameMap(root)
	rev := make(map[string]string, len(shortMap))
	for original, short := range shortMap {
		rev[short] = original
	}
	return originalRequestInfo{
		reverseToolNameMap: rev,
		emitThinking:       shouldIncludeReasoningSummary(root),
	}
}

// ConvertCodexResponseToClaudeNonStream converts a non-stream Codex response body to Claude message format.
func ConvertCodexResponseToClaudeNonStream(originalRequestRaw []byte, raw []byte) ([]byte, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("decode codex response: %w", err)
	}
	origInfo := parseOriginalRequest(originalRequestRaw)

	response := root
	if asString(root["type"]) == "response.completed" {
		if resp := asMap(root["response"]); len(resp) > 0 {
			response = resp
		}
	}

	out := map[string]any{
		"id":            asString(response["id"]),
		"type":          "message",
		"role":          "assistant",
		"model":         asString(response["model"]),
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	}

	inputTokens, outputTokens, cached := extractUsage(asMap(response["usage"]))
	outUsage := asMap(out["usage"])
	outUsage["input_tokens"] = inputTokens
	outUsage["output_tokens"] = outputTokens
	if cached > 0 {
		outUsage["cache_read_input_tokens"] = cached
	}
	out["usage"] = outUsage

	content := make([]any, 0, 6)
	hasToolCall := false
	trimLeadingDecorations := true

	for _, itemRaw := range asSlice(response["output"]) {
		item := asMap(itemRaw)
		switch asString(item["type"]) {
		case "reasoning":
			if !origInfo.emitThinking {
				continue
			}
			if text := collectReasoningText(item); text != "" {
				content = append(content, map[string]any{
					"type":     "thinking",
					"thinking": text,
				})
			}
		case "message":
			for _, partRaw := range asSlice(item["content"]) {
				part := asMap(partRaw)
				if asString(part["type"]) == "output_text" {
					text := asRawString(part["text"])
					if trimLeadingDecorations {
						cleaned, removed := removeLeadingCodexDecoration(text)
						text = cleaned
						if strings.TrimSpace(text) != "" {
							trimLeadingDecorations = false
						} else if !removed {
							trimLeadingDecorations = false
						}
					}
					if text == "" {
						continue
					}
					content = append(content, map[string]any{
						"type": "text",
						"text": text,
					})
				}
			}
		case "function_call":
			hasToolCall = true
			name := asString(item["name"])
			if originalName, ok := origInfo.reverseToolNameMap[name]; ok {
				name = originalName
			}
			toolBlock := map[string]any{
				"type":  "tool_use",
				"id":    asString(item["call_id"]),
				"name":  name,
				"input": map[string]any{},
			}
			if args := stripANSI(asString(item["arguments"])); args != "" {
				var obj map[string]any
				if json.Unmarshal([]byte(args), &obj) == nil && len(obj) > 0 {
					toolBlock["input"] = obj
				}
			}
			content = append(content, toolBlock)
		}
	}
	out["content"] = content

	stopReason := asString(response["stop_reason"])
	switch {
	case hasToolCall:
		out["stop_reason"] = "tool_use"
	case stopReason != "":
		out["stop_reason"] = stopReason
	default:
		out["stop_reason"] = "end_turn"
	}

	if seq, ok := response["stop_sequence"]; ok {
		out["stop_sequence"] = seq
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode claude response: %w", err)
	}
	return encoded, nil
}

// ConvertCodexCountTokensToClaude converts a Codex response to Claude count_tokens output.
func ConvertCodexCountTokensToClaude(_ []byte, raw []byte) ([]byte, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("decode codex response: %w", err)
	}
	response := root
	if asString(root["type"]) == "response.completed" {
		if resp := asMap(root["response"]); len(resp) > 0 {
			response = resp
		}
	}
	usage := asMap(response["usage"])
	if len(usage) == 0 {
		return nil, fmt.Errorf("missing usage")
	}
	inputTokens := asInt64(usage["input_tokens"])
	if inputTokens == 0 {
		if _, ok := usage["input_tokens"]; !ok {
			return nil, fmt.Errorf("missing usage.input_tokens")
		}
	}
	out := map[string]any{
		"input_tokens": inputTokens,
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode claude count_tokens response: %w", err)
	}
	return encoded, nil
}

func extractUsage(usage map[string]any) (int64, int64, int64) {
	if len(usage) == 0 {
		return 0, 0, 0
	}
	inputTokens := asInt64(usage["input_tokens"])
	outputTokens := asInt64(usage["output_tokens"])
	cachedTokens := int64(0)
	if details := asMap(usage["input_tokens_details"]); len(details) > 0 {
		cachedTokens = asInt64(details["cached_tokens"])
	}

	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}
	return inputTokens, outputTokens, cachedTokens
}

func collectReasoningText(item map[string]any) string {
	var parts []string
	appendText := func(v any) {
		text := asRawString(v)
		if text != "" {
			parts = append(parts, text)
		}
	}

	summary := item["summary"]
	switch v := summary.(type) {
	case []any:
		for _, p := range v {
			part := asMap(p)
			if text := asRawString(part["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	case string:
		appendText(v)
	}

	if len(parts) == 0 {
		for _, p := range asSlice(item["content"]) {
			part := asMap(p)
			if text := asRawString(part["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}

	return strings.Join(parts, "")
}

func restoreOriginalToolName(reverse map[string]string, current string) string {
	if original, ok := reverse[current]; ok {
		return original
	}
	return current
}

func removeLeadingCodexDecoration(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	idx := 0
	removed := false
	for idx < len(lines) {
		trimmed := strings.TrimSpace(lines[idx])
		if trimmed == "" {
			if removed {
				idx++
				continue
			}
			break
		}
		if isCodexDecorationLine(trimmed) {
			removed = true
			idx++
			continue
		}
		break
	}

	if !removed {
		return text, false
	}
	cleaned := strings.Join(lines[idx:], "\n")
	cleaned = strings.TrimLeft(cleaned, "\n")
	return cleaned, true
}

func isCodexDecorationLine(line string) bool {
	s := strings.ToLower(strings.TrimSpace(line))
	s = strings.TrimLeft(s, "*-• ")
	if strings.HasPrefix(s, "baked for ") {
		return true
	}
	if strings.HasPrefix(s, "read ") && strings.Contains(s, " files") && strings.Contains(s, "ctrl+o") {
		return true
	}
	return false
}
