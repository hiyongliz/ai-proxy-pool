# Summary None Compatibility Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 默认请求不再发送 `reasoning.summary=none`，避免上游拒绝并恢复子代理能力。

**Architecture:** 在 Claude→Codex 请求转换中，仅在需要摘要时写入 `reasoning.summary`；默认仅保留 `reasoning.effort`，以最小变更兼容上游校验。

**Tech Stack:** Go, encoding/json, 现有 translator 层。

---

## 文件与职责

- 修改：`internal/translator/codex/claude/request.go` — 构造 Codex 请求 payload，控制 `reasoning.summary` 字段写入。
- 修改：`internal/translator/codex/claude/request_test.go` — 新增/更新单测验证 summary 字段存在性。

---

### Task 1: 调整默认 summary 字段行为

**Files:**
- Modify: `internal/translator/codex/claude/request_test.go`

- [ ] **Step 1: 写入失败测试（默认不含 summary）**

```go
func TestConvertClaudeRequestToCodex_DefaultReasoningSummaryOmitted(t *testing.T) {
    req := translator.TranslateRequest{
        Model:  "claude-3-5-sonnet",
        Path:   "/v1/messages",
        Body:   []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`),
        Stream: false,
    }

    got, err := ConvertClaudeRequestToCodex(req)
    if err != nil {
        t.Fatalf("unexpected err: %v", err)
    }

    var out map[string]any
    if err := json.Unmarshal(got.Body, &out); err != nil {
        t.Fatalf("unmarshal: %v", err)
    }

    reasoning := asMap(out["reasoning"])
    if _, ok := reasoning["summary"]; ok {
        t.Fatalf("expected reasoning.summary to be omitted")
    }
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/translator/codex/claude -run TestConvertClaudeRequestToCodex_DefaultReasoningSummaryOmitted -v`
Expected: FAIL（当前 summary=none 会导致测试失败）

- [ ] **Step 3: 修改实现以不写入 summary**

在 `internal/translator/codex/claude/request.go` 默认 `out["reasoning"]` 中移除 `summary` 字段：

```go
"reasoning": map[string]any{
    "effort": "minimal",
},
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/translator/codex/claude -run TestConvertClaudeRequestToCodex_DefaultReasoningSummaryOmitted -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/translator/codex/claude/request.go internal/translator/codex/claude/request_test.go
git commit -m "fix: omit default reasoning summary"
```

---

### Task 2: 保持启用 thinking 时 summary=auto

**Files:**
- Modify: `internal/translator/codex/claude/request_test.go`

- [ ] **Step 1: 写入/确认测试（thinking 启用时 summary=auto）**

```go
func TestConvertClaudeRequestToCodex_ThinkingEnablesSummaryAuto(t *testing.T) {
    req := translator.TranslateRequest{
        Model:  "claude-3-5-sonnet",
        Path:   "/v1/messages",
        Body:   []byte(`{"model":"claude-3-5-sonnet","thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`),
        Stream: false,
    }

    got, err := ConvertClaudeRequestToCodex(req)
    if err != nil {
        t.Fatalf("unexpected err: %v", err)
    }

    var out map[string]any
    if err := json.Unmarshal(got.Body, &out); err != nil {
        t.Fatalf("unmarshal: %v", err)
    }

    reasoning := asMap(out["reasoning"])
    if reasoning["summary"] != "auto" {
        t.Fatalf("expected reasoning.summary=auto, got %v", reasoning["summary"])
    }
}
```

- [ ] **Step 2: 运行测试确认通过**

Run: `go test ./internal/translator/codex/claude -run TestConvertClaudeRequestToCodex_ThinkingEnablesSummaryAuto -v`
Expected: PASS

- [ ] **Step 3: 提交（如需分提交）**

```bash
git add internal/translator/codex/claude/request_test.go
git commit -m "test: cover thinking summary auto"
```

---

## 验证

- [ ] 运行相关测试集：

```bash
go test ./internal/translator/codex/claude -v
```

**预期**：全部通过。

## 备注

- 若上游仍拒绝请求，再考虑是否必须显式写入 `summary=auto` 作为默认行为。