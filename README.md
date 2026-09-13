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
- **工具调用** — 原生支持 OpenAI `tools` 参数：客户端声明的工具定义注入系统指令，模型首轮即以 JSON 文本声明工具调用，网关强校验解析为标准 `tool_calls` 返回（ToolForge 风格，首轮截断，一次 LLM 调用即可闭环）；`role:"tool"` 结果回传后自然续接
- **纯文本模式（对齐 kuku2api）** — 未传 `tools` 的请求按 kuku2api 思路以纯文本黑盒处理：折叠上游一切工具事件、工具驱动的轮次自动续读、`finish_reason` 归一为 `stop`，绝不向客户端泄漏工具协议（无白屏/空回复）
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

## 工具调用与工具名翻译

网关原生支持 OpenAI `tools` 参数：客户端声明工具后，模型在单回合内返回标准 `tool_calls`
（流式/非流式一致），客户端执行后把结果以 `role: "tool"` 消息回传即可续接对话。

```bash
curl -s http://localhost:7863/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "@makers/deepseek-v4-flash",
    "messages": [{"role": "user", "content": "读取 /etc/hostname 的内容"}],
    "tools": [{"type": "function", "function": {"name": "read_file", "description": "读取文件", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}}]
  }'
```

上游模型倾向输出其**原生工具名**（如 `read`/`bash`/`glob`），而客户端可能声明了不同的名字
（如 `read_file`/`run_command`/`search_files`）。网关内置**工具名翻译层**，将上游原生名
按别名映射表重写为客户端实际声明的工具名；已匹配或无法映射的名字原样透传（并输出告警日志）。
同时系统指令会约束模型「只使用声明列表中的工具名」，从源头降低翻译需求。

## 纯文本模式（无 tools 请求）

未传 `tools` 的请求走**纯文本模式**，完整对齐 [kuku2api](https://github.com/xinxinshuhao-create/kuku2api)
「把上游 agent 当纯文本黑盒」的思路：

- 上游回车内出现 `block-start`(tool-call) / `tool-call-delta` / `block-end` 等工具事件时，
  客户端流直接**折叠丢弃**（服务端日志可见 `textOnly: dropping tool-call block`）
- 工具驱动的轮次**不会终止对话**——`turn/end` 时若本轮仅发生了被丢弃的工具调用，
  自动续读后续轮次，直到模型输出真正的正文
- `finish_reason` 统一归一为 `stop`，绝不输出 `tool_calls`；配合直答指令
  （"单轮收敛、禁止工具调用"）从源头抑制上游 agent loop
- 效果：无工具请求永远得到一段完整的纯文本回答，**不会白屏、不会空回复**，
  与 OpenAI 普通 chat 行为完全一致

这也意味着：客户端声明 `tools` 时获得完整工具调用能力；不声明时获得纯净的对话体验，
二者互不干扰。

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
│   ├── upstream/client.go        # Harness RPC 客户端 + 浏览器指纹 + SSE 读取 + textOnly 折叠
│   ├── upstream/client_test.go   # textOnly 折叠/续读/归一 + 指令注入单测
│   ├── upstream/directive.go     # 工具定义注入指令（ToolForge 风格）+ 纯文本直答指令
│   ├── server/server.go          # OpenAI 兼容 handler + 流式/非流式 + 工具调用 + textOnly 接线
│   ├── server/tools.go           # 工具名翻译层（上游原生名 → 客户端声明名）
│   ├── server/tools_test.go      # 翻译层单测
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
`role:"tool"` 回传续接）、工具名翻译（声明 `read_text`/`run_calc`）、长上下文 + 跨文件推理、
以及纯文本模式防泄漏（无 tools 请求 `finish_reason=stop`、零 tool_calls 泄漏、正文不白屏）。

## 免责声明

本项目仅供学习和研究使用。请遵守 DeepSeek Harness / EdgeOne Makers 平台服务条款，
自行承担使用风险。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

MIT