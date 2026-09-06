package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPool() *Pool {
	return NewPool(PoolConfig{MinSize: 0, MaxSize: 1})
}

func TestReplayHistorySameSessionReturnsNil(t *testing.T) {
	p := testPool()
	p.AppendHistory("k", "sA", "hello", "hi")
	if turns := p.ReplayHistory("k", "sA"); turns != nil {
		t.Fatalf("same session should hit live context, got %v", turns)
	}
}

func TestReplayHistoryFreshSessionGetsFullHistory(t *testing.T) {
	p := testPool()
	p.AppendHistory("k", "sA", "u1", "a1")
	p.AppendHistory("k", "sA", "u2", "a2")

	turns := p.ReplayHistory("k", "sB")
	if len(turns) != 4 {
		t.Fatalf("expected 4 turns replayed into fresh session, got %d: %v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "u1" || turns[3].Role != "assistant" || turns[3].Text != "a2" {
		t.Fatalf("replay order wrong: %v", turns)
	}

	again := p.ReplayHistory("k", "sC")
	if len(again) != 4 {
		t.Fatalf("cache must persist across replays until a new turn is appended, got %d", len(again))
	}
}

func TestReplayHistoryColdKeyReturnsNil(t *testing.T) {
	p := testPool()
	if turns := p.ReplayHistory("fresh-key", "sX"); turns != nil {
		t.Fatalf("cold key must not replay, got %v", turns)
	}
}

func TestAppendHistoryMarksServingSession(t *testing.T) {
	p := testPool()
	p.AppendHistory("k", "sA", "u1", "a1")
	p.ReplayHistory("k", "sB") // fresh session inherits context
	p.AppendHistory("k", "sB", "u2", "a2")
	if turns := p.ReplayHistory("k", "sB"); turns != nil {
		t.Fatalf("session sB now carries live context, must not replay, got %v", turns)
	}
	turns := p.ReplayHistory("k", "sD")
	if len(turns) != 4 {
		t.Fatalf("expected 4 turns after two requests, got %d", len(turns))
	}
}

func TestAppendHistoryEmptyUserKeepsMarkerOnly(t *testing.T) {
	p := testPool()
	p.AppendHistory("k", "sA", "", "ignored")
	if len(p.cache["k"]) != 0 {
		t.Fatalf("empty user text must not append turns, got %v", p.cache["k"])
	}
	if p.lastSrv["k"] != "sA" {
		t.Fatalf("serving session marker must still be updated")
	}
}

func TestExhaustFingerprintCooldown(t *testing.T) {
	p := testPool()
	p.exhaustFingerprint("sig1")
	if !p.fingerprintCooling("sig1") {
		t.Fatalf("sig1 must be in cooldown after exhaustion")
	}
	if p.fingerprintCooling("sig2") {
		t.Fatalf("sig2 must not be cooling")
	}
	p.exhaustFingerprint("sig1")
	p.fpMu.Lock()
	n := len(p.exhaustedFP)
	p.fpMu.Unlock()
	if n != 1 {
		t.Fatalf("exhaustion must be idempotent, got %d records", n)
	}
}

func TestExhaustFingerprintExpiry(t *testing.T) {
	p := testPool()
	p.exhaustFingerprint("sig1")
	p.fpMu.Lock()
	p.exhaustedFP["sig1"] = time.Now().Add(-25 * time.Hour)
	p.fpMu.Unlock()
	if p.fingerprintCooling("sig1") {
		t.Fatalf("sig1 cooldown must have expired after 24h")
	}
	p.cleanupExhausted()
	p.fpMu.Lock()
	n := len(p.exhaustedFP)
	p.fpMu.Unlock()
	if n != 0 {
		t.Fatalf("cleanup must remove expired records, got %d", n)
	}
}

func TestReplayCap(t *testing.T) {
	p := testPool()
	p.AppendHistory("k", "sA", "u1", "a1")
	p.AppendHistory("k", "sA", "u2", "a2")
	p.AppendHistory("k", "sA", "u3", "a3")
	p.AppendHistory("k", "sA", "u4", "a4")
	p.AppendHistory("k", "sA", "u5", "a5")
	p.AppendHistory("k", "sA", "u6", "a6")
	turns := p.ReplayHistory("k", "sB")
	if len(turns) != maxReplayTurns {
		t.Fatalf("replay must be capped at %d turns, got %d", maxReplayTurns, len(turns))
	}
	if turns[0].Text != "u2" {
		t.Fatalf("cap must keep the newest turns, first=%q", turns[0].Text)
	}
}

func TestCreateSessionQuotaExhaustsFingerprints(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session.create", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"type":"server-response","rpcId":"rpc-1","result":{"ok":false,"error":{"code":"rate_limit","message":"rate limit exceeded, try later"}}}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := NewPool(PoolConfig{
		MinSize:     0,
		MaxSize:     1,
		UpstreamURL: ts.URL,
		AgentPreset: "minimal",
	})

	_, err := p.createSession()
	if err == nil {
		t.Fatalf("createSession must fail against a quota-limit upstream")
	}

	p.fpMu.Lock()
	n := len(p.exhaustedFP)
	p.fpMu.Unlock()
	if n == 0 {
		t.Fatalf("quota failures must exhaust the drawn fingerprints, got %d records", n)
	}
	if !p.fingerprintCooling(pickSig(p)) {
		t.Fatalf("at least one exhausted fingerprint must be cooling")
	}
}

func pickSig(p *Pool) string {
	p.fpMu.Lock()
	defer p.fpMu.Unlock()
	for sig := range p.exhaustedFP {
		return sig
	}
	return ""
}

// fakeUpstream returns a pool-friendly upstream stub: session.create sleeps
// briefly to emulate RPC latency, events.mux streams a single turn/end so
// InitSession completes, and session.prompt accepts.  inflight/max track the
// observed concurrency of session.create for parallelism assertions.
func fakeUpstream(t *testing.T, createDelay time.Duration, inflight, max *atomic.Int32) *httptest.Server {
	t.Helper()
	var seq atomic.Int32 // monotonic session id source
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session.create", func(w http.ResponseWriter, r *http.Request) {
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			m := max.Load()
			if cur <= m || max.CompareAndSwap(m, cur) {
				break
			}
		}
		if createDelay > 0 {
			time.Sleep(createDelay)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":"server-response","rpcId":"r","result":{"ok":true,"value":{"sessionId":"s-%d"}}}`, seq.Add(1))
	})
	mux.HandleFunc("/api/events.mux", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", sseEvent("turn/start", `{"turn":1}`))
		fmt.Fprintf(w, "data: %s\n\n", sseEvent("turn/end", `{"turn":1}`))
	})
	mux.HandleFunc("/api/session.prompt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"server-response","rpcId":"r","result":{"ok":true,"value":{"accepted":true}}}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func sseEvent(evType, dataJSON string) string {
	env, _ := json.Marshal(map[string]any{
		"type":    "server-request",
		"rpcId":   "rpc-sse",
		"method":  "session/event",
		"payload": map[string]any{"sessionId": "s-sse", "event": map[string]any{"type": evType, "seq": 1, "time": time.Now().UnixMilli(), "data": json.RawMessage(dataJSON)}},
	})
	return string(env)
}

func waitFor(t *testing.T, what string, want int, get func() int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for get() != want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := get(); got != want {
		t.Fatalf("%s: want %d, got %d", what, want, got)
	}
}

// TestWarmCapsConcurrentCreates verifies the background warm path caps the
// number of parallel session creations (bounded burst scaling, not one
// session per RPC round-trip) and that every spawned create lands in the pool.
func TestWarmCapsConcurrentCreates(t *testing.T) {
	var inflight, maxInflight atomic.Int32
	ts := fakeUpstream(t, 150*time.Millisecond, &inflight, &maxInflight)

	p := NewPool(PoolConfig{MinSize: 0, MaxSize: 8, UpstreamURL: ts.URL, AgentPreset: "minimal"})
	for i := 0; i < 10; i++ {
		p.warm() // only maxConcurrentWarm of these should actually start
	}
	time.Sleep(50 * time.Millisecond) // let spawned goroutines reach the handler

	p.warmingMu.Lock()
	n := p.warming
	p.warmingMu.Unlock()
	if n > p.maxConcurrentWarm {
		t.Fatalf("in-flight warms %d exceed cap %d", n, p.maxConcurrentWarm)
	}

	waitFor(t, "warm creates landing", p.maxConcurrentWarm, p.Count)
	if got := maxInflight.Load(); got != int32(p.maxConcurrentWarm) {
		t.Fatalf("expected %d parallel creates, observed max concurrency %d", p.maxConcurrentWarm, got)
	}
}

// TestAcquireScalesUnderBurst verifies the pool scales up under a concurrent
// burst: every waiter gets a distinct session and the pool grows to the burst
// size, with session creation running in parallel.
func TestAcquireScalesUnderBurst(t *testing.T) {
	var inflight, maxInflight atomic.Int32
	ts := fakeUpstream(t, 100*time.Millisecond, &inflight, &maxInflight)

	const burst = 6
	p := NewPool(PoolConfig{MinSize: 0, MaxSize: burst, UpstreamURL: ts.URL, AgentPreset: "minimal"})

	sessions := make([]*Session, burst)
	errs := make([]error, burst)
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sessions[i], errs[i] = p.Acquire(ctx)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("acquire %d failed: %v", i, err)
		}
	}
	if got := p.Count(); got != burst {
		t.Fatalf("pool did not scale to burst: want %d sessions, got %d", burst, got)
	}
	if max := maxInflight.Load(); max < 2 {
		t.Fatalf("expected parallel session creation under burst, observed max concurrency %d", max)
	}

	seen := make(map[string]bool, burst)
	for _, s := range sessions {
		if seen[s.SessionID] {
			t.Fatalf("duplicate session %s handed out", s.SessionID)
		}
		seen[s.SessionID] = true
		p.Release(s, true)
	}
	if got := len(p.free); got != burst {
		t.Fatalf("expected %d free sessions after release, got %d", burst, got)
	}
}
