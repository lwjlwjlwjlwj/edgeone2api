package server

import (
	"log"
	"strings"
)

// upstreamToClientAliases maps known upstream-native tool names (the DeepSeek
// Harness session's built-in tools) to common client-side aliases.  The
// upstream model tends to emit calls using its native tool names; the tool
// translation layer rewrites those names to a tool the client actually
// declared, so the agent loop stays executable on the client side.
var upstreamToClientAliases = map[string][]string{
	"bash":          {"run_command", "shell", "execute_command", "execute", "terminal", "run_shell", "exec"},
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

// normalizeUpstreamToolName reduces an upstream-native tool name to one or
// more candidate base names, most specific first.  DeepSeek-Harness MCP
// tools surface as mcp__<provider>__<server>_<tool> (e.g.
// mcp__edgeone__workspace_run_command) and unified tools as u_<tool> (e.g.
// u_exec); the trailing tokens are the names the alias table actually
// knows, so each underscore-separated suffix is tried in turn.
func normalizeUpstreamToolName(name string) []string {
	if strings.HasPrefix(name, "mcp__") {
		rest := strings.TrimPrefix(name, "mcp__")
		parts := strings.Split(rest, "__")
		last := parts[len(parts)-1]
		if last == "" {
			return []string{name}
		}
		return stripLeadingTokens(last)
	}
	return stripLeadingTokens(name)
}

// stripLeadingTokens yields s and every suffix after an underscore boundary,
// most specific first: "u_exec" -> ["u_exec", "exec"], "workspace_run_command"
// -> ["workspace_run_command", "run_command"].
func stripLeadingTokens(s string) []string {
	var out []string
	for {
		out = append(out, s)
		i := strings.IndexByte(s, '_')
		if i <= 0 || i == len(s)-1 {
			return out
		}
		s = s[i+1:]
	}
}

// toolAliases resolves an upstream-native tool name to the client-side
// alias names that may match a declared tool, most specific first.  Plain
// names resolve to their own alias list (or, when the name appears as an
// alias of some key, that key's list); MCP-prefixed names are first
// normalized so the base tool token participates in the lookup.
func toolAliases(name string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, cand := range normalizeUpstreamToolName(name) {
		add(cand)
		if aliases := upstreamToClientAliases[cand]; len(aliases) > 0 {
			for _, a := range aliases {
				add(a)
			}
			continue
		}
		for key, aliases := range upstreamToClientAliases {
			for _, a := range aliases {
				if a == cand {
					for _, b := range upstreamToClientAliases[key] {
						add(b)
					}
					break
				}
			}
		}
	}
	return out
}

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
		matched := false
		for _, alias := range toolAliases(name) {
			if _, ok := declared[alias]; ok {
				out[i].Function.Name = alias
				log.Printf("[TRANSLATE] tool %q -> %q (client declared)", name, alias)
				matched = true
				break
			}
		}
		if !matched {
			log.Printf("[TRANSLATE] upstream tool %q not declared by client (aliases: %v)", name, toolAliases(name))
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
	for _, alias := range toolAliases(name) {
		if _, ok := declared[alias]; ok {
			log.Printf("[TRANSLATE] tool %q -> %q (client declared)", name, alias)
			return alias
		}
	}
	return name
}
