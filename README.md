# edgeone2api

把 EdgeOne Agents（DeepSeek Harness）包装成 OpenAI 兼容 API 的网关。

上游是带 agent loop 的对话引擎，原生输出流式 block 事件（文本、推理、工具调用）。
本网关负责两件事：**把 agent 协议翻译成 OpenAI 标准协议**，以及**保证工具只在客户端执行、上游沙箱永不介入**。

```
浏览器 / 任意 OpenAI 客户端
        │  HTTP /v1/chat/completions（OpenAI 格式）
        ▼
   edgeone2api  ── 会话池（4~32 弹性，指纹冷却）──►  EdgeOne Agents
        ▲                                                │
        └────── SSE block 事件（text / reasoning / tool-call）┘
```

## 特性

- **OpenAI 兼容**：`/v1/chat/completions`（流式 + 非流式）、`/v1/models`、`/healthz`
- **双模式**：
  - 未声明 `tools` → **纯文本模式**：折叠上游一切工具事件，`finish_reason` 归一为 `stop`，工具驱动的轮次自动续读直到输出正文，永远得到一段完整回答
  - 声明 `tools` → **工具调用模式**：解析上游原生 tool-call block（或 ToolForge JSON 文本协议），返回标准 `tool_calls` 给客户端执行
- **工具名白名单 + 别名映射**：上游漂移出的 `bash` / `str_replace_editor` / `mcp__edgeone__*` 等名字，在到达客户端前被校验/改写/丢弃，杜绝"客户端报 Tool not found → 模型编造沙箱被挡"的故障链
- **会话池**：空闲自动回补到 `pool_min`，突发并发预热到 `pool_max`，浏览器指纹限流后 24h 冷却
- **会话亲和**：客户端带 `X-Session-Key` 即绑定固定会话，多轮对话上下文连续
- **会话生命周期健壮性**：上游会话被回收（"session not found"）被识别为生命周期事件而非配额事件，自动换新会话重试，不烧指纹、不 502
- **安全**：SSE 流在 turn 结束时立即 cancel，上游 agent loop（沙箱工具执行）在启动前即被掐断——工具永远跑在客户端，不在沙箱

## 快速开始

### 1. 配置

```bash
cp config.example.json config.json
# 编辑 config.json：至少填 api_key 与 upstream_url
```

| 配置项 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7863` | 监听地址 |
| `api_key` | 空 | 请求鉴权 Bearer token；留空则不做鉴权 |
| `models` | 见示例 | 对外暴露的模型列表 |
| `pool_min` / `pool_max` | `2` / `8` | 会话池弹性范围 |
| `ttl_minutes` | `0` | 会话 TTL（0 = 不过期） |
| `free_ttl_minutes` | `90` | 空闲会话回收上限（上游约 1.5-2h 销毁空闲会话，早于它回收避免僵尸） |
| `bind_ttl_minutes` | `30` | 绑定会话空闲回收上限 |
| `max_req_per_session` | `200` | 单会话最大请求数，超出自动轮换 |
| `upstream_url` | `https://deepseek-harness.edgeone.cool` | 上游端点 |
| `agent_preset` | `minimal` | 上游会话预设 |
| `request_timeout_seconds` | `600` | 非流式整体超时 |
| `stream_idle_seconds` | `180` | 流式无事件判定死连接 |
| `default_reasoning_effort` | `off` | 全局推理强度兜底（`off`/`high`/`max`/空） |
| `request_jitter_ms` | `0` | 发送前随机延迟，伪并发防抖 |
| `max_concurrent` | `0` | 并发上游请求上限（0 = 不限） |
| `model_map` | 见示例 | 模型名 → 上游 provider/model 映射，可带每模型 `reasoning_effort` |

所有配置项均支持同名环境变量覆盖（`listen` → `EDGEONE_API_LISTEN`，`api_key` → `EDGEONE_API_KEY`，`models` → `EDGEONE_API_MODELS`，`pool_min` → `EDGEONE_API_POOL_MIN`，依此类推），无需配置文件也可运行。

### 2. 运行

```bash
go build -o edgeone2api ./cmd/server
./edgeone2api -config config.json
# 或容器：
docker build -t edgeone2api .
docker run -d --name edgeone2api --network host \
  -v "$PWD/config.json:/app/config.json:ro" \
  -e EDGEONE_API_LISTEN=:7863 \
  edgeone2api
```

健康检查：

```bash
curl http://localhost:7863/healthz
# {"poolSize":4,"status":"ok"}
```

## 使用

### 普通对话（无工具）

```bash
curl http://localhost:7863/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $API_KEY" \
  -d '{"model":"@makers/deepseek-v4-flash","messages":[{"role":"user","content":"你好"}]}'
```

### 工具调用

```bash
curl http://localhost:7863/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $API_KEY" \
  -d '{
    "model": "@makers/deepseek-v4-flash",
    "messages": [{"role":"user","content":"杭州天气怎么样？"}],
    "tools": [{"type":"function","function":{
      "name":"get_weather",
      "description":"查询天气",
      "parameters":{"type":"object","properties":{"city":{"type":"string"}}}
    }}]
  }'
```

返回 `finish_reason="tool_calls"` 与标准 `tool_calls`，由**客户端**执行并把结果以 `role=tool` 消息发回继续对话。网关只做协议翻译，工具永远不在上游执行。

### 多轮上下文

每次请求带 `X-Session-Key: <任意字符串>` 即绑定同一上游会话：

```bash
curl ... -H 'X-Session-Key: my-session-1' -d '{...}'
```

同一 key 的后续请求共享上下文；不传则从池中取空闲会话。会话达到 `max_req_per_session` 或连续失败后自动轮换为全新会话。

### 会话池状态

```bash
curl http://localhost:7863/pool   # free/bound/total、配额轮换计数
```

## 可用模型

`model_map` 将客户端请求的模型名映射到上游 provider/model，客户端可选传 `reasoning_effort`（`off`/`high`/`max`），或由映射默认指定。

| 模型 | provider | 推理强度 |
|---|---|---|
| `@makers/deepseek-v4-flash` | edgeone-makers | off / high / max |
| `@makers/deepseek-v4-pro` | edgeone-makers | off / high / max |
| `@makers/hy3` / `@makers/hy3-preview` | edgeone-makers | 无 |
| `@makers/minimax-m3` / `@makers/minimax-m2.7` | edgeone-makers | 无 |
| `@makers/kimi-k2.6` | edgeone-makers | 无 |

> `deepseek-official` provider 的模型需要额外的上游凭证，默认不可用，已从默认 `model_map` 排除。

## 工具名过滤（tool filter）

上游模型可能输出**它自己平台的原生工具名**（Codex/EdgeOne 风格的 `bash`、`str_replace_editor`、`mcp__edgeone__*`），而客户端声明的是 `exec`、`read` 等。网关在返回前做一层过滤：

| 类别 | 处理 |
|---|---|
| 客户端已声明的名字 | 原样透传 |
| 别名表命中（如 `bash→exec`、`str_replace_editor→read`） | 改写为目标名（仅当目标名被客户端声明才生效） |
| `mcp__edgeone__*` / `workspace_run_command` 等平台能力 | 一律丢弃，绝不透传 |
| 其余未知名 | 丢弃 + 日志 `[TOOLS] filter: drop tool "X"` |

别名表在 `internal/server/toolfilter.go`，加新映射只需补一行。日志里出现 `drop tool` 即提示需要补映射。

## 架构与关键机制

### 纯文本模式（textOnly）

未传 `tools` 时：

- 上游 tool-call block 事件在客户端流中**折叠丢弃**（服务端日志 `textOnly: dropping tool-call block`）
- 工具驱动的轮次**不会终止对话**：`turn/end` 时若本轮只有被丢弃的工具调用，自动续读后续轮次直到模型输出真正正文
- `finish_reason` 统一归一为 `stop`

### 工具调用模式

- **原生通道**：上游发 tool-call block（`block-start` → `tool-call-delta` → `block-end`），网关流式拼装为标准 `tool_calls`
- **文本协议通道**（ToolForge fallback）：模型以纯文本 JSON `{"tool_calls":[...]}` 声明调用时，网关解析并补发结构化 `tool_calls` delta
- 两通道都经过工具名过滤层

### 会话生命周期

- 上游约 20 分钟回收空闲会话：`session not found` 被识别为**生命周期事件**（`MarkGone`），换新会话重试，不烧指纹
- 真实配额错误（`IsQuotaError`）→ `MarkQuotaExceeded` → 该浏览器指纹冷却 24h
- 绑定会话的 mutex 用 CAS 门控 + `active` 标志，杜绝锁泄漏导致的同 key 请求挂死

## 开发

```bash
go build ./... && go vet ./... && go test ./...
```

布局：

```
cmd/server/           入口：配置、会话池初始化、HTTP 服务
internal/config/      配置加载与默认值（支持环境变量覆盖）
internal/auth/        会话池、指纹冷却、生命周期（MarkGone/MarkQuotaExceeded）
internal/server/      OpenAI 协议层、双模式聚合、工具名过滤（toolfilter.go）
internal/upstream/    EdgeOne SSE 客户端（block 事件解析、textOnly 折叠续读）
internal/toolcall/    （保留）
scripts/              真实场景验证脚本（gen_demo_data / verify_longctx / verify_tools）
```

验证脚本直接请求真实上游（`@makers/deepseek-v4-flash`）：

```bash
python3 scripts/gen_demo_data.py   # 生成演示数据到 /tmp/kuku2api_demo/
python3 scripts/verify_longctx.py  # 长上下文 + 多轮会话连续性
python3 scripts/verify_tools.py    # 工具调用矩阵（非流式/流式/多轮链式闭环/名字翻译/纯文本防泄漏）
```

## 逆向说明

架构链路（浏览器 → Harness）：

```
浏览器/本代理 → POST /api/session.* (HTTP JSON-RPC) → EdgeOne Agents
  → dsh web sidecar (127.0.0.1:port) → @deepseek-ai/dsh (agent loop)
  → dsh-llm-pi-ai → 本地 gateway proxy → AI Gateway（真实 LLM）
```

- `session.prompt` 是纯 LLM 端口：`{sessionId, mode: "steer"|"queue", content, clientTimeZone?}`；`steer` 打断当前轮直接回答（代理默认），`queue` 追加到队列等待
- `session.create` 支持 `agentPreset`：`standard`/`code`/`minimal`/`cordis`（内置锁定预设）+ 自定义
- 前端 bundle 只是 UI + RPC 转发壳，agent loop 与 LLM 调用全部在服务端 sidecar（`@deepseek-ai/dsh` npm 包的 `lib/bin.js web` 命令）内

## Docker 部署

```bash
docker compose up -d --build
```

- 端口映射与配置通过 `docker-compose.yml` 的 environment 配置
- Dockerfile 多阶段构建，alpine 运行时无外部依赖

## 说明

- 上游 `session.create` 偶发超时属上游侧问题（容器启动预热时可见），网关会自动重试
- 本项目不内置任何模型，纯协议网关；模型能力取决于上游

## 免责声明

本项目仅供学习和研究使用。请遵守 DeepSeek Harness / EdgeOne Makers 平台服务条款，自行承担使用风险。
