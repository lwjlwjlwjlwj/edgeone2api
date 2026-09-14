package server

import (
	"strings"
	"testing"
)

func declaredTools(names ...string) []openaiTool {
	var out []openaiTool
	for _, n := range names {
		out = append(out, openaiTool{
			Type:     "function",
			Function: openaiToolFunc{Name: n},
		})
	}
	return out
}

func TestToolFilterExactMatch(t *testing.T) {
	f := newToolFilter(declaredTools("exec", "read", "write"))
	if got, ok := f.resolve("exec"); !ok || got != "exec" {
		t.Fatalf("exact match failed: %q %v", got, ok)
	}
}

func TestToolFilterAliasToDeclared(t *testing.T) {
	// exec declared -> bash/sh/python/glob alias to exec
	f := newToolFilter(declaredTools("exec", "read"))
	cases := map[string]string{
		"bash":               "exec",
		"shell":              "exec",
		"python3":            "exec",
		"str_replace_editor": "read",
		"read_file":          "read",
	}
	for src, want := range cases {
		got, ok := f.resolve(src)
		if !ok || got != want {
			t.Fatalf("alias %q: got %q ok=%v want %q", src, got, ok, want)
		}
	}
}

func TestToolFilterAliasInertWhenTargetUndeclared(t *testing.T) {
	// Only "exec" declared: str_replace_editor -> read must NOT resolve
	// because read is not declared.
	f := newToolFilter(declaredTools("exec"))
	if _, ok := f.resolve("str_replace_editor"); ok {
		t.Fatal("alias to undeclared target must be inert")
	}
}

func TestToolFilterDropUnknown(t *testing.T) {
	f := newToolFilter(declaredTools("exec", "read"))
	if _, ok := f.resolve("mcp__edgeone__workspace_run_command"); ok {
		t.Fatal("platform tool must be dropped")
	}
	if _, ok := f.resolve("totally_made_up"); ok {
		t.Fatal("unknown tool must be dropped")
	}
	if _, ok := f.resolve(""); ok {
		t.Fatal("empty name must be dropped")
	}
}

func TestToolFilterFilterCalls(t *testing.T) {
	f := newToolFilter(declaredTools("exec", "read"))
	in := []openaiToolCall{
		{ID: "c1", Function: openaiToolCallFn{Name: "exec", Arguments: `{"command":"ls"}`}},
		{ID: "c2", Function: openaiToolCallFn{Name: "bash", Arguments: `{"command":"ls"}`}},
		{ID: "c3", Function: openaiToolCallFn{Name: "mcp__edgeone__workspace_run_command", Arguments: `{}`}},
		{ID: "c4", Function: openaiToolCallFn{Name: "str_replace_editor", Arguments: `{"command":"view","path":"/x"}`}},
	}
	out := f.filterCalls(in)
	if len(out) != 3 {
		t.Fatalf("want 3 calls kept, got %d: %+v", len(out), out)
	}
	if out[0].Function.Name != "exec" || out[1].Function.Name != "exec" || out[2].Function.Name != "read" {
		t.Fatalf("bad rewrite: %+v", out)
	}
	if out[0].ID != "c1" || out[1].ID != "c2" || out[2].ID != "c4" {
		t.Fatalf("ids must be preserved: %+v", out)
	}
}

func TestToolFilterFilterCallsAllDropped(t *testing.T) {
	f := newToolFilter(declaredTools("exec"))
	in := []openaiToolCall{
		{ID: "c1", Function: openaiToolCallFn{Name: "nope", Arguments: `{}`}},
		{ID: "c2", Function: openaiToolCallFn{Name: "mcp__edgeone__workspace_run_command", Arguments: `{}`}},
	}
	if out := f.filterCalls(in); len(out) != 0 {
		t.Fatalf("want all dropped, got %+v", out)
	}
}

func TestToolFilterNilFilterPassthrough(t *testing.T) {
	// nil filter (defensive): everything passes
	var f *toolFilter
	if got := f.filterStreamCall("anything"); got != "anything" {
		t.Fatalf("nil filter must pass through, got %q", got)
	}
}

func TestToolFilterStreamCall(t *testing.T) {
	f := newToolFilter(declaredTools("exec"))
	if got := f.filterStreamCall("bash"); got != "exec" {
		t.Fatalf("stream alias failed: %q", got)
	}
	if got := f.filterStreamCall("bash_not_real"); got != "" {
		t.Fatalf("stream drop failed: %q", got)
	}
}

func TestToolFilterManyAliasesCompile(t *testing.T) {
	// Guard: alias table keys are all non-empty and sorted sanity
	f := newToolFilter(declaredTools("exec", "read", "write"))
	for src, dst := range f.aliases {
		if strings.TrimSpace(src) == "" || strings.TrimSpace(dst) == "" {
			t.Fatalf("bad alias entry %q -> %q", src, dst)
		}
	}
}
