<div align="center">

# xai-router

**面向 xAI（Grok）API 的高性能单文件网关**

Key 池自动切换 · 带额度的客户端令牌 · 精确计费 · 详细请求日志 · 现代化管理后台

[English](README.md) · [简体中文](README.zh-CN.md)

[![CI](https://github.com/Kiyliy/xai-router/actions/workflows/ci.yml/badge.svg)](https://github.com/Kiyliy/xai-router/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

</div>

![概览](docs/screenshots/overview-dark.png)

xai-router 位于你的应用和 `https://api.x.ai` 之间。把任意 OpenAI / Anthropic 兼容客户端指向它，
用你自己签发的令牌访问；网关把请求分发到一组 xAI key 上——某个 key 被限流或失效时自动换下一个——
并精确记录每次请求花了多少钱。

整个服务是一个 Go 二进制文件，内嵌 SQLite 数据库和网页后台。不需要 Redis、不需要 CGO、不依赖任何外部服务。

## 功能

- **透明转发 xAI 全部接口**：`/v1/chat/completions`、`/v1/responses`（含 `previous_response_id`、
  `GET/DELETE /v1/responses/{id}`、`/compact`）、`/v1/messages`（Anthropic 兼容，支持 `x-api-key`）、
  图片（生成、编辑、deferred）、视频（生成、续写、编辑、轮询）、TTS/STT、文件、集合、批处理、模型列表、
  tokenize、embeddings 等。未知的 `/v1/*`、`/v2/*` 路径原样透传。SSE 流式响应逐 chunk 转发并立即 flush。
- **WebSocket**：`/v1/realtime`（实时语音）、流式 `/v1/tts`、`/v1/stt` 以及其他所有带 `Upgrade` 的请求，
  走同样的令牌校验、key 选择和换 key 流程。会话日志记录连接时长、双向消息数与字节数、关闭码与原因以及前 N 条消息；
  按官方语音价格计费，令牌额度用完时自动断开。浏览器可以用 `xai-client-secret.<令牌>` 子协议传令牌。
- **Key 池**：按优先级分组，组内加权随机 / 随机 / 轮询；遇到 401/403/429/5xx、网络错误或命中可配置的错误关键词时自动换 key。
  被限流的 key 进入冷却（遵循 `Retry-After`），连续失败的 key 触发熔断，无效、被封或余额耗尽的 key 自动禁用，
  原因直接显示在后台（可疑错误会先用免费的 `GET /v1/api-key` 复核，客户端无法通过在自己的输入里写"invalid key"
  之类的词让正常 key 被禁用）。可按 key 设置并发上限和每分钟请求数上限。
- **资源绑定**：异步视频/图片任务、deferred 对话、已存储的 response、文件、批处理和集合，后续访问总是使用创建它的那个 key，并且只有创建它的令牌能访问。
- **客户端令牌**：可签发任意多个 `sk-xr-…` 令牌，每个令牌可设置名称、美元额度、过期时间、模型白名单
  （支持别名和 `grok-4*` 通配）以及启用开关，分别统计用量。额度是严格的：每个请求发出前先按最坏情况预扣，
  结束后按实际费用结算，并发请求不会超额。模型列表接口（`/v1/models`、`/v1/language-models` 等）只返回该令牌允许的模型。
  管理员凭据与客户端令牌完全分开。
- **精确计费**：上游返回 `cost_in_usd_ticks` 时直接采用；否则按价格表计算——缓存输入、推理 token、图片输入、
  ≥200k 长上下文档位、Priority 档、服务端工具调用、按张/按秒的图片视频价格。全部用整数 tick 记账
  （1 美元 = 10¹⁰ tick），合计分毫不差。模型和官方价格自动从 xAI 同步，也可手动触发。
- **Key 健康检查**：测试单个或全部 key：`GET /v1/api-key` 查出被封/被禁用的 key，`GET /v1/models`
  列出可用模型，可选发送 1 个 token 的探测请求测延迟。支持批量导入 key 并顺带测试。
- **详细请求日志**：每个客户端请求一条记录，内嵌每一次上游尝试（用了哪个 key、状态码、错误、key 池采取的动作、耗时）；
  记录请求方法、路径、脱敏后的请求头、完整请求体和响应体（流式保存原始 SSE，并拼出完整文本和推理内容）、
  首包时间、总耗时、模型、各类 token、费用及其来源、key、令牌和客户端 IP。base64 大字段会被剔除，
  单条日志大小可配置上限，过期日志每天自动清理。可按时间、模型、状态、key、令牌、路径、请求 ID 搜索；
  打开任意一条可左右分栏查看请求和响应，或一键"复制为 curl"。
- **按客户端请求口径统计**：请求数、成功率、token、费用、延迟的时间趋势，按模型、key、令牌拆分。日志被清理后统计数据仍保留。
- **管理 API 全部开放**：后台的所有操作都通过带版本号的 REST API（`/api/v1`）完成，错误格式统一，
  提供 [OpenAPI 3 规范](web/static/openapi.yaml)，后台内也能直接浏览。
- **安全默认值**：未设置管理员密码时自动随机生成并只打印一次；令牌以 SHA-256 哈希存储并常量时间比较；
  请求体大小限制同样作用于 chunked 请求；不向上游透传 Cookie 等客户端头；后台使用严格 CSP，不加载任何第三方脚本；调试接口默认关闭；优雅退出时等待流式请求结束。
  带自动证书的 Caddy 部署方式见 [HTTPS 部署](docs/deploy-tls.md)。

## 截图

| 日志 | 日志详情 |
|---|---|
| ![日志](docs/screenshots/logs.png) | ![日志详情](docs/screenshots/log-detail.png) |
| **Key 管理** | **令牌** |
| ![Key](docs/screenshots/keys.png) | ![令牌](docs/screenshots/tokens.png) |
| **模型与价格** | **浅色主题** |
| ![模型](docs/screenshots/models.png) | ![概览浅色](docs/screenshots/overview-light.png) |

## 快速开始

### Docker Compose（推荐）

```bash
mkdir xai-router && cd xai-router
curl -O https://raw.githubusercontent.com/Kiyliy/xai-router/main/docker-compose.yml
docker compose up -d
docker compose logs xai-router | grep -A3 "generated an admin password"
```

打开 `http://localhost:8080`，用日志里打印的密码登录，然后：

1. **Key → 批量导入**：粘贴 xAI key（每行一个），勾选"测试"。
2. **令牌 → 新建**：复制 `sk-xr-…` 密钥（只显示一次）。
3. 调用：

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-xr-..." \
  -H "Content-Type: application/json" \
  -d '{"model": "grok-4.3", "messages": [{"role": "user", "content": "你好"}]}'
```

想自己构建镜像：克隆仓库后把 `docker-compose.yml` 里的 `image:` 换成 `build: .`。

### 二进制

从 [Releases](https://github.com/Kiyliy/xai-router/releases) 下载 Linux / macOS / Windows 版本，或从源码构建（Go 1.25+）：

```bash
go build -o xai-router ./cmd/xai-router
XR_ADMIN_PASSWORD='换成强密码' ./xai-router
```

### 配置

只有启动参数用环境变量；其余所有配置（重试规则、冷却、超时、请求体上限、日志保留天数、同步间隔、可信代理等）
都在后台 **设置** 页实时修改，或调用 `PUT /api/v1/settings`。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `XR_LISTEN` | `:8080` | 监听地址 |
| `XR_DATA_DIR` | `./data` | `xai-router.db` 所在目录 |
| `XR_ADMIN_PASSWORD` | — | 初始管理员密码（仅首次启动生效），不设置则随机生成 |
| `XR_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `XR_LOG_FORMAT` | `text` | `text` 或 `json` |

忘记密码：执行 `xai-router reset-password`（Docker 下用 `docker compose exec xai-router xai-router reset-password`），
会打印新密码并让所有会话下线。

## 调用网关

任何 OpenAI 兼容 SDK 都可以，只需改 base URL 和 key：

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-xr-...")

# Chat Completions，流式
for chunk in client.chat.completions.create(
    model="grok-4.3", stream=True,
    messages=[{"role": "user", "content": "写一首关于路由器的俳句"}],
):
    print(chunk.choices[0].delta.content or "", end="")

# Responses API，带会话状态
r1 = client.responses.create(model="grok-4.3", input="我叫 Ada。")
r2 = client.responses.create(model="grok-4.3", input="我叫什么？", previous_response_id=r1.id)
print(r2.output_text)
```

Anthropic SDK：`Anthropic(base_url="http://localhost:8080", api_key="sk-xr-...")`。

视频是异步的：`POST /v1/videos/generations` 返回 `request_id`，再轮询 `GET /v1/videos/{request_id}`。
网关会把每次轮询都发到提交任务的那个 key 上，并在返回完成结果时只计费一次。

WebSocket 也一样，连网关而不是 `wss://api.x.ai`：

```python
import json, websocket  # pip install websocket-client

ws = websocket.create_connection(
    "wss://gateway.example.com/v1/realtime?model=grok-voice-latest",
    header=["Authorization: Bearer sk-xr-..."],
)
ws.send(json.dumps({"type": "conversation.item.create", "item": {
    "type": "message", "role": "user", "content": [{"type": "input_text", "text": "你好"}]}}))
ws.send(json.dumps({"type": "response.create"}))
```

浏览器里无法设置请求头，可以把令牌放在子协议里：`new WebSocket(url, ["xai-client-secret.sk-xr-..."])`。

网关自身返回的错误采用 OpenAI 格式 `{"error": {"message", "type": "xai_router_error", "code"}}`，例如
`invalid_api_key`、`insufficient_quota`（429：本次请求的预扣费用超过剩余额度）、`model_not_allowed`、`model_required`、
`model_disabled`、`model_not_priced`、`no_upstream_keys`。上游的错误原样返回。

## 管理 API

所有接口都在 `/api/v1` 下，使用 `Authorization: Bearer <token>`：token 可以是 `POST /api/v1/auth/login`
返回的会话，也可以是在 **设置 → 管理 API 令牌** 中创建的长期令牌（`xra-…`）。错误统一为
`{"error": {"code": "not_found", "message": "key not found"}}`。完整规范见
[`web/static/openapi.yaml`](web/static/openapi.yaml)，后台 **API 文档** 页也可浏览。

```bash
BASE=http://localhost:8080
TOKEN=$(curl -s $BASE/api/v1/auth/login -d '{"password":"..."}' | jq -r .token)
auth=(-H "Authorization: Bearer $TOKEN")

# 批量导入 key 并做健康检查
curl -s "${auth[@]}" $BASE/api/v1/keys/import \
  -d '{"keys": "xai-AAA...\nbackup,xai-BBB...", "priority": 0, "test": true}'

# 创建客户端令牌：额度 $5，只允许 Grok 4 系列，年底过期
curl -s "${auth[@]}" $BASE/api/v1/tokens \
  -d '{"name": "my-app", "quota_usd": 5, "allowed_models": ["grok-4*"], "expires_at": "2026-12-31T23:59:59Z"}'

# 最近一小时的失败请求
curl -s "${auth[@]}" "$BASE/api/v1/logs?status=error&from=$(date -d '1 hour ago' +%s)"

# 单条日志（含请求/响应体和全部上游尝试），以及复制为 curl
curl -s "${auth[@]}" $BASE/api/v1/logs/42
curl -s "${auth[@]}" "$BASE/api/v1/logs/42/curl?base_url=$BASE" | jq -r .curl

# 近 7 天按模型统计花费
curl -s "${auth[@]}" "$BASE/api/v1/stats/breakdown?by=model&range=7d"

# 立即从 xAI 同步模型和官方价格
curl -s "${auth[@]}" -X POST $BASE/api/v1/models/sync
```

| 模块 | 接口 |
|---|---|
| 认证 | `POST /auth/login`、`POST /auth/logout`、`GET /auth/me`、`PUT /auth/password` |
| Key | `GET/POST /keys`、`GET/PATCH/DELETE /keys/{id}`、`POST /keys/import`、`POST /keys/test`、`POST /keys/{id}/test`、`POST /keys/{id}/reset` |
| 令牌 | `GET/POST /tokens`、`GET/PATCH/DELETE /tokens/{id}`、`POST /tokens/{id}/regenerate` |
| 管理令牌 | `GET/POST /admin-tokens`、`DELETE /admin-tokens/{id}` |
| 模型 | `GET/POST /models`、`PATCH/DELETE /models/{id}`、`POST /models/sync` |
| 日志 | `GET/DELETE /logs`、`GET /logs/{id}`、`GET /logs/{id}/curl` |
| 统计 | `GET /stats/overview`、`GET /stats/timeseries`、`GET /stats/breakdown` |
| 设置与系统 | `GET/PUT /settings`、`GET /system/info`、`GET /healthz` |

## 计费说明

xAI 会在 `usage.cost_in_usd_ticks` 中返回每次请求的实际扣费（1 美元 = 10,000,000,000 tick），
已包含缓存折扣、工具调用和各种倍率。只要上游返回了这个值，网关就直接记录（`cost_source = upstream`）；
没有返回时（例如 Anthropic 格式的流式响应）按价格表计算（`cost_source = computed`）：

```
输入   = (prompt − cached − image) × 输入价 + cached × 缓存价 + image × 图片输入价
输出   = (completion + reasoning) × 输出价
长上下文：prompt ≥ 200k token 时，整个请求按长上下文价格计
Priority：响应中 service_tier = "priority" 时 × 2
工具：web search $5/千次，X 帖子 $5/千条，X 用户 $10/千个，代码执行 $5/千次，集合检索 $2.5/千次
图片：按张（grok-imagine-image-2.0 按画质 × 分辨率）
视频：秒数 × 每秒价格，在任务完成时计费一次
语音：xAI 不返回费用，按官方价格计算：实时语音每分钟 $0.08 + 每次文本输入 $0.004；
      TTS 每百万字符 $15；STT 每小时 $0.10（REST）或 $0.20（流式）
```

该公式已对照 xAI 真实返回验证（Chat Completions、Responses、Messages 三个接口），计算值与上游扣费精确到 tick 一致。
不同接口的 token 口径不同（Chat 的推理 token 单独列出；Responses 的 `output_tokens` 已含推理；
Messages 的 `input_tokens` 不含缓存命中），网关会先统一口径再计价。

**额度。** 请求转发前，先按费用上限在令牌额度里预扣（按提示长度、`max_tokens`（未设置时按 4096）加 4096 个推理 token、
图片张数 × 单价、视频秒数 × 单价等估算）。剩余额度不够预扣就返回 `insufficient_quota`；请求结束后用实际费用替换预扣额。
所以并发请求加起来也不会超过额度。WebSocket 会话每 2 秒结算一次，额度用完即断开。

## 开发

```bash
go test ./...                 # 单元测试 + 集成测试（模拟 xAI 上游）
go vet ./... && staticcheck ./...
go run ./cmd/xai-router       # http://localhost:8080
```

后台是 [`web/static`](web/static) 下的纯 HTML/CSS/JS，没有框架也没有构建步骤，通过 `go:embed` 打包进二进制。

```
cmd/xai-router     入口、优雅退出、命令行
internal/proxy     /v1 转发、SSE、换 key、资源绑定
internal/pool      key 选择、冷却、熔断、限流
internal/billing   用量解析、价格表、计费、模型同步
internal/reqlog    异步日志写入、脱敏截断、过期清理
internal/keytest   key 健康检查
internal/admin     /api/v1 管理接口
internal/store     SQLite 表结构与查询
test/              基于模拟 xAI 的端到端测试
```

## 许可证

[MIT](LICENSE)
