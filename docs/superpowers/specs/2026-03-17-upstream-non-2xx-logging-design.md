# 上游非 2xx 响应日志设计

## 目标
当上游响应状态码非 2xx 时，使用 `slog` 打印**原始客户端请求体**与**上游响应体**，便于排查问题；不改变现有重试/熔断逻辑与响应转发行为。

## 范围
- 位置：`internal/proxy/handler.go` 的 `Server.doUpstreamRequest`。
- 日志条件：`resp.StatusCode` 非 2xx（即不以 2 开头）。
- 日志内容：provider、status、method、upstream_host/path、attempt、原始客户端请求体、上游响应体（非流式时）。

## 方案
### 方案 A（采用）
在 `doUpstreamRequest` 中集中处理：
- 获取 `resp` 后判断 `StatusCode`。
- **非流式响应**：读取 `resp.Body` 到 `rawBody`，`slog.Warn` 记录请求体与响应体，然后继续使用 `rawBody` 走现有翻译/转发逻辑（必要时用 `io.NopCloser(bytes.NewReader(rawBody))` 复用）。
- **流式响应（SSE）**：不读取响应体，仅记录请求体与状态/头，避免阻塞与日志膨胀。

### 方案 B
将请求/响应体透传到 `recordUpstreamResponse` 再统一记录。调用链改动多，且流式响应难处理。

### 方案 C
在 `loggingMiddleware` 中记录。无法获得上游响应体，不满足需求。

## 数据流
1. `handleProxy` 读取原始 `body` → 传给 `doUpstreamRequest`。
2. `doUpstreamRequest` 发送上游请求 → 获取 `resp`。
3. 若 `resp.StatusCode` 非 2xx：
   - 非流式：读取 `rawBody`，记录日志后继续翻译/转发。
   - 流式：记录请求体与状态/头，不读取响应体。
4. 其余流程保持不变。

## 错误处理
- 读取响应体失败时，日志记录读取错误并按原逻辑返回错误。
- 不调整重试与熔断判断逻辑。

## 测试设计
在 `internal/proxy/handler_test.go` 新增/扩展测试：
1. 非 2xx 响应触发日志（通过替换 `slog` handler 或捕获日志缓冲区断言）。
2. 非 2xx 且非流式响应时仍正确转发响应。
3. 流式响应（SSE）非 2xx 不读取全量 body。

## 边界用例
- 204 No Content：记录请求体，响应体为空。
- 302/307：记录响应体（若存在）。
- 400/500：记录请求与响应体。

## 风险与权衡
- 日志体积可能较大：仅在非 2xx 时触发，并对流式响应保守处理。
- 读取响应体需保证转发可继续：通过复用 `rawBody` 保证行为一致。
