package upstream

import (
	"encoding/json"
	"strings"
)

// BuildDirective returns the per-request system directive.  When the client
// declares tools, the model is told to emit a tool_calls JSON text as its
// answer (a request for the caller to run the tool), never to execute tools
// itself and never to drift into the platform's native agent loop.  Without
// tools it is a plain "answer directly" directive.
//
// This is the ToolForge-style approach: the upstream is used as a plain LLM,
// tool definitions are injected as protocol text, the model declares a call
// in the first turn, and the gateway parses + validates it against the
// client's declared set before returning standard tool_calls to the client.
func BuildDirective(toolsJSON string) string {
	if toolsJSON == "" {
		return `[System Directive]
You are an OpenAI-compatible API assistant. Answer the user's question directly and naturally. Do not mention tools, skills, or your own capabilities unless asked.
---`
	}

	var tools []map[string]any
	if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
		tools = nil
	}
	var names []string
	var schemaLines []string
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		names = append(names, name)
		desc, _ := fn["description"].(string)
		params, _ := fn["parameters"].(map[string]any)
		summary := summarizeSchema(params)
		schemaLines = append(schemaLines, "Tool name: "+name)
		if desc != "" {
			schemaLines = append(schemaLines, "Description: "+truncateString(desc, 240))
		}
		if summary != "" {
			schemaLines = append(schemaLines, "Parameters: "+summary)
		}
	}

	var b strings.Builder
	b.WriteString("[System Directive]\nYou are an OpenAI-compatible API assistant. You NEVER execute tools yourself. You only emit tool-call requests that the caller will run on your behalf.\n\n")
	if len(schemaLines) > 0 {
		b.WriteString("Available tools (use ONLY these exact names):\n" + strings.Join(schemaLines, "\n") + "\n\n")
	}
	b.WriteString("[Tool Calling Protocol]\n")
	b.WriteString("When the user's request requires one of the available tools, your ENTIRE answer must be a single JSON object and nothing else (no code fence, no explanation, no preamble, no suffix):\n")
	b.WriteString(`{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"<exact tool name>","arguments":"<JSON-string of parameters>"}}]}` + "\n")
	b.WriteString("- The tool name MUST be exactly one of the names listed above; never invent or translate names.\n")
	b.WriteString("- arguments is a JSON-encoded string matching the tool's parameters (escape inner quotes).\n")
	b.WriteString("- Emitting tool_calls is a REQUEST for the caller to execute; you will receive the result in a later message. Never claim a result you do not have.\n")
	b.WriteString("- Never emit calls for tools not listed above (e.g. mcp__edgeone__*, bash, read, glob, python, skill).\n")
	b.WriteString("When no tool is needed, answer directly with plain text and be genuinely helpful.\n---")
	return b.String()
}

// InitPrompt returns a minimal session-initialization prompt used when a fresh
// session is created.  It primes the session with the assistant identity and
// the no-native-tools posture without creating any skill files.
func InitPrompt() []ContentItem {
	return []ContentItem{{
		Type: "text",
		Text: "[Session Initialization]\nYou are an OpenAI-compatible API assistant. You answer directly; you never execute tools yourself. Reply with a single word: OK.",
	}}
}

func summarizeSchema(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	props, _ := params["properties"].(map[string]any)
	if len(props) == 0 {
		return ""
	}
	var keys []string
	for k := range props {
		keys = append(keys, k)
	}
	return "{" + strings.Join(keys, ", ") + "}"
}

func truncateString(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
