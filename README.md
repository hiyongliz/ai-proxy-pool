# ai-proxy-pool

Go 实现的 Claude 多供应商定向代理服务。  
可配置多个 Claude/Claude 兼容上游，并按请求头、模型名、路由规则进行转发。

## 功能

- 多上游供应商配置
- 路由策略：`round_robin`、`weighted_random`
- 定向路由：
  - 请求头强制指定（默认 `X-AI-Provider`）
  - 按 `model_prefix` / `model_regex`
  - 按 `path_prefix`
- 自动注入上游鉴权（`auth_token`/`bearer`/`x-api-key`）
- 结构化请求/响应日志（含状态码、耗时、路由到的 provider）
- 健康检查接口：`GET /healthz`

## 快速开始

1. 准备配置：

```bash
cp config.example.yaml config.yaml
```

2. 设置环境变量：

```bash
export PROXY_AUTH_TOKEN=your_proxy_token
export ANTHROPIC_API_KEY_PRIMARY=your_primary_key
export ANTHROPIC_API_KEY_BACKUP=your_backup_key
```

3. 启动服务：

```bash
go run ./cmd/ai-proxy-pool -config config.yaml
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

## 配置说明

- `server.listen_addr`: 监听地址
- `server.auth.*`: 代理入口认证（默认 `Authorization: Bearer <token>`）
- `router.strategy`: 负载策略（`round_robin` / `weighted_random`）
- `router.default_provider`: 规则未命中时的默认供应商
- `router.route_rules`: 路由规则列表
- `providers[]`: 上游供应商列表
  - `base_url`: 上游地址
  - `path_prefix`: 上游路径前缀
  - `api_key`: 支持 `${ENV}` 环境变量展开
  - `auth_type`: 鉴权注入方式（推荐 `auth_token`）
  - `static_headers`: 固定头（如 `anthropic-version`）

## 测试

```bash
go test ./... -race
```
