package claude

import (
	"bufio"
	"fmt"
	"io"
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
		// keep as is
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
