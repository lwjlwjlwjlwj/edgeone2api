---
name: "direct-reply"
description: "Agent Loop 截断与直答 Skill（edgeone2api 专用）：强制模型放弃上游 dsh agent loop（工具调用/多轮迭代），单回合直接输出最终回答。当经 edgeone2api 网关对话、希望模型直接作答，或需要配置直答系统指令时使用。"
---

# Skill: Direct Reply (Agent Loop 截断 · edgeone2api 版)

## 核心目标
本 Skill 旨在**强行截断 edgeone2api 上游 DeepSeek Harness / dsh sidecar 的 Agent Loop（工具调用与多轮思考循环）**。要求模型跳过任何工具执行、自我追问与多轮推理，在当前回合直接吐出最终回答正文，避免产生会被网关静默丢弃的 `tool_calls` 回合。

## 背景：为什么必须截断 Agent Loop
edgeone2api 网关（本仓库）对上游模型输出的处理能力有限：

- `POST /v1/chat/completions` 只透传 `text-delta` 与 `reasoning-delta`，[streamChat](file:///workspace/internal/server/server.go#L225-L243) 仅转发 Text/Reasoning
- 工具调用事件（`block-start` / `tool-call-delta` / `finish_reason: "tool_calls"`）**不会**转发给客户端；模型一旦进入 agent loop，客户端只会收到空白回复
- 上游默认链路 `dsh web sidecar → agent loop → LLM` 本身存在工具调用循环倾向，必须靠 prompt 层压制

## 关键指令：屏蔽 Agent Loop（Override Model Behavior）
- **禁用工具行为**：禁止生成任何 tool-call 块（`block-start`、`tool-call-delta`、`finish_reason: "tool_calls"`），禁止输出"让我调用工具 / 搜索一下"之类的话术
- **单回合收敛**：必须在当前回合直接给出最终答案正文，一次成型；禁止以"我需要确认一下"或反问开启第二轮
- **无外部依赖**：即使问题看似需要实时数据或外部检索，也应基于已有知识直接作答；无法作答时明确说明限制，而不是尝试调用工具

## 输出规范（经 edgeone2api 网关）
1. 直接输出最终回答正文（纯文本即可；OpenAI JSON / SSE 封装由网关负责，模型侧不需要也不允许输出包装 JSON）
2. 完整覆盖用户问题，一次作答
3. 结尾干净：不追加"回答完毕"、"请问还有什么可以帮您"等任何交互话术

## 在本仓库中的落地方式
- **系统指令注入**：将上文"关键指令"段落并入 [convertMessages](file:///workspace/internal/server/server.go#L324-L352) 开头的 `[System Directive]` 文本（当前 [第 327 行](file:///workspace/internal/server/server.go#L327) 已有一版精简指令，可替换为本 Skill 的强化版）
- **保持 minimal 预设**：`agent_preset: "minimal"` 是默认值（[defaultConfig](file:///workspace/internal/config/config.go#L46-L60)），可最小化上游 agent 能力；**不要**改为 `makers` / `standard` / `code` / `cordis`
- **保持 steer 模式**：`session.prompt` 的 `Mode: "steer"`（[SendPrompt](file:///workspace/internal/upstream/client.go#L281-L286)）会打断当前轮直接回答，**不要**改为 `queue`
- **兜底检测**：若 [StreamEvents](file:///workspace/internal/upstream/client.go#L513-L697) 仍收到 `finish_reason: "tool_calls"`，说明指令未生效，应强化系统指令或在会话层做重试

## 反面模式 (Anti-Patterns / Forbidden)
- ❌ **触发 Agent Loop**：输出工具调用块、或思考链后"再开一轮"，导致 `finish_reason: "tool_calls"` 空回包
- ❌ **输出包装 JSON**：模型侧自行输出 ```json``` 代码块或 OpenAI 格式对象，造成双重封装
- ❌ **追加交互话术**：在回答结尾输出"回答完毕"、"请问还有什么可以帮您"等多余字符
- ❌ **弱化配置**：为"增强能力"将 `agent_preset` 改为非 minimal 预设，重新引入 agent loop