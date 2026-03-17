# 设计文档：Claude→Codex 请求 summary=none 兼容处理

日期：2026-03-17

## 目标与范围

**目标**：修复上游仅接受 `summary` 为 `concise|detailed|auto` 时导致的子代理请求失败问题，确保请求可被接受并成功返回。

**范围**：仅修改 Claude→Codex 请求转换层对 `reasoning.summary` 的构造逻辑；不改动上游、不新增配置、不影响非相关字段。

## 背景与问题

当前 `internal/translator/codex/claude/request.go` 在构造 Codex 请求时默认写入：

```json
"reasoning": {
  "effort": "minimal",
  "summary": "none"
}
```

上游明确不接受 `summary=none`，只接受 `concise|detailed|auto`，导致子代理请求被拒绝。

## 方案对比

1) **删除 summary 字段（推荐）**
- 优点：最小化请求字段；语义与“不需要摘要”一致；避免上游校验。
- 缺点：若上游未来要求 summary 必填，需再次适配（概率低）。

2) **将 none 改为 auto**
- 优点：简单、与上游支持值一致。
- 缺点：语义从“不需要摘要”变为“自动摘要”，可能产生额外输出或成本。

3) **配置化控制**
- 优点：灵活可控。
- 缺点：引入新配置面，改动更大，不符合本次最小修复目标。

## 设计与数据流

**核心策略**：
- 默认仅设置 `reasoning.effort`，**不写入** `reasoning.summary`。
- 当 `shouldIncludeReasoningSummary(root)` 为真时，显式写入 `summary: "auto"` 并保留现有 `include` 行为。

**数据流**：
Claude 请求 → `ConvertClaudeRequestToCodex` 解析 → 构建 `out` → 仅在需要摘要时写入 `summary` → JSON 编码 → 发送给上游。

## 错误处理

保持现有错误处理路径（JSON 解码/编码失败、缺失 model）。不新增额外错误分支。

## 测试策略

采用 TDD：
1. 先写测试：默认情况下输出 JSON 不包含 `reasoning.summary`。
2. 运行测试失败（红）。
3. 修改实现通过测试（绿）。

建议测试用例：
- **默认请求**：无 `thinking` 字段时，`reasoning.summary` 不存在。
- **启用 thinking**：`thinking.type` 为 `enabled/auto/adaptive` 时，`summary=auto` 保持不变。

## 潜在问题

1. 上游若要求 summary 必填（与当前报错相反），仍可能拒绝（概率低）。
2. 某些调用依赖 summary 字段（当前无外部依赖迹象）。

## 影响评估

- 行为变化仅限于请求 payload 字段精简。
- 不影响翻译逻辑主体与响应处理。
- 风险较低、可回滚。