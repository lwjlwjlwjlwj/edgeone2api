package server

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"edgeone2api/internal/toolcall"
)

const testToolsJSON = `[{"type":"function","function":{"name":"exec","description":"run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}]`

// userReportedSample is the exact broken tool_calls text the user pasted from
// their OpenClaw run: an arguments object wrapped in an unescaped outer
// string with only partially-escaped inner quotes.  It is not deterministically
// repairable — the migrated toolforge engine must report it as truncated so the
// gateway issues a corrective turn instead of leaking broken text.
const userReportedSample = `{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec","arguments":"{"command":"KEY=$(python3 -c \"import sqlite3; db=sqlite3.connect('/home/ist/new-api-deploy/data/one-api.db'); row=db.execute('SELECT key FROM tokens WHERE status=1 AND deleted_at IS NULL ORDER BY id LIMIT 1').fetchone(); print(row[0] if row else '')"); if [ -z \"$KEY\" ]; then echo NO_ACTIVE_TOKEN; else echo TOKEN_FOUND_LEN=${#KEY}; curl -s -m 10 -H \"Authorization: Bearer $KEY\" http://127.0.0.1:3030/v1/models | python3 -c \"import sys,json; d=json.load(sys.stdin); ids=[m.get('id') for m in d.get('data',[])]; print('MODELS:', ids); print('HAS_SENSENOVA:', any('sensenova' in str(i).lower() for i in ids))\"; fi"}"}}]}`

// --- firstToolMarker (stream sieve marker detection) ---

func TestFirstToolMarker(t *testing.T) {
	cases := []struct {
		in    string
		want  int
		empty bool // -1
	}{
		{`<|XYML|tool_calls>`, 0, false},
		{`<:QNML:invoke name="x">`, 0, false},
		{`<|invoke name="exec">`, 0, false},
		{`<tool_calls>`, 0, false},
		{`<tool_use>`, 0, false},
		{`{"tool_calls":[{`, 0, false},
		{`"tool_calls":`, -1, true},
		{`function.name: bash`, 0, false},
		{"你好，我来帮你。", -1, true},
		{"Sure, let me check that for you:\n<|XYML|tool_calls>\n<|XYML|invoke name=\"exec\">", 33, false},
		{"```json\n{\"tool_calls\":[", 0, false},
		{"prefix\n```\n<|XYML|invoke", 11, false},
	}
	for _, c := range cases {
		got := firstToolMarker(c.in)
		if c.empty {
			if got != -1 {
				t.Fatalf("firstToolMarker(%q) = %d, want -1", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("firstToolMarker(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// --- streamSieve ---

func TestStreamSieveProseStreamsLive(t *testing.T) {
	s := newStreamSieve()
	var emitted strings.Builder
	for _, ch := range []string{"你好", "，我来", "帮你。"} {
		emitted.WriteString(s.feed(ch))
	}
	held, rest := s.flush()
	if held != "" {
		t.Fatalf("held should be empty for prose, got %q", held)
	}
	if strings.TrimSpace(rest) == "" {
		t.Fatalf("prose must be flushed as rest")
	}
	out := emitted.String() + rest
	if !strings.Contains(out, "你好，我来帮你。") {
		t.Fatalf("prose lost: %q", out)
	}
}

func TestStreamSieveCapturesEnvelope(t *testing.T) {
	s := newStreamSieve()
	var emitted strings.Builder
	chunks := []string{
		"<|XYML|tool_calls>",
		"\n<|XYML|invoke name=\"exec\">",
		"\n<|XYML|parameter name=\"command\"><![CDATA[",
		`echo "hi"`,
		"]]></|XYML|parameter>\n</|XYML|invoke>\n</|XYML|tool_calls>",
	}
	for _, ch := range chunks {
		if e := s.feed(ch); e != "" {
			emitted.WriteString(e)
		}
	}
	if emitted.Len() != 0 {
		t.Fatalf("envelope should not stream live, emitted %q", emitted.String())
	}
	held, rest := s.flush()
	if rest != "" {
		t.Fatalf("unexpected rest %q", rest)
	}
	if !strings.Contains(held, "tool_calls") || !strings.Contains(held, "CDATA") {
		t.Fatalf("held envelope incomplete: %q", held)
	}
}

func TestStreamSieveMarkerAcrossChunkBoundary(t *testing.T) {
	s := newStreamSieve()
	// The <<|XYML|tool_calls> marker arrives split across chunks; a naive
	// per-chunk check would leak "<|X" as content.
	out := s.feed("<|X") + s.feed("YML|tool_calls>\n<|XYML|invoke name=\"exec\">")
	if out != "" {
		t.Fatalf("split marker leaked content: %q", out)
	}
	held, rest := s.flush()
	if rest != "" {
		t.Fatalf("split marker produced rest %q", rest)
	}
	if !strings.HasPrefix(held, "<|XYML|tool_calls>") {
		t.Fatalf("held must start at the marker, got %q", held)
	}
}

func TestStreamSieveProseThenEnvelope(t *testing.T) {
	s := newStreamSieve()
	var emitted strings.Builder
	emitted.WriteString(s.feed("Let me check"))
	emitted.WriteString(s.feed(" that:\n<|XYML|tool_calls>"))
	emitted.WriteString(s.feed("\n<|XYML|invoke name=\"exec\">"))
	if !strings.Contains(emitted.String(), "Let me check that:") {
		t.Fatalf("prose before envelope must stream, got %q", emitted.String())
	}
	held, _ := s.flush()
	if !strings.HasPrefix(held, "<|XYML|tool_calls>") {
		t.Fatalf("envelope not captured whole: %q", held)
	}
}

func TestStreamSieveDrainForNativeTool(t *testing.T) {
	s := newStreamSieve()
	s.feed("prose<|XYML|tool_calls>")
	s.feed("<|XYML|invoke name=\"exec\">")
	drained := s.drainForNativeTool()
	if !strings.Contains(drained, "tool_calls") {
		t.Fatalf("drain must release held envelope, got %q", drained)
	}
	held, _ := s.flush()
	if held != "" {
		t.Fatalf("after drain nothing should be held, got %q", held)
	}
}

// --- sidecar integration (skipped when python is unavailable) ---

func requireSidecar(t *testing.T) *toolcall.Client {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	// A previous test may have left a sidecar running on the port.
	_ = exec.Command("pkill", "-f", "internal/toolcall/server.py").Run()
	time.Sleep(200 * time.Millisecond)
	tc := toolcall.NewClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := tc.Start(ctx, 15*time.Second); err != nil {
		t.Fatalf("sidecar start: %v", err)
	}
	t.Cleanup(tc.Stop)
	return tc
}

func TestSidecarParseXYML(t *testing.T) {
	tc := requireSidecar(t)
	ctx := context.Background()
	raw := "<|XYML|tool_calls>\n<|XYML|invoke name=\"exec\">\n<|XYML|parameter name=\"command\"><![CDATA[echo \"hi\"]]></|XYML|parameter>\n</|XYML|invoke>\n</|XYML|tool_calls>"
	calls, err := tc.ParseToolCalls(ctx, raw, []byte(testToolsJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.Function.Name != "exec" {
		t.Fatalf("expected name exec, got %q", c.Function.Name)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments %q not valid JSON: %v", c.Function.Arguments, err)
	}
	if args["command"] != `echo "hi"` {
		t.Fatalf("unexpected command %q", args["command"])
	}
}

func TestSidecarRecoverUserReportedBrokenJSON(t *testing.T) {
	tc := requireSidecar(t)
	// The user's broken sample must NOT be repairable deterministically:
	// the engine reports it as truncated so the gateway issues a corrective
	// turn instead of inventing a call.
	calls, reason, err := tc.RecoverToolCalls(context.Background(), userReportedSample, []byte(testToolsJSON))
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("broken sample must not yield calls, got %+v", calls)
	}
	if reason == "" {
		t.Fatalf("broken sample must return a recovery reason")
	}
	msg, err := tc.RetryMessage(context.Background(), userReportedSample, reason)
	if err != nil || strings.TrimSpace(msg) == "" {
		t.Fatalf("retry message failed: %v %q", err, msg)
	}
}

func TestSidecarParseUnknownToolKept(t *testing.T) {
	tc := requireSidecar(t)
	// Gateway parse policy keeps unknown tool names so the Go translation
	// layer can alias native names (bash -> client's exec) later.
	raw := `<|XYML|tool_calls><|XYML|invoke name="bash"><|XYML|parameter name="command"><![CDATA[ls]]></|XYML|parameter></|XYML|invoke></|XYML|tool_calls>`
	calls, err := tc.ParseToolCalls(context.Background(), raw, []byte(testToolsJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Fatalf("unknown tool must be kept for translation, got %+v", calls)
	}
}

func TestSidecarRenderHistory(t *testing.T) {
	tc := requireSidecar(t)
	messages := `[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"exec","arguments":"{\"command\":\"ls\"}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"file1 file2"}
	]`
	items, err := tc.RenderHistory(context.Background(), []byte(messages))
	if err != nil {
		t.Fatalf("render history: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d: %+v", len(items), items)
	}
	var joined string
	for _, it := range items {
		joined += it.Role + ":" + it.Content + "\n"
	}
	if !strings.Contains(joined, "[Tool Result id=c1]") {
		t.Fatalf("tool result must be flattened with its id:\n%s", joined)
	}
	if !strings.Contains(joined, "<|XYML|tool_calls>") {
		t.Fatalf("assistant tool_calls must be rendered as XYML:\n%s", joined)
	}
}

func TestSidecarInstructionsIncludeProfile(t *testing.T) {
	tc := requireSidecar(t)
	ins, err := tc.BuildInstructions(context.Background(), []byte(testToolsJSON))
	if err != nil {
		t.Fatalf("instructions: %v", err)
	}
	if !strings.Contains(ins.Instructions, "XYML TOOL CALL PROTOCOL") {
		t.Fatalf("instructions must contain the XYML protocol block, got:\n%s", ins.Instructions)
	}
	if ins.Profile.ID == "" {
		t.Fatalf("tool profile must be detected, got %+v", ins.Profile)
	}
}