# edgeone2api

> EdgeOne Makers Agent 的 OpenAI 兼容 API 网关，Go 实现。

逆向自 EdgeOne Makers / DeepSeek Harness Web Chat 的 `session.prompt` RPC 接口，将其封装为标准 OpenAI 兼容 `/v1/chat/completions` 端点，支持 SSE 流式输出，免登录即可调用 Makers Agent 托管的 DeepSeek 系列模型。

## 功能特性

- **免登录** — 直接调用 Harness 的 `session.create` / `session.prompt` RPC，无需登录即可使用
- **多模型支持** — `@makers/deepseek-v4-flash`、`@makers/deepseek-v4-pro`、`@makers/kimi-k2.6`、`@makers/hy3`、`@makers/minimax-m3` 等（通过 `model_map` 配置）
- **推理强度可调** — 支持 OpenAI 标准 `reasoning_effort` 参数（`off`/`high`/`max`）
- **浏览器指纹隔离** — 每个会话独立 UA/Sec-CH-UA 指纹，上游将每个会话视为独立浏览器，限流互不影响
- **弹性凭证池** — 会话池自动创建/复用/维护，配额感知轮换（绕过单一会话的用量上限）；会话不足时并发后台扩容，突发请求不排队
- **SSE 流式** — 流式透传上游事件流；非流式自动聚合 `content`
- **工具调用（prompt 层模拟）** — 支持 OpenAI `tools` 参数：工具定义渲染为私有定界符文本协议注入对话，模型在回复末尾声明调用，网关解析回标准 `tool_calls` 返回（流式/非流式一致）；`role:"tool"` 结果回传后续接。**上游始终被当作纯文本黑盒**，网关自身从不执行工具，沙箱也不必被调用。性质为 best-effort，边界见下方「工具调用」
- **纯文本模式（对齐 kuku2api）** — 无论是否声明 `tools`，上游的原生工具事件一律折叠丢弃、工具驱动的轮次自动续读、`finish_reason` 归一为 `stop`；无 `tools` 的请求永远得到一段完整纯文本回答（无白屏/空回复）
- **可选鉴权** — 配置 `api_key` 后需 Bearer token 访问
- **Go 单二进制** — 无外部依赖，`go build` 即得

## 快速开始

### 1. 构建 & 配置

```bash
go build -o edgeone2api ./cmd/server
cp config.example.json config.json
# 编辑 config.json，设置 api_key（可留空 = 不鉴权）、model_map 等
```

### 2. 启动服务

```bash
./edgeone2api -config config.json
```

或直接用环境变量（无需配置文件）：

```bash
EDGEONE_API_LISTEN=:7863 EDGEONE_API_POOL_MIN=4 EDGEONE_API_POOL_MAX=32 ./edgeone2api
```

### 3. 验证

```bash
# 健康检查
curl -s http://localhost:7863/healthz

# 模型列表
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"

# 聊天（非流式）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"@makers/deepseek-v4-flash","messages":[{"role":"user","content":"你好"}]}'

# 聊天（流式）
curl -N http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"@makers/deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"数到3"}]}'

# 鉴权可省略（api_key 为空时）；会话池状态
curl -s http://localhost:7863/pool
```

## 配置说明

```json
{
  "listen": ":7863",
  "api_key": "",
  "models": ["@makers/deepseek-v4-flash", "@makers/deepseek-v4-pro"],
  "pool_min": 4,
  "pool_max": 32,
  "ttl_minutes": 0,
  "bind_ttl_minutes": 30,
  "max_req_per_session": 200,
  "upstream_url": "https://deepseek-harness.edgeone.cool",
  "agent_preset": "minimal",
  "model_map": {
    "@makers/deepseek-v4-flash": {"provider": "edgeone-makers", "model": "@makers/deepseek-v4-flash"}
  }
}
```

| 字段 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `listen` | `EDGEONE_API_LISTEN` | `:7863` | 监听地址 |
| `api_key` | `EDGEONE_API_KEY` | 空 | API 鉴权 key（空=不鉴权） |
| `models` | `EDGEONE_API_MODELS` | `[flash, pro]` | 支持的模型列表（`/v1/models` 返回） |
| `pool_min` | `EDGEONE_API_POOL_MIN` | `4` | 会话池最小会话数 |
| `pool_max` | `EDGEONE_API_POOL_MAX` | `32` | 会话池最大会话数（并发上限） |
| `ttl_minutes` | `EDGEONE_API_TTL_MINUTES` | `0` | 会话最长生命周期（分钟，`0`=无限制，仅失败或达上限时回收） |
| `bind_ttl_minutes` | `EDGEONE_API_BIND_TTL_MINUTES` | `30` | 绑定会话（会话连续性）空闲超时（分钟） |
| `max_req_per_session` | `EDGEONE_API_MAX_REQ_PER_SESSION` | `200` | 单会话最大请求数，超出后自动轮换 |
| `upstream_url` | `EDGEONE_API_UPSTREAM` | `https://deepseek-harness.edgeone.cool` | 上游 Harness 地址 |
| `agent_preset` | — | `minimal` | agent 预设（`minimal`/`makers`/`standard`/`code`/`cordis`） |

### 模型与推理强度

`model_map` 将客户端请求的模型名映射到上游 provider/model。客户端可选传 `reasoning_effort`
（`off`/`high`/`max`），或由映射默认指定。会话级缓存避免重复调用 selectModel。

```bash
curl -s http://localhost:7863/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"@makers/deepseek-v4-flash","reasoning_effort":"high","messages":[{"role":"user","content":"9.11和9.8哪个大？"}]}'
```

可用模型目录（`session.models` 实测）：

| 模型 | provider | 推理强度 |
|---|---|---|
| `@makers/deepseek-v4-flash` | edgeone-makers | off / high / max |
| `@makers/deepseek-v4-pro` | edgeone-makers | off / high / max |
| `@makers/hy3` / `@makers/hy3-preview` | edgeone-makers | 无 |
| `@makers/minimax-m3` / `@makers/minimax-m2.7` | edgeone-makers | 无 |
| `@makers/kimi-k2.6` | edgeone-makers | 无 |

> `deepseek-official` provider 的模型（如 `deepseek-v4-pro`）需要设置 `EDGEONE_API_KEY`
> （通过 Harness 的 credentials 服务），默认不可用，已从默认 `model_map` 中排除。

## API

### `POST /v1/chat/completions`

OpenAI 兼容。支持 `stream`（SSE）、`max_tokens`、`temperature`、`top_p`、`reasoning_effort`、`tools`（见下方「工具调用」）。

### `GET /v1/models`

返回配置的模型列表。

### `GET /pool`

查看会话池状态（free/bound/total、配额轮换计数）。

### `GET /healthz`

健康检查。

## 会话连续性

通过 `X-Session-Key` 请求头关联会话：同一 key 的请求复用同一上游会话（保留对话上下文）。
未提供时自动生成 key。会话达到 `max_req_per_session` 或连续失败后自动轮换为全新会话。

## 工具调用（partial, upstream-limited）

上游是一个**消费级网页套壳**，不是开发者 API。它没有原生的 `tools` / `function calling`
参数——更重要的是，它的人格会主动守卫“被当作工具执行器”这一角色。因此本网关**在 prompt 层
模拟工具调用**（与 [kuku2api](https://github.com/xinxinshuhao-create/kuku2api) 的做法一致）：
请求带 `tools` 时，工具定义被渲染成一段私有定界符文本协议，注入到对话之前；模型在回复末尾
按该格式声明调用，网关解析回标准 `tool_calls`（流式/非流式一致）。

**重要：上游始终被当作纯文本黑盒。** 网关从不执行工具，也从不把原生 tool-call 块透传给客户端；
`defer cs.Cancel()` 在每轮结束时切断上游事件流，使上游 agent loop 不会启动。客户端收到
`tool_calls` 后自行执行，再以 `role: "tool"` 消息回传结果续接。

```bash
curl -s http://localhost:7863/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "@makers/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "现在几点？"}],
    "tools": [{"type": "function", "function": {"name": "get_time", "description": "获取当前时间", "parameters": {"type": "object", "properties": {"timezone": {"type": "string"}}}}}]
  }'
```

`tool_choice: "none"` 时尊重客户端意图，**不注入**协议，与不传 `tools` 行为一致。

**能做到什么（实测）**

- 语义上“合理且知名”、描述清晰的工具（如 `get_time` / `search` 类）通常能被识别并按格式输出，
  返回合法 OpenAI `tool_calls`（`finish_reason: "tool_calls"`，`arguments` 为 JSON 字符串）。
- 流式与非流式均支持；流式下工具块**绝不作为 content 泄漏**（带尾窗口门控，正文零额外延迟）。

**做不到什么（实测，非推测）**

- **自定义/陌生工具会被拒绝。** 请求调用自造工具时，上游可能回复“不在我的可用工具列表中”
  并指出该定界符调用格式是合成的——它已识别出注入协议是合成的。属产品级人格保护，非提示词可绕过。
- **模型自认能直接回答的问题会跳过工具。** 通用知识类问题它会直接作答而不调用。
- 因此**不要**把它用于需要确定性工具调用的 Agent 场景；按 best-effort 对待，逐工具验证。
- 若需要真正的 function calling，请使用原生支持该能力的上游。

## 纯文本模式（对齐 kuku2api）

本网关**始终**把上游当纯文本黑盒（无论请求是否带 `tools`），完整对齐
[kuku2api](https://github.com/xinxinshuhao-create/kuku2api) 的思路：

- 上游回车内出现 `block-start`(tool-call) / `tool-call-delta` / `block-end` 等工具事件时，
  客户端流直接**折叠丢弃**（服务端日志可见 `dropping tool-call block (plain-text posture)`）
- 工具驱动的轮次**不会终止对话**——`turn/end` 时若本轮仅发生了被丢弃的工具调用，
  自动续读后续轮次，直到模型输出真正的正文
- `finish_reason` 统一归一为 `stop`，原生路径绝不输出 `tool_calls`；工具调用只可能来自
  prompt 层协议的解析
- 效果：无工具请求永远得到一段完整的纯文本回答，**不会白屏、不会空回复**，
  与 OpenAI 普通 chat 行为完全一致

### 会话池扩容

会话池在 `pool_min`（默认 4）~ `pool_max`（默认 32）之间弹性伸缩：

- 空闲时维护协程（15 秒周期）自动回补到 `pool_min`
- 突发请求超过可用会话时，触发**并发后台预热**（最多 4 个会话创建并行在途），
  请求以 300ms 轮询等待新会话就绪，而不是串行排队创建
- 任一指纹（浏览器身份）被上游限流后进入 24h 冷却，不再参与会话创建

## 逆向说明

架构链路（浏览器 → Harness）：

```
浏览器/本代理 → POST /api/session.* (HTTP JSON-RPC) → EdgeOne Agents
  → dsh web sidecar (127.0.0.1:port) → @deepseek-ai/dsh (agent loop)
  → dsh-llm-pi-ai → 本地 gateway proxy → AI Gateway（真实 LLM）
```

- `session.prompt` 是纯 LLM 端口：`{sessionId, mode: "steer"|"queue", content, clientTimeZone?}`
  - `steer` = 打断当前轮直接回答（代理默认使用）
  - `queue` = 追加到队列等待
- `session.create` 支持 `agentPreset`：`standard`/`code`/`minimal`/`cordis`（内置锁定预设）+ 自定义
- 前端 bundle（`dsh-client-*`）只是 UI + RPC 转发壳，agent loop 与 LLM 调用全部在服务端 sidecar
  （`@deepseek-ai/dsh` npm 包的 `lib/bin.js web` 命令）内

## Docker 部署

```bash
docker compose up -d --build
```

- 端口映射 `7863:7863`，通过 `docker-compose.yml` 的 environment 配置
- Dockerfile 多阶段构建，alpine 运行时无外部依赖

## 目录结构

```
edgeone2api/
├── cmd/server/main.go            # 入口：配置、会话池初始化、HTTP 服务
├── internal/
│   ├── auth/pool.go              # 会话池：创建/绑定/轮换/并发预热扩容（核心）
│   ├── auth/pool_test.go         # 会话池单测（含并发扩容回归）
│   ├── config/config.go          # 配置加载 + model_map + env override
│   ├── upstream/client.go        # Harness RPC 客户端 + 浏览器指纹 + SSE 读取 + 纯文本折叠
│   ├── upstream/client_test.go   # 折叠/续读/归一 + 指令注入单测
│   ├── upstream/directive.go     # 工具定义注入（kuku2api 定界符协议）+ 纯文本直答指令
│   ├── server/server.go          # OpenAI 兼容 handler + 流式/非流式 + 工具解析/门控
│   ├── server/server_test.go     # 定界符解析/工具名校验/tool_choice 单测
│   └── toolcall/                 # 可选的工具调用中间件（独立部署）
├── config.example.json
├── scripts/                   # 真实场景验证脚本（见下节）
│   ├── gen_demo_data.py       # 演示数据生成（sales.csv / inventory_notes.txt）
│   ├── verify_longctx.py      # 长上下文 + 多轮会话连续性验证
│   └── verify_tools.py        # 复杂工具调用矩阵验证（S1-S6）
├── Dockerfile
├── docker-compose.yml
└── go.mod
```

## 真实场景验证

验证不依赖 mock——直接请求真实上游 `deepseek-harness.edgeone.cool`，模型为
`@makers/deepseek-v4-flash`。脚本：

```bash
python3 scripts/gen_demo_data.py   # 生成演示数据到 /tmp/kuku2api_demo/
python3 scripts/verify_longctx.py  # 长上下文：6527 字符文档 + 5 轮增量对话（仅发新消息），10/10 通过
python3 scripts/verify_tools.py    # 工具调用矩阵：S1-S6，18/18 断言通过
```

`verify_tools.py` 覆盖：非流式/流式、多轮链式工具闭环（read_file → calculate →
`role:"tool"` 回传续接）、自定义工具名直透（声明 `read_text`/`run_calc`）、长上下文 + 跨文件推理、
以及纯文本模式防泄漏（无 tools 请求 `finish_reason=stop`、零 tool_calls 泄漏、正文不白屏）。

> 脚本默认目标为 `http://127.0.0.1:7863`，可用 `EDGEONE_API_BASE` 覆盖。
> 工具层为 best-effort（见「工具调用」边界），S4 对“模型是否实际调用自定义工具”只报告、不断言。

## 免责声明

本项目仅供学习和研究使用。请遵守 DeepSeek Harness / EdgeOne Makers 平台服务条款，
自行承担使用风险。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

MIT