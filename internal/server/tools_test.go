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
