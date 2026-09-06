package server

import "log"

// upstreamToClientAliases maps known upstream-native tool names (the DeepSeek
// Harness session's built-in tools) to common client-side aliases.  The
// upstream model tends to emit calls using its native tool names; the tool
// translation layer rewrites those names to a tool the client actually
// declared, so the agent loop stays executable on the client side.
var upstreamToClientAliases = map[string][]string{
	"bash":          {"run_command", "shell", "execute_command", "execute", "terminal", "run_shell"},
	"read":          {"read_file", "read_text", "view_file", "cat", "read_files"},
	"write":         {"write_file", "create_file", "overwrite_file", "save_file"},
	"edit":          {"edit_file", "replace_in_file", "apply_patch", "write_file", "str_replace_editor"},
	"glob":          {"search_files", "find_files", "list_files", "list_directory", "glob_files"},
	"grep":          {"grep_search", "search_files", "code_search", "ripgrep"},
	"webfetch":      {"web_fetch", "fetch_url", "http_request", "browse", "web_read"},
	"websearch":     {"web_search", "search_web", "search", "web_search_engine"},
	"apply_patch":   {"patch", "apply_patch_editor", "edit_file", "replace_in_file"},
	"ls":            {"list_directory", "read_directory", "list_files", "directory_listing"},
	"session_recall": {"session_recall", "recall"},
	"session_search": {"session_search", "search_session"},
	"skill":         {"skill", "use_skill", "load_skill"},
	"mcp":           {"mcp", "mcp_call", "use_mcp"},
}

// declaredToolSet is the set of tool names the client declared for this
// request, used by the translation layer to pick a matching alias.
type declaredToolSet map[string]openaiTool

// translateToolCalls rewrites upstream-native tool call names to names the
// client declared.  Calls whose name is already declared are passed through
// untouched.  A name that cannot be mapped to any declared tool is passed
// through with a warning: inventing a mapping is worse than letting the
// client surface the mismatch.
func translateToolCalls(calls []openaiToolCall, declared declaredToolSet) []openaiToolCall {
	if len(calls) == 0 || len(declared) == 0 {
		return calls
	}
	out := make([]openaiToolCall, len(calls))
	for i, c := range calls {
		out[i] = c
		name := c.Function.Name
		if _, ok := declared[name]; ok {
			continue // already a declared tool name
		}
		aliases := upstreamToClientAliases[name]
		if len(aliases) == 0 {
			log.Printf("[TRANSLATE] upstream tool %q has no alias mapping; passing through", name)
			continue
		}
		matched := false
		for _, alias := range aliases {
			if _, ok := declared[alias]; ok {
				out[i].Function.Name = alias
				log.Printf("[TRANSLATE] tool %q -> %q (client declared)", name, alias)
				matched = true
				break
			}
		}
		if !matched {
			log.Printf("[TRANSLATE] upstream tool %q not declared by client (aliases: %v)", name, aliases)
		}
	}
	return out
}

// translateToolCallName rewrites a single upstream-native tool name for the
// streaming incremental path, where only the name delta needs rewriting.
func translateToolCallName(name string, declared declaredToolSet) string {
	if len(declared) == 0 {
		return name
	}
	if _, ok := declared[name]; ok {
		return name
	}
	for _, alias := range upstreamToClientAliases[name] {
		if _, ok := declared[alias]; ok {
			log.Printf("[TRANSLATE] tool %q -> %q (client declared)", name, alias)
			return alias
		}
	}
	return name
}
