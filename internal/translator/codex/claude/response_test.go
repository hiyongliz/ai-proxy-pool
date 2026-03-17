package claude

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestConvertCodexResponseToClaudeNonStream(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"tools":[
			{"name":"mcp__very_long_tool_namespace__lookup_customer_data_abcdefghijklmnopqrstuvwxyz"}
		]
	}`)

	raw := []byte(`{
		"id":"resp_1",
		"type":"response",
		"model":"gpt-5-codex",
		"stop_reason":"stop",
		"usage":{"input_tokens":10,"output_tokens":4,"input_tokens_details":{"cached_tokens":3}},
		"output":[
			{"type":"message","content":[{"type":"output_text","text":"hello"}]},
			{"type":"function_call","call_id":"call_1","name":"mcp__lookup_customer_data_abcdefghijklmnopqrstuvwxyz","arguments":"{\"q\":\"abc\"}"}
		]
	}`)

	out, err := ConvertCodexResponseToClaudeNonStream(originalReq, raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if payload["type"] != "message" {
		t.Fatalf("unexpected type: %#v", payload["type"])
	}
	if payload["model"] != "gpt-5-codex" {
		t.Fatalf("unexpected model: %#v", payload["model"])
	}
	if payload["stop_reason"] != "tool_use" {
		t.Fatalf("unexpected stop_reason: %#v", payload["stop_reason"])
	}

	usage := payload["usage"].(map[string]any)
	if usage["input_tokens"] != float64(7) {
		t.Fatalf("unexpected input_tokens: %#v", usage["input_tokens"])
	}

	content := payload["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("unexpected content length: %d", len(content))
	}
	toolUse := content[1].(map[string]any)
	if toolUse["name"] != "mcp__very_long_tool_namespace__lookup_customer_data_abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("tool name should be restored: %#v", toolUse["name"])
	}
}

func TestConvertCodexResponseToClaudeStream(t *testing.T) {
	t.Parallel()

	in := strings.Join([]string{
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

	var out bytes.Buffer
	if _, err := ConvertCodexResponseToClaudeStream(nil, &out, strings.NewReader(in)); err != nil {
		t.Fatalf("convert stream: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: message_delta",
		`"type":"text_delta"`,
		"event: message_stop",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected stream output to contain %q, got=%s", want, got)
		}
	}
}

func TestConvertCodexResponseToClaudeStreamRestoreToolName(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"tools":[
			{"name":"mcp__very_long_tool_namespace__lookup_customer_data_abcdefghijklmnopqrstuvwxyz"}
		]
	}`)
	in := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"mcp__lookup_customer_data_abcdefghijklmnopqrstuvwxyz"}}`,
		"",
		`data: {"type":"response.function_call_arguments.done","arguments":"{\"q\":\"x\"}"}`,
		"",
		`data: {"type":"response.output_item.done","item":{"type":"function_call"}}`,
		"",
	}, "\n")

	var out bytes.Buffer
	if _, err := ConvertCodexResponseToClaudeStream(originalReq, &out, strings.NewReader(in)); err != nil {
		t.Fatalf("convert stream: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"name":"mcp__very_long_tool_namespace__lookup_customer_data_abcdefghijklmnopqrstuvwxyz"`) {
		t.Fatalf("stream should restore original tool name, got=%s", got)
	}
}

func TestConvertCodexResponseToClaudeNonStreamSkipsReasoningWhenThinkingDisabled(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)

	raw := []byte(`{
		"id":"resp_1",
		"type":"response",
		"model":"gpt-5-codex",
		"stop_reason":"stop",
		"usage":{"input_tokens":10,"output_tokens":4},
		"output":[
			{"type":"reasoning","summary":[{"text":"internal reasoning"}]},
			{"type":"message","content":[{"type":"output_text","text":"final answer"}]}
		]
	}`)

	out, err := ConvertCodexResponseToClaudeNonStream(originalReq, raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}

	content := payload["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("unexpected content length: %d %#v", len(content), content)
	}
	part := content[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "final answer" {
		t.Fatalf("unexpected content part: %#v", part)
	}
}

func TestConvertCodexResponseToClaudeStreamSkipsReasoningWhenThinkingDisabled(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)

	in := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5-codex"}}`,
		"",
		`data: {"type":"response.reasoning_summary_part.added"}`,
		"",
		`data: {"type":"response.reasoning_summary_text.delta","delta":"internal reasoning"}`,
		"",
		`data: {"type":"response.reasoning_summary_part.done"}`,
		"",
		`data: {"type":"response.content_part.added"}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"final answer"}`,
		"",
		`data: {"type":"response.content_part.done"}`,
		"",
		`data: {"type":"response.completed","response":{"stop_reason":"stop","usage":{"input_tokens":9,"output_tokens":5}}}`,
		"",
	}, "\n")

	var out bytes.Buffer
	if _, err := ConvertCodexResponseToClaudeStream(originalReq, &out, strings.NewReader(in)); err != nil {
		t.Fatalf("convert stream: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "thinking_delta") {
		t.Fatalf("thinking blocks should be skipped when thinking disabled, got=%s", got)
	}
	if !strings.Contains(got, `"type":"text_delta"`) {
		t.Fatalf("text delta should still exist, got=%s", got)
	}
}

func TestConvertCodexResponseToClaudeNonStreamStripsLeadingCodexDecoration(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)

	raw := []byte(`{
		"id":"resp_1",
		"type":"response",
		"model":"gpt-5-codex",
		"stop_reason":"stop",
		"usage":{"input_tokens":10,"output_tokens":4},
		"output":[
			{"type":"message","content":[{"type":"output_text","text":"* Baked for 1m 31s\n\nfinal answer"}]}
		]
	}`)

	out, err := ConvertCodexResponseToClaudeNonStream(originalReq, raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	content := payload["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("unexpected content length: %d %#v", len(content), content)
	}
	part := content[0].(map[string]any)
	if got := part["text"]; got != "final answer" {
		t.Fatalf("unexpected text after stripping decoration: %#v", got)
	}
}

func TestConvertCodexResponseToClaudeStreamStripsLeadingCodexDecoration(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)

	in := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5-codex"}}`,
		"",
		`data: {"type":"response.content_part.added"}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"* Baked for 1m 31s\n\nfinal answer"}`,
		"",
		`data: {"type":"response.content_part.done"}`,
		"",
		`data: {"type":"response.completed","response":{"stop_reason":"stop","usage":{"input_tokens":9,"output_tokens":5}}}`,
		"",
	}, "\n")

	var out bytes.Buffer
	if _, err := ConvertCodexResponseToClaudeStream(originalReq, &out, strings.NewReader(in)); err != nil {
		t.Fatalf("convert stream: %v", err)
	}
	got := out.String()
	if strings.Contains(strings.ToLower(got), "baked for") {
		t.Fatalf("stream output should strip leading decoration, got=%s", got)
	}
	if !strings.Contains(got, `"text":"final answer"`) {
		t.Fatalf("stream output should preserve final answer, got=%s", got)
	}
}

func TestConvertCodexResponseToClaudeNonStreamPreserveMarkdownListNewline(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)

	raw := []byte(`{
		"id":"resp_1",
		"type":"response",
		"model":"gpt-5-codex",
		"stop_reason":"stop",
		"usage":{"input_tokens":10,"output_tokens":4},
		"output":[
			{"type":"message","content":[{"type":"output_text","text":"intro\n- one\n- two"}]}
		]
	}`)

	out, err := ConvertCodexResponseToClaudeNonStream(originalReq, raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	content := payload["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("unexpected content length: %d %#v", len(content), content)
	}
	part := content[0].(map[string]any)
	if got := part["text"]; got != "intro\n- one\n- two" {
		t.Fatalf("markdown list newline should be preserved, got=%#v", got)
	}
}

func TestConvertCodexResponseToClaudeStreamPreserveMarkdownListNewline(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)

	in := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5-codex"}}`,
		"",
		`data: {"type":"response.content_part.added"}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"intro\n"}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"- one\n- two"}`,
		"",
		`data: {"type":"response.content_part.done"}`,
		"",
		`data: {"type":"response.completed","response":{"stop_reason":"stop","usage":{"input_tokens":9,"output_tokens":5}}}`,
		"",
	}, "\n")

	var out bytes.Buffer
	if _, err := ConvertCodexResponseToClaudeStream(originalReq, &out, strings.NewReader(in)); err != nil {
		t.Fatalf("convert stream: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"text":"intro\n- one\n- two"`) {
		t.Fatalf("stream output should preserve markdown list newline, got=%s", got)
	}
}

func TestConvertCodexResponseToClaudeStreamPreserveToolArgumentsSpaces(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"model":"claude-4-sonnet",
		"tools":[{"name":"Bash"}],
		"messages":[{"role":"user","content":[{"type":"text","text":"run git summary"}]}]
	}`)

	in := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"Bash"}}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"cmd\":\"git "}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","delta":"status --short && git "}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","delta":"diff --stat && git diff --cached --stat\"}"}`,
		"",
		`data: {"type":"response.output_item.done","item":{"type":"function_call"}}`,
		"",
	}, "\n")

	var out bytes.Buffer
	if _, err := ConvertCodexResponseToClaudeStream(originalReq, &out, strings.NewReader(in)); err != nil {
		t.Fatalf("convert stream: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		`"partial_json":"{\"cmd\":\"git "`,
		`"partial_json":"status --short \u0026\u0026 git "`,
		`"partial_json":"diff --stat \u0026\u0026 git diff --cached --stat\"}"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("tool arguments chunk should preserve spaces, missing=%q got=%s", want, got)
		}
	}
	if strings.Contains(got, `gitstatus--short&&gitdiff--stat&&gitdiff--cached--stat`) {
		t.Fatalf("tool arguments should not collapse spaces, got=%s", got)
	}
}

func TestStripANSI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"bare ESC pair", "cd /path\x1b\x1b", "cd /path"},
		{"single ESC", "hello\x1bworld", "helloworld"},
		{"CSI color", "\x1b[32mgreen\x1b[0m", "green"},
		{"CSI bold", "\x1b[1mbold\x1b[22m", "bold"},
		{"mixed CSI and bare ESC", "\x1b[31mred\x1b[0m\x1b", "red"},
		{"no escape", "clean string", "clean string"},
		{"empty", "", ""},
		{"only ESC", "\x1b\x1b\x1b", ""},
		{"json with ESC", "{\"cmd\":\"cd /dir\x1b\x1b\"}", `{"cmd":"cd /dir"}`},
		{"OSC sequence", "\x1b]0;title\x07text", "text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := stripANSI(tt.input); got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestConvertCodexResponseToClaudeStreamStripsANSIFromToolArgs(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"model":"claude-4-sonnet",
		"tools":[{"name":"Bash"}],
		"messages":[{"role":"user","content":[{"type":"text","text":"run cd"}]}]
	}`)

	in := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"Bash"}}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"command\":\"cd /path/to/dir"}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","delta":"\"\u001b\u001b}"}`,
		"",
		`data: {"type":"response.output_item.done","item":{"type":"function_call"}}`,
		"",
	}, "\n")

	var out bytes.Buffer
	if _, err := ConvertCodexResponseToClaudeStream(originalReq, &out, strings.NewReader(in)); err != nil {
		t.Fatalf("convert stream: %v", err)
	}
	got := out.String()
	if strings.Contains(got, `\u001b`) {
		t.Fatalf("stream output should strip ANSI from tool args, got=%s", got)
	}
	if !strings.Contains(got, `"partial_json"`) {
		t.Fatalf("stream should contain partial_json deltas, got=%s", got)
	}
}

func TestConvertCodexResponseToClaudeNonStreamStripsANSIFromToolArgs(t *testing.T) {
	t.Parallel()

	originalReq := []byte(`{
		"tools":[{"name":"Bash"}]
	}`)

	raw := []byte(`{
		"id":"resp_1",
		"type":"response",
		"model":"gpt-5-codex",
		"stop_reason":"stop",
		"usage":{"input_tokens":10,"output_tokens":4},
		"output":[
			{"type":"function_call","call_id":"call_1","name":"Bash","arguments":"{\"command\":\"cd /path/to/dir\u001b\u001b\"}"}
		]
	}`)

	out, err := ConvertCodexResponseToClaudeNonStream(originalReq, raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	content := payload["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("unexpected content length: %d", len(content))
	}
	toolUse := content[0].(map[string]any)
	input := toolUse["input"].(map[string]any)
	cmd, ok := input["command"].(string)
	if !ok {
		t.Fatalf("command should be a string, got=%#v", input["command"])
	}
	if strings.Contains(cmd, "\x1b") {
		t.Fatalf("command should not contain ESC chars, got=%q", cmd)
	}
	if cmd != "cd /path/to/dir" {
		t.Fatalf("command should be clean path, got=%q", cmd)
	}
}
