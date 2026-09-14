package server

import (
	"log"
	"strings"
)

// toolFilter maps upstream-emitted tool-call names onto the client's declared
// tool set.  The whitelist is built per-request from the client's `tools`
// payload (zero maintenance); the alias table is a small fixed set of
// upstream-native names (EdgeOne platform / Codex-style harness) mapped to
// common client names, and an alias only applies when its target is actually
// declared by the client.  Unresolvable names are dropped and logged, so the
// operator sees exactly which alias would need to be added.
//
// This closes the "model drifted into bash/str_replace_editor and the client
// answered 'Tool not found'" failure mode: the client never sees a tool call
// it did not declare, so it cannot feed back an error the model then
// misreads as a sandbox policy rejection.
type toolFilter struct {
	declared map[string]bool // client-declared tool names (exact)
	aliases  map[string]string
}

// upstreamAliases: upstream-native / platform tool names -> preferred client
// tool name.  Values are only used when present in the client's declared set.
var upstreamAliases = map[string]string{
	"bash":               "exec",
	"shell":              "exec",
	"sh":                 "exec",
	"zsh":                "exec",
	"python":             "exec",
	"python3":            "exec",
	"str_replace_editor": "read", // Codex-style file editor; "view" intent maps to read
	"text_editor":        "read",
	"read_file":          "read",
	"write_file":         "write",
	"glob":               "exec",
	"grep":               "exec",
	"code_search":        "exec",
}

// forbiddenPrefixes: upstream platform capabilities that must NEVER be
// surfaced to the client, alias or not.  The EdgeOne sandbox tool namespace
// (mcp__edgeone__*) is the platform's own runtime, not the user's machine;
// letting one through would re-introduce the exact drift we are closing.
var forbiddenPrefixes = []string{
	"mcp__edgeone__",
	"edgeone__",
	"workspace_run_command",
}

// newToolFilter builds a filter from the client's declared tools.  Only the
// exact declared names are whitelisted; aliases resolve only to declared
// names, otherwise the alias is inert.
func newToolFilter(tools []openaiTool) *toolFilter {
	f := &toolFilter{
		declared: make(map[string]bool, len(tools)),
		aliases:  make(map[string]string, len(upstreamAliases)),
	}
	for _, t := range tools {
		if n := strings.TrimSpace(t.Function.Name); n != "" {
			f.declared[n] = true
		}
	}
	// Only keep aliases whose target is declared by this client.
	for src, dst := range upstreamAliases {
		if f.declared[dst] {
			f.aliases[src] = dst
		}
	}
	return f
}

// resolve maps an upstream tool name to a client tool name.
// ok=false means the call must be dropped (not declared, no usable alias).
func (f *toolFilter) resolve(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	// Platform-native capabilities are always dropped, never aliased.
	for _, p := range forbiddenPrefixes {
		if strings.HasPrefix(name, p) {
			log.Printf("[TOOLS] filter: drop platform tool %q (forbidden prefix %q)", name, p)
			return "", false
		}
	}
	if f.declared[name] {
		return name, true
	}
	if dst, ok := f.aliases[name]; ok {
		log.Printf("[TOOLS] filter: alias %q -> %q", name, dst)
		return dst, true
	}
	return "", false
}

// filterCalls rewrites/drops a batch of parsed tool calls (non-streaming path
// and the ToolForge JSON-text fallback).  Returns the calls to surface to the
// client; nil when everything was dropped.
func (f *toolFilter) filterCalls(calls []openaiToolCall) []openaiToolCall {
	if f == nil || len(calls) == 0 {
		return calls
	}
	out := make([]openaiToolCall, 0, len(calls))
	for _, c := range calls {
		dst, ok := f.resolve(c.Function.Name)
		if !ok {
			log.Printf("[TOOLS] filter: drop tool %q (not declared by client)", c.Function.Name)
			continue
		}
		c.Function.Name = dst
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// filterStreamCall decides whether a streaming tool-call chunk should reach
// the client.  Returns the (possibly aliased) name when the call is
// acceptable, or "" when it must be suppressed.
func (f *toolFilter) filterStreamCall(name string) string {
	if f == nil {
		return name
	}
	dst, ok := f.resolve(name)
	if !ok {
		log.Printf("[TOOLS] filter: drop tool %q (not declared by client)", name)
		return ""
	}
	return dst
}

// hasDeclared reports whether the client declared at least one tool.
func (f *toolFilter) hasDeclared() bool {
	return f != nil && len(f.declared) > 0
}
