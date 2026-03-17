package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type streamState struct {
	hasToolCall               bool
	blockIndex                int
	hasReceivedArgumentsDelta bool
	reverseToolNameMap        map[string]string
	emitThinking              bool
	leadingTextPending        bool
	bufferingFirstTextBlock   bool
	firstTextBlock            strings.Builder
}

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

// ConvertCodexResponseToClaudeStream converts Codex SSE stream to Claude SSE stream.
func ConvertCodexResponseToClaudeStream(originalRequestRaw []byte, w io.Writer, r io.Reader) (int64, error) {
	var total int64
	origInfo := parseOriginalRequest(originalRequestRaw)
	state := &streamState{
		reverseToolNameMap: origInfo.reverseToolNameMap,
		emitThinking:       origInfo.emitThinking,
		leadingTextPending: true,
	}
	type flusher interface {
		Flush()
	}
	f, _ := w.(flusher)
	writeChunk := func(s string) error {
		if s == "" {
			return nil
		}
		n, err := io.WriteString(w, s)
		total += int64(n)
		if err == nil && f != nil {
			f.Flush()
		}
		return err
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		rawData := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]

		if strings.TrimSpace(rawData) == "" || strings.TrimSpace(rawData) == "[DONE]" {
			return nil
		}

		chunk, err := convertSingleCodexEventToClaudeSSE([]byte(rawData), state)
		if err != nil {
			return err
		}
		return writeChunk(chunk)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if err := flush(); err != nil {
				return total, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			dataLines = append(dataLines, data)
		}
	}
	if err := scanner.Err(); err != nil {
		return total, err
	}
	if err := flush(); err != nil {
		return total, err
	}
	return total, nil
}

func convertSingleCodexEventToClaudeSSE(raw []byte, state *streamState) (string, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return "", fmt.Errorf("decode codex sse event: %w", err)
	}

	switch asString(root["type"]) {
	case "response.created":
		return handleResponseCreated(root)
	case "response.reasoning_summary_part.added":
		return handleReasoningSummaryPartAdded(state)
	case "response.reasoning_summary_text.delta":
		return handleReasoningSummaryTextDelta(root, state)
	case "response.reasoning_summary_part.done":
		return handleReasoningSummaryPartDone(state)
	case "response.content_part.added":
		return handleContentPartAdded(state)
	case "response.output_text.delta":
		return handleOutputTextDelta(root, state)
	case "response.content_part.done":
		return handleContentPartDone(state)
	case "response.output_item.added":
		return handleOutputItemAdded(root, state)
	case "response.function_call_arguments.delta":
		return handleFunctionCallArgsDelta(root, state)
	case "response.function_call_arguments.done":
		return handleFunctionCallArgsDone(root, state)
	case "response.output_item.done":
		return handleOutputItemDone(root, state)
	case "response.completed":
		return handleResponseCompleted(root, state)
	default:
		return "", nil
	}
}

func handleResponseCreated(root map[string]any) (string, error) {
	response := asMap(root["response"])
	payload := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            asString(response["id"]),
			"type":          "message",
			"role":          "assistant",
			"model":         asString(response["model"]),
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
			"content":     []any{},
			"stop_reason": nil,
		},
	}
	return sse("message_start", payload)
}

func handleReasoningSummaryPartAdded(state *streamState) (string, error) {
	if !state.emitThinking {
		return "", nil
	}
	payload := map[string]any{
		"type":  "content_block_start",
		"index": state.blockIndex,
		"content_block": map[string]any{
			"type":     "thinking",
			"thinking": "",
		},
	}
	return sse("content_block_start", payload)
}

func handleReasoningSummaryTextDelta(root map[string]any, state *streamState) (string, error) {
	if !state.emitThinking {
		return "", nil
	}
	payload := map[string]any{
		"type":  "content_block_delta",
		"index": state.blockIndex,
		"delta": map[string]any{
			"type":     "thinking_delta",
			"thinking": asRawString(root["delta"]),
		},
	}
	return sse("content_block_delta", payload)
}

func handleReasoningSummaryPartDone(state *streamState) (string, error) {
	if !state.emitThinking {
		return "", nil
	}
	payload := map[string]any{
		"type":  "content_block_stop",
		"index": state.blockIndex,
	}
	state.blockIndex++
	return sse("content_block_stop", payload)
}

func handleContentPartAdded(state *streamState) (string, error) {
	if state.leadingTextPending {
		state.bufferingFirstTextBlock = true
		state.firstTextBlock.Reset()
		return "", nil
	}
	payload := map[string]any{
		"type":  "content_block_start",
		"index": state.blockIndex,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	}
	return sse("content_block_start", payload)
}

func handleOutputTextDelta(root map[string]any, state *streamState) (string, error) {
	if state.bufferingFirstTextBlock {
		state.firstTextBlock.WriteString(asRawString(root["delta"]))
		return "", nil
	}
	payload := map[string]any{
		"type":  "content_block_delta",
		"index": state.blockIndex,
		"delta": map[string]any{
			"type": "text_delta",
			"text": asRawString(root["delta"]),
		},
	}
	return sse("content_block_delta", payload)
}

func handleContentPartDone(state *streamState) (string, error) {
	if state.bufferingFirstTextBlock {
		original := state.firstTextBlock.String()
		cleaned, removed := removeLeadingCodexDecoration(original)
		state.bufferingFirstTextBlock = false
		state.firstTextBlock.Reset()

		if strings.TrimSpace(cleaned) == "" {
			if !removed {
				state.leadingTextPending = false
			}
			return "", nil
		}
		state.leadingTextPending = false

		startPayload := map[string]any{
			"type":  "content_block_start",
			"index": state.blockIndex,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		}
		deltaPayload := map[string]any{
			"type":  "content_block_delta",
			"index": state.blockIndex,
			"delta": map[string]any{
				"type": "text_delta",
				"text": cleaned,
			},
		}
		stopPayload := map[string]any{
			"type":  "content_block_stop",
			"index": state.blockIndex,
		}

		a, err := sse("content_block_start", startPayload)
		if err != nil {
			return "", err
		}
		b, err := sse("content_block_delta", deltaPayload)
		if err != nil {
			return "", err
		}
		c, err := sse("content_block_stop", stopPayload)
		if err != nil {
			return "", err
		}
		state.blockIndex++
		return a + b + c, nil
	}
	payload := map[string]any{
		"type":  "content_block_stop",
		"index": state.blockIndex,
	}
	state.blockIndex++
	return sse("content_block_stop", payload)
}

func handleOutputItemAdded(root map[string]any, state *streamState) (string, error) {
	item := asMap(root["item"])
	if asString(item["type"]) != "function_call" {
		return "", nil
	}
	state.hasToolCall = true
	state.leadingTextPending = false
	state.hasReceivedArgumentsDelta = false

	startPayload := map[string]any{
		"type":  "content_block_start",
		"index": state.blockIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    asString(item["call_id"]),
			"name":  restoreOriginalToolName(state.reverseToolNameMap, asString(item["name"])),
			"input": map[string]any{},
		},
	}
	deltaPayload := map[string]any{
		"type":  "content_block_delta",
		"index": state.blockIndex,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": "",
		},
	}

	a, err := sse("content_block_start", startPayload)
	if err != nil {
		return "", err
	}
	b, err := sse("content_block_delta", deltaPayload)
	if err != nil {
		return "", err
	}
	return a + b, nil
}

func handleFunctionCallArgsDelta(root map[string]any, state *streamState) (string, error) {
	state.hasReceivedArgumentsDelta = true
	payload := map[string]any{
		"type":  "content_block_delta",
		"index": state.blockIndex,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": stripANSI(asRawString(root["delta"])),
		},
	}
	return sse("content_block_delta", payload)
}

func handleFunctionCallArgsDone(root map[string]any, state *streamState) (string, error) {
	if state.hasReceivedArgumentsDelta {
		return "", nil
	}
	args := stripANSI(asRawString(root["arguments"]))
	if args == "" {
		return "", nil
	}
	payload := map[string]any{
		"type":  "content_block_delta",
		"index": state.blockIndex,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": args,
		},
	}
	return sse("content_block_delta", payload)
}

func handleOutputItemDone(root map[string]any, state *streamState) (string, error) {
	item := asMap(root["item"])
	if asString(item["type"]) != "function_call" {
		return "", nil
	}
	payload := map[string]any{
		"type":  "content_block_stop",
		"index": state.blockIndex,
	}
	state.blockIndex++
	return sse("content_block_stop", payload)
}

func handleResponseCompleted(root map[string]any, state *streamState) (string, error) {
	response := asMap(root["response"])
	stopReason := asString(response["stop_reason"])
	switch {
	case state.hasToolCall:
		stopReason = "tool_use"
	case stopReason == "max_tokens" || stopReason == "stop":
	default:
		stopReason = "end_turn"
	}
	inputTokens, outputTokens, cached := extractUsage(asMap(response["usage"]))

	messageDelta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}
	if cached > 0 {
		usage := asMap(messageDelta["usage"])
		usage["cache_read_input_tokens"] = cached
		messageDelta["usage"] = usage
	}

	a, err := sse("message_delta", messageDelta)
	if err != nil {
		return "", err
	}
	b, err := sse("message_stop", map[string]any{"type": "message_stop"})
	if err != nil {
		return "", err
	}
	return a + b, nil
}

func sse(event string, payload map[string]any) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\n")
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return buf.String(), nil
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

// ansiPattern matches ANSI escape sequences:
//   - CSI sequences: ESC [ ... final_byte
//   - OSC sequences: ESC ] ... ST
//   - Bare ESC characters (0x1B)
var ansiPattern = regexp.MustCompile(`\x1b(?:\[[0-9;]*[a-zA-Z]|\][^\x07\x1b]*(?:\x07|\x1b\\))|\x1b`)

// stripANSI removes ANSI escape sequences and bare ESC characters from s.
func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}
