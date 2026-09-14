package server

import (
	"encoding/json"
	"testing"

	"edgeone2api/internal/upstream"
)

func tc(name, args string) string {
	return upstream.TCStart + "\n" +
		upstream.TCNameS + name + upstream.TCNameE + "\n" +
		upstream.TCArgsS + args + upstream.TCArgsE + "\n" +
		upstream.TCEnd
}

func TestParseToolCallsPlainTextPassthrough(t *testing.T) {
	text := "现在不能查询时间。"
	content, calls := parseToolCalls(text)
	if content != text || len(calls) != 0 {
		t.Fatalf("plain text must pass through unchanged, got %q / %+v", content, calls)
	}
}

func TestParseToolCallsExtractsCallAndStripsBlock(t *testing.T) {
	text := "我来查询。\n\n" + tc("get_time", `{"timezone":"Asia/Shanghai"}`)
	content, calls := parseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("expected one call, got %+v", calls)
	}
	if calls[0].Function.Name != "get_time" {
		t.Fatalf("wrong name: %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"timezone":"Asia/Shanghai"}` {
		t.Fatalf("wrong arguments: %q", calls[0].Function.Arguments)
	}
	if calls[0].ID == "" || calls[0].Type != "function" {
		t.Fatalf("call must carry an id and type=function: %+v", calls[0])
	}
	if content != "我来查询。" {
		t.Fatalf("head text must be retained without the block, got %q", content)
	}
}

func TestParseToolCallsMultipleBlocks(t *testing.T) {
	text := tc("read_file", `{"path":"/a"}`) + "\n" + tc("run", `{"cmd":"ls"}`)
	_, calls := parseToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("expected two calls, got %+v", calls)
	}
	if calls[0].Function.Name != "read_file" || calls[1].Function.Name != "run" {
		t.Fatalf("names/order wrong: %+v", calls)
	}
}

// A block whose arguments are not a JSON object is dropped, so ordinary prose
// that happens to contain the delimiters can never be mistaken for a call.
func TestParseToolCallsRejectsNonObjectArguments(t *testing.T) {
	_, calls := parseToolCalls(tc("x", `not json`))
	if len(calls) != 0 {
		t.Fatalf("non-object arguments must be rejected, got %+v", calls)
	}
	_, calls = parseToolCalls(tc("x", `[1,2]`))
	if len(calls) != 0 {
		t.Fatalf("array arguments must be rejected, got %+v", calls)
	}
}

func TestParseToolCallsIgnoresDelimiterWithoutName(t *testing.T) {
	text := "正文 " + upstream.TCStart + " 后面还有话"
	content, calls := parseToolCalls(text)
	if len(calls) != 0 {
		t.Fatalf("a lone delimiter must not yield a call, got %+v", calls)
	}
	if content != text {
		t.Fatalf("text must be returned unchanged, got %q", content)
	}
}

func TestToolChoiceNone(t *testing.T) {
	cases := map[string]bool{
		`"none"`:              true,
		` "none" `:            true,
		`"auto"`:              false,
		`"required"`:          false,
		`{"type":"function"}`: false,
		``:                    false,
	}
	for in, want := range cases {
		if got := toolChoiceNone(json.RawMessage(in)); got != want {
			t.Errorf("toolChoiceNone(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPartialDelimSuffix(t *testing.T) {
	r := []rune(upstream.TCStart)
	full := string(r)
	cases := []struct {
		s    string
		want int
	}{
		{"plain text", 0},
		{string(r[:len(r)-1]), 6}, // first two codepoints (3 bytes each)
		{string(r[:1]), 3},
		{"x" + full, 0}, // a complete delimiter is not a partial suffix
	}
	for _, c := range cases {
		if got := partialDelimSuffix(c.s, upstream.TCStart); got != c.want {
			t.Errorf("partialDelimSuffix(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}
