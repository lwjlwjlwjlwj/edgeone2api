package upstream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func env(eventType string, data map[string]any) SSEEnvelope {
	se := map[string]any{
		"sessionId": "s1",
		"event": map[string]any{
			"type": eventType,
			"seq":  0,
			"time": 0,
			"data": data,
		},
	}
	raw, _ := json.Marshal(se)
	return SSEEnvelope{Type: "server-request", RpcID: "rpc-1", Method: "session/event", Payload: raw}
}

func chunkEnv(turn int, chunk map[string]any) SSEEnvelope {
	return env("assistant/chunk", map[string]any{
		"turn":  turn,
		"step":  0,
		"chunk": chunk,
	})
}

func runEvents(t *testing.T, chunks []SSEEnvelope) ChatResult {
	t.Helper()
	envCh := make(chan SSEEnvelope, 32)
	for _, e := range chunks {
		envCh <- e
	}
	close(envCh)
	c := &Client{}
	cs := &ChatStream{envCh: envCh}
	res, err := c.StreamEvents(context.Background(), cs, nil, 0)
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	return res
}

// The upstream is always a plain-text black box: a native tool-call block is
// folded away, the finish reason is normalized to "stop", and the tool-driven
// turn does not terminate the round (follow-up text still flows through).
func TestStreamEventsFoldsToolCallsAcrossTurns(t *testing.T) {
	chunks := []SSEEnvelope{
		env("turn/start", map[string]any{"turn": 1}),
		chunkEnv(1, map[string]any{"type": "text-delta", "text": "Let me check."}),
		chunkEnv(1, map[string]any{"type": "block-start", "blockType": "tool-call"}),
		chunkEnv(1, map[string]any{"type": "tool-call-delta", "index": 0, "id": "t_1", "name": "read", "argumentsDelta": `{"path":"/x"}`}),
		chunkEnv(1, map[string]any{"type": "finish", "reason": map[string]any{"kind": "tool-calls"}}),
		env("turn/end", map[string]any{"turn": 1}),
		env("turn/start", map[string]any{"turn": 2}),
		chunkEnv(2, map[string]any{"type": "text-delta", "text": "Final answer."}),
		env("turn/end", map[string]any{"turn": 2}),
	}
	res := runEvents(t, chunks)
	if res.Text != "Let me check.Final answer." {
		t.Fatalf("plain-text mode must keep consuming follow-up turns, got %q", res.Text)
	}
	if res.FinishReason != "stop" {
		t.Fatalf("finish reason must normalize to stop, got %q", res.FinishReason)
	}
}

func TestStreamEventsCleanTurnStopsImmediately(t *testing.T) {
	chunks := []SSEEnvelope{
		env("turn/start", map[string]any{"turn": 1}),
		chunkEnv(1, map[string]any{"type": "text-delta", "text": "Direct answer"}),
		env("turn/end", map[string]any{"turn": 1}),
		env("turn/start", map[string]any{"turn": 2}),
		chunkEnv(2, map[string]any{"type": "text-delta", "text": "should-not-appear"}),
		env("turn/end", map[string]any{"turn": 2}),
	}
	res := runEvents(t, chunks)
	if res.Text != "Direct answer" {
		t.Fatalf("a clean turn must return at turn/end, got %q", res.Text)
	}
}

func TestStreamEventsReasoningCaptured(t *testing.T) {
	chunks := []SSEEnvelope{
		env("turn/start", map[string]any{"turn": 1}),
		chunkEnv(1, map[string]any{"type": "reasoning-delta", "text": "thinking..."}),
		chunkEnv(1, map[string]any{"type": "text-delta", "text": "answer"}),
		env("turn/end", map[string]any{"turn": 1}),
	}
	res := runEvents(t, chunks)
	if res.Reasoning != "thinking..." {
		t.Fatalf("reasoning must be preserved, got %q", res.Reasoning)
	}
	if res.Text != "answer" {
		t.Fatalf("text must be preserved, got %q", res.Text)
	}
}

func TestBuildDirectiveNoToolsSuppressesAgentLoop(t *testing.T) {
	d := BuildDirective("")
	for _, want := range []string{"NEVER emit tool calls", "single turn only", `finish_reason "tool_calls"`} {
		if !strings.Contains(d, want) {
			t.Fatalf("directive missing %q:\n%s", want, d)
		}
	}
}

func TestBuildDirectiveWithToolsKeepsProtocol(t *testing.T) {
	d := BuildDirective(`[{"type":"function","function":{"name":"read_file","description":"read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]`)
	if !strings.Contains(d, "- read_file:") {
		t.Fatalf("tools directive must list declared tool names:\n%s", d)
	}
	if !strings.Contains(d, TCStart) || !strings.Contains(d, TCArgsS) {
		t.Fatalf("tools directive must carry the delimiter protocol:\n%s", d)
	}
	// Compact schema (no spaces) so the model treats it as a machine template.
	if !strings.Contains(d, `"properties":{"path":{"type":"string"}}`) {
		t.Fatalf("tools directive must render the parameters schema compactly:\n%s", d)
	}
	if strings.Contains(d, "NEVER emit tool calls") {
		t.Fatalf("tools directive must enable the tool protocol, not forbid it:\n%s", d)
	}
}

func TestBuildDirectiveUnparseableToolsFallsBack(t *testing.T) {
	d := BuildDirective("not-json")
	if !strings.Contains(d, "NEVER emit tool calls") {
		t.Fatalf("unparseable tools must fall back to the plain directive:\n%s", d)
	}
}
