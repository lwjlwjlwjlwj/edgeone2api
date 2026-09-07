package server

import (
	"encoding/json"
	"testing"
)

// userReportedSample is the exact tool_calls text the user pasted from their
// OpenClaw run: the arguments object was wrapped in an unescaped outer string
// ("arguments":"{...}") AND the inner object's quotes are only partially
// escaped.  Such a document cannot be repaired deterministically — parseToolCalls
// must return nil so the callers fall back to the corrective-retry path.
const userReportedSample = `{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec","arguments":"{"command":"KEY=$(python3 -c \"import sqlite3; db=sqlite3.connect('/home/ist/new-api-deploy/data/one-api.db'); row=db.execute('SELECT key FROM tokens WHERE status=1 AND deleted_at IS NULL ORDER BY id LIMIT 1').fetchone(); print(row[0] if row else '')"); if [ -z \"$KEY\" ]; then echo NO_ACTIVE_TOKEN; else echo TOKEN_FOUND_LEN=${#KEY}; curl -s -m 10 -H \"Authorization: Bearer $KEY\" http://127.0.0.1:3030/v1/models | python3 -c \"import sys,json; d=json.load(sys.stdin); ids=[m.get('id') for m in d.get('data',[])]; print('MODELS:', ids); print('HAS_SENSENOVA:', any('sensenova' in str(i).lower() for i in ids))\"; fi"}"}}]}`

func TestParseToolCallsObjectArguments(t *testing.T) {
	raw := `{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec","arguments":{"command":"echo \"hi\""}}}]}`
	calls := parseToolCalls(raw)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	c := calls[0]
	if c.Function.Name != "exec" {
		t.Fatalf("expected name exec, got %q", c.Function.Name)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments %q is not valid JSON: %v", c.Function.Arguments, err)
	}
	if args["command"] != `echo "hi"` {
		t.Fatalf("unexpected command value %q", args["command"])
	}
}

func TestParseToolCallsLegacyStringArguments(t *testing.T) {
	raw := `{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/x\"}"}}]}`
	calls := parseToolCalls(raw)
	if len(calls) != 1 || calls[0].Function.Name != "read_file" {
		t.Fatalf("unexpected calls: %+v", calls)
	}
	if calls[0].Function.Arguments != `{"path":"/x"}` {
		t.Fatalf("string arguments must pass through untouched, got %q", calls[0].Function.Arguments)
	}
}

func TestParseToolCallsUserReportedWrappedObject(t *testing.T) {
	if calls := parseToolCalls(userReportedSample); calls != nil {
		t.Fatalf("partially-escaped wrapped object must NOT be repairable deterministically, got %+v", calls)
	}
}

// TestParseToolCallsWrappedValidObject covers the cleanly-repairable variant:
// an arguments object wrapped in an outer string whose inner object is itself
// valid JSON.  unwrapWrappedArguments strips the wrapping quotes and the
// object-form parser takes over.
func TestParseToolCallsWrappedValidObject(t *testing.T) {
	raw := `{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec","arguments":"{"command":"echo \"hi\""}"}}]}`
	calls := parseToolCalls(raw)
	if len(calls) != 1 || calls[0].Function.Name != "exec" {
		t.Fatalf("wrapped-valid-object must parse after unwrap, got %+v", calls)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments %q are not valid JSON: %v", calls[0].Function.Arguments, err)
	}
	if args["command"] != `echo "hi"` {
		t.Fatalf("unexpected command value %q", args["command"])
	}
}

func TestParseToolCallsCodeFence(t *testing.T) {
	raw := "```json\n" + `{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"glob","arguments":{"pattern":"**/*.go"}}}]}` + "\n```"
	calls := parseToolCalls(raw)
	if len(calls) != 1 || calls[0].Function.Name != "glob" {
		t.Fatalf("fenced tool_calls must parse, got %+v", calls)
	}
}

func TestParseToolCallsNotJSONReturnsNil(t *testing.T) {
	for _, s := range []string{"", "你好", "I cannot execute tools.", "```json\n{broken"} {
		if calls := parseToolCalls(s); calls != nil {
			t.Fatalf("expected nil for %q, got %+v", s, calls)
		}
	}
}

func TestParseToolCallsMultipleCallsKeepsOrder(t *testing.T) {
	raw := `{"tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"glob","arguments":{"pattern":"*.go"}}},
		{"id":"call_2","type":"function","function":{"name":"read_file","arguments":{"path":"main.go"}}}
	]}`
	calls := parseToolCalls(raw)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "glob" || calls[1].Function.Name != "read_file" {
		t.Fatalf("call order lost: %+v", calls)
	}
}

func TestParseToolCallsAssignsMissingID(t *testing.T) {
	raw := `{"tool_calls":[{"type":"function","function":{"name":"exec","arguments":{"command":"ls"}}}]}`
	calls := parseToolCalls(raw)
	if len(calls) != 1 || calls[0].ID == "" {
		t.Fatalf("missing id must be generated, got %+v", calls)
	}
}

func TestParseToolCallsNullArgumentsBecomesEmptyObject(t *testing.T) {
	raw := `{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"foo","arguments":null}}]}`
	calls := parseToolCalls(raw)
	if len(calls) != 1 || calls[0].Function.Arguments != "{}" {
		t.Fatalf("null arguments should become {}, got %+v", calls)
	}
}

func TestJsonish(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`{"tool_calls":[]}`, true},
		{"```json\n{\"a\":1}\n```", true},
		{`[1,2]`, true},
		{"hello world", false},
		{"", false},
		{"  # comment", false},
	}
	for _, c := range cases {
		if got := jsonish(c.in); got != c.want {
			t.Fatalf("jsonish(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
