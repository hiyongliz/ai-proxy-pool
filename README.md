# ai-proxy-pool

Go 实现的 Claude 多供应商智能代理服务。
支持多个 Claude/Claude 兼容上游，按请求头、模型名、路由规则进行转发，并提供模型映射、负载均衡等功能。

## 功能

- 多上游供应商配置
- 路由策略：`round_robin`、`weighted_random`
- 定向路由：
  - 请求头强制指定（默认 `X-AI-Provider`）
  - 按 `model_prefix` / `model_regex`
  - 按 `path_prefix`
- 模型映射：每个 provider 可配置模型名替换规则
- 自动注入上游鉴权（`auth_token`/`bearer`/`x-api-key`）
- 自动重试：5xx 错误或网络异常时自动切换到其他 provider
- 热重载：配置文件修改后自动生效，无需重启
- Prometheus 指标：`GET /metrics`
- 后台守护进程模式（`-d`）
- 日志文件输出
- 结构化请求/响应日志（含状态码、耗时、路由到的 provider）
- 健康检查接口：`GET /healthz`

## 安装

```bash
# 从源码安装
make install

# 或直接 go install
go install ./cmd/ai-proxy-pool
```

## 快速开始

1. 准备配置：

```bash
mkdir -p ~/.ai_proxy_pool
cp config.example.yaml ~/.ai_proxy_pool/config.yaml
```

2. 设置环境变量：

```bash
export PROXY_AUTH_TOKEN=your_proxy_token
export ANTHROPIC_API_KEY_PRIMARY=your_primary_key
export ANTHROPIC_API_KEY_BACKUP=your_backup_key
```

3. 启动服务：

```bash
# 前台运行
ai-proxy-pool

# 后台守护进程运行
ai-proxy-pool -d
```

4. 请求示例：

```bash
curl -sS http://127.0.0.1:8080/v1/messages \
  -H 'content-type: application/json' \
  -H "Authorization: Bearer ${PROXY_AUTH_TOKEN}" \
  -d '{
    "model":"claude-4-sonnet",
    "max_tokens":64,
    "messages":[{"role":"user","content":"hello"}]
  }'
```

强制指定供应商：

```bash
curl -sS http://127.0.0.1:8080/v1/messages \
  -H 'content-type: application/json' \
  -H "Authorization: Bearer ${PROXY_AUTH_TOKEN}" \
  -H 'X-AI-Provider: claude-backup' \
  -d '{"model":"claude-4-sonnet","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'
```

## 命令行参数

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-config` | 配置文件路径 | `~/.ai_proxy_pool/config.yaml` |
| `-d` | 后台守护进程运行 | `false` |
| `-stop` | 停止后台守护进程 | `false` |
| `-restart` | 重启后台守护进程 | `false` |
| `-logs` | 查看并持续跟随日志输出 | `false` |
| `-log` | 日志文件路径 | `~/.ai_proxy_pool/ai-proxy-pool.log` |

**配置文件默认路径**：`~/.ai_proxy_pool/config.yaml`（可通过 `-config` 覆盖）

## 配置说明

### Server

| 字段 | 说明 | 默认值 |
|------|------|--------|
| `listen_addr` | 监听地址 | `:8080` |
| `read_timeout` | 读超时 | `30s` |
| `write_timeout` | 写超时 | `300s` |
| `idle_timeout` | 空闲超时 | `60s` |
| `upstream_timeout` | 上游请求超时 | `300s` |
| `max_request_body_bytes` | 单请求体大小上限 | `8388608` (8 MiB) |
| `auth.enabled` | 启用代理入口认证 | `false` |
| `auth.token` | 认证 Token（支持 `${ENV}`） | - |
| `retry.max_attempts` | 最大重试次数（含首次） | `3` |
| `retry.retry_on_5xx` | 5xx 响应时重试 | `true` |
| `retry.retry_on_network` | 网络错误时重试 | `true` |

说明：当 `auth.enabled=true` 时，入口会兼容两种认证头：
- `Authorization: Bearer <token>`
- `X-Api-Key: <token>`

### Router

| 字段 | 说明 | 默认值 |
|------|------|--------|
| `strategy` | 负载策略 | `round_robin` |
| `default_provider` | 规则未命中时的默认供应商 | - |
| `header_provider_key` | 强制指定 provider 的请求头 | `X-AI-Provider` |
| `route_rules` | 路由规则列表 | - |

### Providers

| 字段 | 说明 |
|------|------|
| `name` | 供应商名称（唯一标识） |
| `base_url` | 上游地址 |
| `path_prefix` | 上游路径前缀 |
| `enabled` | 是否启用（默认 `true`） |
| `weight` | 权重（用于 `weighted_random`） |
| `timeout` | 单独超时设置 |
| `api_key` | API Key（支持 `${ENV}`） |
| `auth_type` | 鉴权方式：`auth_token`/`bearer`/`x-api-key`/`none` |
| `static_headers` | 固定头（如 `anthropic-version`） |
| `model_prefixes` | 模型前缀提示（用于路由） |
| `model_mapping` | 模型名映射（见下方） |

### 模型映射

每个 provider 可配置 `model_mapping`，将客户端请求的模型名替换为上游实际使用的模型名：

```yaml
providers:
  - name: "claude-backup"
    base_url: "https://your-provider.example.com"
    model_mapping:
      claude-opus-4-6: "claude-opus-4-5"     # 请求 4-6 → 上游用 4-5
      claude-sonnet-4-6: "claude-sonnet-4-5"
```

对客户端完全透明，日志中会记录映射信息。

## Makefile 命令

```bash
make build    # 编译
make run      # 编译并运行
make test     # 运行测试
make lint     # 代码检查
make clean    # 清理构建产物
make install  # 安装到 $GOPATH/bin
```

## 运维

### 停止后台进程

```bash
ai-proxy-pool -stop
```

### 重启后台进程

```bash
ai-proxy-pool -restart
```

### 热重载配置

配置文件修改后会自动重载，也可手动触发：

```bash
kill -HUP $(cat ~/.ai_proxy_pool/ai-proxy-pool.pid)
```

### 查看日志

```bash
ai-proxy-pool -logs
```

## License

MIT
