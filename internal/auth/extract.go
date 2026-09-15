package auth

import "encoding/json"

// ExtractKey 从 OpenAI 兼容请求体提取会话粘性键，按下列顺序依次尝试，找不到返回空串（绝不失败）：
//
//  1. metadata.conversation_id
//  2. metadata.conversationId
//  3. conversation_id
//  4. conversationId
//  5. metadata.user_id
//
// 设计移植自 workbuddy2api internal/session.ExtractKey（issue #35 经验）：
// 客户端实际常发 camelCase 的 conversationId，只识别 snake_case 会导致粘性不命中、
// 同对话轮转不同会话、上游上下文缓存 miss；两种命名均识别，snake_case 优先级更高
// （同值不同名命中同一对话时返回相同值，天然不混用）。
//
// 与 X-Session-Key header 的关系：header 显式键优先（向后兼容），无 header 时回落
// 本函数做透明粘性——客户端零感知，同一对话自动钉在同一上游会话。
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["user_id"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	return strOrEmpty(obj["conversationId"])
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
