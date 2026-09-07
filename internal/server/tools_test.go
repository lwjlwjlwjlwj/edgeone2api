package server

import "testing"

func declared(names ...string) declaredToolSet {
	d := declaredToolSet{}
	for _, n := range names {
		d[n] = openaiTool{}
	}
	return d
}

func TestTranslateToolCallsRewritesUpstreamName(t *testing.T) {
	calls := []openaiToolCall{{
		ID: "call_1", Type: "function",
		Function: openaiToolCallFn{Name: "read", Arguments: `{"path":"/x"}`},
	}}
	out := translateToolCalls(calls, declared("read_file"))
	if len(out) != 1 || out[0].Function.Name != "read_file" {
		t.Fatalf("expected read -> read_file, got %+v", out)
	}
	if out[0].Function.Arguments != `{"path":"/x"}` {
		t.Fatalf("arguments must pass through untouched, got %q", out[0].Function.Arguments)
	}
}

func TestTranslateToolCallsPassesDeclaredNameThrough(t *testing.T) {
	calls := []openaiToolCall{{
		ID: "call_1", Type: "function",
		Function: openaiToolCallFn{Name: "read_file", Arguments: "{}"},
	}}
	out := translateToolCalls(calls, declared("read_file"))
	if len(out) != 1 || out[0].Function.Name != "read_file" {
		t.Fatalf("declared name must pass through, got %+v", out)
	}
}

func TestTranslateToolCallsUnmappedNamePassesThrough(t *testing.T) {
	calls := []openaiToolCall{{
		ID: "call_1", Type: "function",
		Function: openaiToolCallFn{Name: "mystery_tool", Arguments: "{}"},
	}}
	out := translateToolCalls(calls, declared("read_file"))
	if len(out) != 1 || out[0].Function.Name != "mystery_tool" {
		t.Fatalf("unmapped name must pass through unchanged, got %+v", out)
	}
}

func TestTranslateToolCallsNoDeclaredToolsIsNoOp(t *testing.T) {
	calls := []openaiToolCall{{
		ID: "call_1", Type: "function",
		Function: openaiToolCallFn{Name: "read", Arguments: "{}"},
	}}
	out := translateToolCalls(calls, nil)
	if len(out) != 1 || out[0].Function.Name != "read" {
		t.Fatalf("no declared tools must disable translation, got %+v", out)
	}
}

func TestTranslateToolCallNameRewrites(t *testing.T) {
	if got := translateToolCallName("read", declared("read_file")); got != "read_file" {
		t.Fatalf("expected read_file, got %q", got)
	}
	if got := translateToolCallName("read_file", declared("read_file")); got != "read_file" {
		t.Fatalf("declared name must pass through, got %q", got)
	}
	if got := translateToolCallName("nope", declared("read_file")); got != "nope" {
		t.Fatalf("unmapped name must pass through, got %q", got)
	}
}

func TestTranslateToolCallsMCPPrefixedRunCommand(t *testing.T) {
	calls := []openaiToolCall{{
		ID: "call_1", Type: "function",
		Function: openaiToolCallFn{Name: "mcp__edgeone__workspace_run_command", Arguments: `{"command":"ls -la /tmp"}`},
	}}
	out := translateToolCalls(calls, declared("exec"))
	if len(out) != 1 || out[0].Function.Name != "exec" {
		t.Fatalf("expected mcp__edgeone__workspace_run_command -> exec, got %+v", out)
	}
	if out[0].Function.Arguments != `{"command":"ls -la /tmp"}` {
		t.Fatalf("arguments must pass through untouched, got %q", out[0].Function.Arguments)
	}
}

func TestTranslateToolCallsMCPPrefixedReadFile(t *testing.T) {
	calls := []openaiToolCall{{
		ID: "call_1", Type: "function",
		Function: openaiToolCallFn{Name: "mcp__edgeone__workspace_read_file", Arguments: `{"path":"/x"}`},
	}}
	out := translateToolCalls(calls, declared("read_file"))
	if len(out) != 1 || out[0].Function.Name != "read_file" {
		t.Fatalf("expected mcp__edgeone__workspace_read_file -> read_file, got %+v", out)
	}
}

func TestTranslateToolCallsUnifiedExec(t *testing.T) {
	calls := []openaiToolCall{{
		ID: "call_1", Type: "function",
		Function: openaiToolCallFn{Name: "u_exec", Arguments: `{}`},
	}}
	out := translateToolCalls(calls, declared("exec"))
	if len(out) != 1 || out[0].Function.Name != "exec" {
		t.Fatalf("expected u_exec -> exec, got %+v", out)
	}
}

func TestTranslateToolCallNameMCPPrefixed(t *testing.T) {
	if got := translateToolCallName("mcp__edgeone__workspace_run_command", declared("run_command")); got != "run_command" {
		t.Fatalf("expected run_command, got %q", got)
	}
	if got := translateToolCallName("mcp__edgeone__workspace_glob", declared("glob_files")); got != "glob_files" {
		t.Fatalf("expected glob_files, got %q", got)
	}
}
