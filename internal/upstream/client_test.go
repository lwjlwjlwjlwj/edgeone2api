package upstream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

// The observed transient stall is "net/http: timeout awaiting response
// headers" — note it contains "timeout" but NOT "timed out", so it must not be
// classified as quota (which would burn a fingerprint for 24h).
func TestIsTransientErrorClassifiesHarnessStalls(t *testing.T) {
	transient := []string{
		`Post "https://x/api/session.selectModel": net/http: timeout awaiting response headers`,
		"context deadline exceeded",
		"dial tcp: i/o timeout",
		"read tcp: connection reset by peer",
		"unexpected EOF",
	}
	for _, m := range transient {
		if !IsTransientError(errorsNew(m)) {
			t.Errorf("expected transient: %q", m)
		}
	}
	permanent := []string{
		"session.prompt: status 400",
		"session.selectModel failed: invalid model",
		"authentication failed",
	}
	for _, m := range permanent {
		if IsTransientError(errorsNew(m)) {
			t.Errorf("did not expect transient: %q", m)
		}
	}
	if IsTransientError(nil) {
		t.Error("nil must not be transient")
	}
}

// A transient stall must never be mistaken for quota exhaustion (the two
// classifiers must be disjoint on that input), otherwise a single network blip
// would exhaust a browser fingerprint for 24h.
func TestTransientStallIsNotQuota(t *testing.T) {
	err := errorsNew(`Post "https://x/api/session.create": net/http: timeout awaiting response headers`)
	if !IsTransientError(err) {
		t.Fatal("stall must be transient")
	}
	if IsQuotaError(err) {
		t.Fatal("stall must NOT be classified as quota")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func errorsNew(s string) error { return errString(s) }

func TestIsTransientErrorIncludesStreamIdle(t *testing.T) {
	// The stream-idle guard produces this exact string; it must be retryable.
	err := errorsNew("stream idle timeout: no upstream event for 3m0s")
	if !IsTransientError(err) {
		t.Fatal("stream idle timeout must be transient")
	}
}

// A non-converging agent loop (endless chain of tool-call turns) must be cut
// off after a bounded number of folded turns instead of hanging the request.
func TestStreamEventsBoundsNonConvergingToolLoop(t *testing.T) {
	var chunks []SSEEnvelope
	for i := 1; i <= 40; i++ {
		chunks = append(chunks,
			env("turn/start", map[string]any{"turn": i}),
			chunkEnv(i, map[string]any{"type": "text-delta", "text": "x"}),
			chunkEnv(i, map[string]any{"type": "block-start", "blockType": "tool-call"}),
			env("turn/end", map[string]any{"turn": i}),
		)
	}

	// Feed on a goroutine so the bounded fold returns without the producer
	// blocking on a full (deadlocking) channel.
	envCh := make(chan SSEEnvelope)
	go func() {
		defer close(envCh)
		for _, e := range chunks {
			envCh <- e
		}
	}()
	c := &Client{}
	cs := &ChatStream{envCh: envCh}
	done := make(chan ChatResult, 1)
	go func() {
		res, _ := c.StreamEvents(context.Background(), cs, nil, 0)
		done <- res
	}()
	select {
	case res := <-done:
		if res.FinishReason != "stop" {
			t.Fatalf("finish reason must be stop, got %q", res.FinishReason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("non-converging loop must be bounded, but StreamEvents hung")
	}
}
