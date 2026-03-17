package claude

import (
	"encoding/json"
	"testing"

	"github.com/hiyongliz/ai-proxy-pool/internal/translator"
)

func TestConvertClaudeRequestToCodexBasic(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"model":"claude-4-sonnet",
		"stream":false,
		"system":[{"type":"text","text":"you are helpful"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":[{"type":"text","text":"hi"}]}
		]
	}`)

	out, err := ConvertClaudeRequestToCodex(translator.TranslateRequest{
		Model:  "claude-4-sonnet",
		Path:   "/v1/messages",
		Body:   raw,
		Stream: true,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out.Path != "/v1/responses" {
		t.Fatalf("unexpected path: %q", out.Path)
	}

	var payload map[string]any
	if err := json.Unmarshal(out.Body, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if payload["model"] != "claude-4-sonnet" {
		t.Fatalf("unexpected model: %#v", payload["model"])
	}
	if payload["stream"] != false {
		t.Fatalf("unexpected stream: %#v", payload["stream"])
	}

	input, ok := payload["input"].([]any)
	if !ok || len(input) < 2 {
		t.Fatalf("unexpected input: %#v", payload["input"])
	}
}

func TestConvertClaudeRequestToCodexCountTokensForcesZeroOutput(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"model":"claude-4-sonnet",
		"stream":true,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)

	out, err := ConvertClaudeRequestToCodex(translator.TranslateRequest{
		Model:  "claude-4-sonnet",
		Path:   "/v1/messages/count_tokens",
		Body:   raw,
		Stream: true,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out.Path != "/v1/responses" {
		t.Fatalf("unexpected path: %q", out.Path)
	}

	var payload map[string]any
	if err := json.Unmarshal(out.Body, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if payload["stream"] != false {
		t.Fatalf("unexpected stream: %#v", payload["stream"])
	}
	maxTokens, ok := payload["max_output_tokens"]
	if !ok {
		t.Fatalf("missing max_output_tokens: %#v", payload)
	}
	if got := asTestInt64(maxTokens); got != 0 {
		t.Fatalf("unexpected max_output_tokens: %d", got)
	}
}

func asTestInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		val, _ := n.Int64()
		return val
	default:
		return -1
	}
}


func TestConvertClaudeRequestToCodexTools(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[{"role":"user","content":[{"type":"text","text":"search"}]}],
		"tools":[
			{"type":"web_search_20250305"},
			{"name":"lookup","description":"lookup tool","input_schema":{"type":"object"}}
		]
	}`)

	out, err := ConvertClaudeRequestToCodex(translator.TranslateRequest{
		Path: "/v1/messages",
		Body: raw,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out.Body, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("unexpected tools: %#v", payload["tools"])
	}

	first := tools[0].(map[string]any)
	if first["type"] != "web_search" {
		t.Fatalf("unexpected first tool: %#v", first)
	}

	second := tools[1].(map[string]any)
	if second["type"] != "function" || second["name"] != "lookup" {
		t.Fatalf("unexpected second tool: %#v", second)
	}
}

func TestConvertClaudeRequestToCodexToolNameShorteningAndMapping(t *testing.T) {
	t.Parallel()

	longA := "mcp__very_long_tool_namespace_a__this_is_a_very_long_function_name_a_abcdefghijklmnopqrstuvwxyz"
	longB := "mcp__very_long_tool_namespace_b__this_is_a_very_long_function_name_b_abcdefghijklmnopqrstuvwxyz"

	raw := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"` + longA + `","input":{"q":"x"}}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_2","name":"` + longB + `","input":{"q":"y"}}]}
		],
		"tools":[
			{"name":"` + longA + `","input_schema":{"type":"object"}},
			{"name":"` + longB + `","input_schema":{"type":"object"}}
		]
	}`)

	out, err := ConvertClaudeRequestToCodex(translator.TranslateRequest{
		Path: "/v1/messages",
		Body: raw,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out.Body, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}

	tools := payload["tools"].([]any)
	t0 := tools[0].(map[string]any)["name"].(string)
	t1 := tools[1].(map[string]any)["name"].(string)
	if len(t0) > 64 || len(t1) > 64 {
		t.Fatalf("tool names should be shortened: %q, %q", t0, t1)
	}
	if t0 == t1 {
		t.Fatalf("shortened tool names should be unique: %q, %q", t0, t1)
	}

	input := payload["input"].([]any)
	fn1 := input[0].(map[string]any)
	fn2 := input[1].(map[string]any)
	if fn1["name"] != t0 {
		t.Fatalf("tool_use name not mapped: got=%v want=%v", fn1["name"], t0)
	}
	if fn2["name"] != t1 {
		t.Fatalf("tool_use name not mapped: got=%v want=%v", fn2["name"], t1)
	}
}

func TestConvertClaudeRequestToCodexToolResultPreserveUnknownContent(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":{"foo":"bar","ok":true}}]}
		]
	}`)

	out, err := ConvertClaudeRequestToCodex(translator.TranslateRequest{
		Path: "/v1/messages",
		Body: raw,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out.Body, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}

	input := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("unexpected input len: %d", len(input))
	}
	output := input[0].(map[string]any)["output"]
	if output == "" {
		t.Fatalf("tool_result unknown content should be preserved")
	}
}

func TestConvertClaudeRequestToCodexFunctionCallArgumentsAreJSONString(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"abc","limit":2}}]}
		],
		"tools":[
			{"name":"lookup","input_schema":{"type":"object"}}
		]
	}`)

	out, err := ConvertClaudeRequestToCodex(translator.TranslateRequest{
		Path: "/v1/messages",
		Body: raw,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out.Body, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}

	input := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("unexpected input len: %d", len(input))
	}
	call := input[0].(map[string]any)
	args, ok := call["arguments"].(string)
	if !ok {
		t.Fatalf("arguments should be string, got=%T", call["arguments"])
	}

	var argsObj map[string]any
	if err := json.Unmarshal([]byte(args), &argsObj); err != nil {
		t.Fatalf("arguments should be valid json string: %v, raw=%q", err, args)
	}
	if argsObj["q"] != "abc" {
		t.Fatalf("unexpected arguments content: %#v", argsObj)
	}
}

func TestConvertClaudeRequestToCodexDefaultReasoningSummaryOmitted(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)

	out, err := ConvertClaudeRequestToCodex(translator.TranslateRequest{
		Path: "/v1/messages",
		Body: raw,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out.Body, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}

	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("missing reasoning: %#v", payload["reasoning"])
	}
	if _, ok := reasoning["summary"]; ok {
		t.Fatalf("expected reasoning.summary to be omitted")
	}
}

func TestConvertClaudeRequestToCodexNoThinkingDisablesReasoningSummary(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"model":"claude-4-sonnet",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)

	out, err := ConvertClaudeRequestToCodex(translator.TranslateRequest{
		Path: "/v1/messages",
		Body: raw,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out.Body, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}

	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("missing reasoning: %#v", payload["reasoning"])
	}
	if _, ok := reasoning["summary"]; ok {
		t.Fatalf("unexpected reasoning summary: %#v", reasoning["summary"])
	}
	if reasoning["effort"] != "minimal" {
		t.Fatalf("unexpected reasoning effort: %#v", reasoning["effort"])
	}
	if _, ok := payload["include"]; ok {
		t.Fatalf("reasoning include should be omitted when thinking disabled: %#v", payload["include"])
	}
}

func TestConvertClaudeRequestToCodexThinkingEnabledKeepsReasoningSummary(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"model":"claude-4-sonnet",
		"thinking":{"type":"enabled","budget_tokens":4096},
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)

	out, err := ConvertClaudeRequestToCodex(translator.TranslateRequest{
		Path: "/v1/messages",
		Body: raw,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(out.Body, &payload); err != nil {
		t.Fatalf("decode output: %v", err)
	}

	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("missing reasoning: %#v", payload["reasoning"])
	}
	if reasoning["summary"] != "auto" {
		t.Fatalf("unexpected reasoning summary: %#v", reasoning["summary"])
	}
	include, ok := payload["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("unexpected include: %#v", payload["include"])
	}
}
