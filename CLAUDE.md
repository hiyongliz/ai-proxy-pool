# AI Proxy Pool

Go 语言 AI 代理池与协议转换网关。

## 构建与测试

- 构建: `make build`
- 测试: `make test` (等同 `go test -race -cover ./...`)
- 格式化: `make fmt`
- 静态检查: `make lint` (等同 `go vet ./...`)
- 覆盖率: `make coverage`

## 项目结构

- `cmd/ai-proxy-pool/` — CLI 入口与子命令 (run/start/stop/status/config)
- `internal/proxy/` — 核心代理引擎 (请求转发/重试/熔断/统计)
- `internal/translator/codex/claude/` — Claude→Codex 协议翻译
- `internal/router/` — 请求路由与上游选择
- `internal/config/` — YAML 配置加载与校验
- `internal/metrics/` — Prometheus 指标

## 关键依赖

- cobra — CLI 框架
- bubbletea/lipgloss — TUI 仪表盘
- fsnotify — 配置热重载
- prometheus — 可观测性

## 提交前检查

1. `make test` 通过
2. `make lint` 无新增警告
