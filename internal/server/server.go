package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mathrand "math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"edgeone2api/internal/auth"
	"edgeone2api/internal/config"
	"edgeone2api/internal/upstream"
)

// normalizeReasoningEffort maps client-supplied reasoning strength values to
// the upstream's accepted set ("off", "high", "max").  OpenAI-standard
// values (low/medium/high) and common aliases are normalized so downstream
// clients can send reasoning_effort without hitting an upstream rejection:
//
//	low / medium / high  -> high   (lowest upstream on-state)
//	max / extreme        -> max
//	off / none / false   -> off
//	anything else        -> ""    (unset: upstream default)
func normalizeReasoningEffort(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return ""
	case "off", "none", "false", "0":
		return "off"
	case "low", "medium", "high":
		return "high"
	case "max", "extreme", "high-max":
		return "max"
	default:
		return ""
	}
}

// Server is an OpenAI-compatible API server backed by DeepSeek Harness sessions
type Server struct {
	pool                   *auth.Pool
	apiKey                 string
	models                 []string
	timeout                time.Duration // non-streaming overall timeout
	streamIdle             time.Duration // streaming: no event for this long = dead (0 = disabled)
	defaultReasoningEffort string        // fallback when client & model_map don't specify
	modelMap               map[string]config.ModelMapping
	jitterMs               int // pseudo-concurrency: random pre-send delay [0, jitterMs)
	rng                    *mathrand.Rand
	rngMu                  sync.Mutex
	sem                    chan struct{} // in-flight upstream request cap (nil = unlimited)
}

// New creates a new server
func New(pool *auth.Pool, apiKey string, models []string, timeout, streamIdle time.Duration, modelMap map[string]config.ModelMapping, defaultReasoningEffort string, jitterMs, maxConcurrent int) *Server {
	if len(models) == 0 {
		models = []string{"@makers/deepseek-v4-flash", "@makers/deepseek-v4-pro"}
	}
	if modelMap == nil {
		modelMap = map[string]config.ModelMapping{}
	}
	s := &Server{
		pool:                   pool,
		apiKey:                 apiKey,
		models:                 models,
		timeout:                timeout,
		streamIdle:             streamIdle,
		defaultReasoningEffort: defaultReasoningEffort,
		modelMap:               modelMap,
		jitterMs:               jitterMs,
		rng:                    mathrand.New(mathrand.NewSource(time.Now().UnixNano())),
	}
	if maxConcurrent > 0 {
		s.sem = make(chan struct{}, maxConcurrent)
	}
	return s
}

// Handler returns the HTTP handler
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/pool", s.handlePool)
	return mux
}

func (s *Server) auth(r *http.Request) bool {
	if s.apiKey == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.TrimSpace(got) == s.apiKey
}

// --- Models ---

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.auth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "auth_error"}})
		return
	}
	data := make([]map[string]any, 0, len(s.models))
	for _, m := range s.models {
		data = append(data, map[string]any{"id": m, "object": "model", "created": time.Now().Unix(), "owned_by": "deepseek-harness"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// --- Request types ---

type openaiChatRequest struct {
	Model           string          `json:"model"`
	Messages        []openaiMessage `json:"messages"`
	Stream          bool            `json:"stream"`
	MaxTokens       int             `json:"max_tokens"`
	Temperature     float64         `json:"temperature"`
	ReasoningEffort string          `json:"reasoning_effort"`
	Tools           []openaiTool    `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
}

type openaiTool struct {
	Type     string         `json:"type"`
	Function openaiToolFunc `json:"function"`
}

type openaiToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// openaiToolCall mirrors the OpenAI tool_calls message field (parsed from
// client history and returned to the client for execution).
type openaiToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function openaiToolCallFn `json:"function"`
}

type openaiToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openaiMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// --- Chat Completions ---

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if !s.auth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "auth_error"}})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}

	var req openaiChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "messages is required"})
		return
	}

	model := req.Model
	if model == "" {
		model = s.models[0]
	}

	log.Printf("[CHAT] model=%s stream=%v msgs=%d", model, req.Stream, len(req.Messages))

	// Pseudo-concurrency: spread concurrent requests with a random delay so
	// they don't hit the upstream in the same instant (its rate limiter trips
	// on simultaneous session.prompt bursts and returns 502/timeouts).
	// Applied before session acquisition so the delay doesn't hold a session.
	if s.jitterMs > 0 {
		s.rngMu.Lock()
		d := time.Duration(s.rng.Intn(s.jitterMs)) * time.Millisecond
		s.rngMu.Unlock()
		if d > 0 {
			time.Sleep(d)
		}
	}

	// Concurrency gate: cap in-flight upstream requests so a burst of client
	// concurrency never overloads the upstream (it degrades to 502/timeouts
	// under sustained parallel load).  Excess requests queue here; from the
	// client's perspective the requests still complete, just slightly later.
	// Waits on the client connection context, not the 180s upstream timeout,
	// so queueing time does not eat into the per-request upstream budget.
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-r.Context().Done():
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "server busy, retry later", "type": "server_error"}})
			return
		}
	}

	// Session affinity: a client that sends X-Session-Key gets a bound,
	// stateful session for continuity.  Anonymous requests (no header) use a
	// stateless free-pool session and release it afterwards, so they can not
	// exhaust the pool.
	sessionKey := r.Header.Get("X-Session-Key")
	bound := sessionKey != ""

	// Timeout semantics differ by mode:
	//   - Non-streaming: overall budget (s.timeout) covers the whole round-trip.
	//   - Streaming: NO overall deadline — an active upstream that keeps
	//     emitting events must never be cut off mid-stream.  The client's own
	//     disconnect cancels r.Context() naturally; a dead upstream is caught
	//     by the per-event idle timeout inside StreamEvents.  The same ctx is
	//     handed to StartChat, whose internal SSE/RPC setup has its own guards
	//     (RPC client timeout + SSE response-header timeout).
	ctx := r.Context()
	if !req.Stream && s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), s.timeout)
		defer cancel()
	}

	var session *auth.Session
	// release is session-explicit on purpose: callers rotate sessions on
	// quota/not-found and would otherwise release a stale (already-recycled)
	// session while leaking the freshly acquired one forever.
	release := func(sess *auth.Session, success bool) {
		if bound {
			s.pool.ReleaseBind(sessionKey, sess, success)
			return
		}
		s.pool.Release(sess, success)
	}
	if bound {
		session, err = s.pool.Bind(ctx, sessionKey)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
			return
		}
	} else {
		session, err = s.pool.Acquire(ctx)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
			return
		}
	}

	// Resolve model mapping and apply selectModel if needed.  If the session
	// turns out to be gone (server-side idle reap), rotate to a fresh one and
	// retry once — otherwise the request proceeds against a dead session and
	// fails with a spurious 502.  Note: only a genuine quota error burns the
	// fingerprint for 24h; a "not found" is routine lifecycle (see MarkGone).
	mm, ok := s.modelMap[model]
	if !ok {
		// Fallback: treat the model string as-is with edgeone-makers provider
		mm = config.ModelMapping{Provider: "edgeone-makers", Model: model}
	}
	// Reasoning effort precedence: client request > model_map mapping >
	// global default_reasoning_effort (config) > unset (upstream default).
	re := normalizeReasoningEffort(req.ReasoningEffort)
	if re == "" {
		re = normalizeReasoningEffort(mm.ReasoningEffort)
	}
	if re == "" {
		re = normalizeReasoningEffort(s.defaultReasoningEffort)
	}
	selKey := mm.Provider + "/" + mm.Model + "/" + re
	if session.SelectedModel != selKey {
		if err := session.Client.SelectModel(ctx, session.SessionID, session.ConversationID, mm.Provider, mm.Model, re); err != nil {
			log.Printf("[SELECTMODEL] %s: %v", selKey, err)
			if upstream.IsSessionNotFound(err) {
				session.MarkGone()
				release(session, false)
				if bound {
					session, err = s.pool.Bind(ctx, sessionKey)
				} else {
					session, err = s.pool.Acquire(ctx)
				}
				if err != nil {
					writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
					return
				}
				if err := session.Client.SelectModel(ctx, session.SessionID, session.ConversationID, mm.Provider, mm.Model, re); err != nil {
					log.Printf("[SELECTMODEL] retry %s: %v", selKey, err)
				} else {
					session.SelectedModel = selKey
					log.Printf("[SELECTMODEL] session=%s model=%s (after rotation)", session.SessionID, selKey)
				}
			}
		} else {
			session.SelectedModel = selKey
			log.Printf("[SELECTMODEL] session=%s model=%s", session.SessionID, selKey)
		}
	}

	chatID := "chatcmpl-" + randHex(24)
	created := time.Now().Unix()

	// Tool calling is emulated in the prompt layer, kuku2api-style: the
	// upstream is always treated as a plain-text black box (it has no native
	// `tools` parameter, and its persona guards against being driven as a tool
	// executor).  When the client declares tools, the definitions are rendered
	// into a private-delimiter text protocol injected ahead of the conversation
	// and the reply is parsed back into standard OpenAI tool_calls for the
	// client to execute.  Tool results arrive as follow-up role=tool messages.
	// Native tool-call blocks are never surfaced: the upstream is always
	// handled as plain text, and
	// the SSE stream is cancelled as soon as the turn ends so the upstream
	// agent loop never runs.
	toolsJSON := ""
	if len(req.Tools) > 0 && !toolChoiceNone(req.ToolChoice) {
		if b, err := json.Marshal(req.Tools); err == nil {
			toolsJSON = string(b)
		}
	}

	if req.Stream {
		s.streamChat(w, ctx, session, chatID, model, created, req.Messages, sessionKey, release, func(ctx context.Context) (*auth.Session, error) {
			if bound {
				return s.pool.Bind(ctx, sessionKey)
			}
			return s.pool.Acquire(ctx)
		}, toolsJSON)
	} else {
		s.nonStreamChat(w, ctx, session, chatID, model, created, req.Messages, sessionKey, release, func(ctx context.Context) (*auth.Session, error) {
			if bound {
				return s.pool.Bind(ctx, sessionKey)
			}
			return s.pool.Acquire(ctx)
		}, toolsJSON)
	}
}

func (s *Server) streamChat(w http.ResponseWriter, ctx context.Context, session *auth.Session, chatID, model string, created int64, msgs []openaiMessage, sessionKey string, release func(*auth.Session, bool), reacquire func(context.Context) (*auth.Session, error), toolsJSON string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		release(session, false)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}

	userText := lastUserMessage(msgs)

	// Try once with the current session; if quota error, retry with a fresh one.
	for attempt := 0; attempt < 2; attempt++ {
		items := s.buildItems(session, sessionKey, msgs, toolsJSON)
		cs, err := session.Client.StartChat(ctx, session.SessionID, session.ConversationID, items)
		if err != nil {
			quotaOrGone := upstream.IsQuotaError(err) || upstream.IsSessionNotFound(err)
			if quotaOrGone && attempt == 0 {
				// Quota / session-destroyed: rope the session and retry once with a
				// fresh one (transparent rotation instead of failed response).
				// Only a genuine quota error burns the fingerprint for 24h; a
				// server-side "not found" (idle reap) must NOT — see MarkGone.
				if upstream.IsQuotaError(err) {
					session.MarkQuotaExceeded()
				} else {
					session.MarkGone()
				}
				release(session, false)
				session, err = reacquire(ctx)
				if err != nil {
					writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
					return
				}
				continue // retry
			}
			if upstream.IsTransientError(err) && attempt == 0 {
				// Upstream harness stall (e.g. "timeout awaiting response headers"):
				// keep the session and retry once after a short backoff rather than
				// surfacing a spurious 502.
				log.Printf("[CHAT] transient upstream error, retrying: %v", err)
				select {
				case <-ctx.Done():
					release(session, false)
					writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"}})
					return
				case <-time.After(500 * time.Millisecond):
				}
				continue // retry, same session
			}
			release(session, false)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"}})
			return
		}

		// Prompt accepted — now flush the 200 and start streaming
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("X-Session-Key", sessionKey)
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Role chunk
		emitSSE(w, chatID, model, created, map[string]any{"role": "assistant"})
		flusher.Flush()

		success := false

		defer func() {
			release(session, success)
		}()

		var sb strings.Builder
		var streamErr error

		// We cannot tell a plain-text tool-call block from prose until its
		// opening delimiter arrives, so hold back a small tail window: text
		// older than the window streams immediately (no added latency for
		// ordinary answers), while a possible delimiter prefix stays buffered.
		// Once the delimiter appears in the raw stream, everything from it on
		// (the whole tool-call block) is withheld from the client and only the
		// parsed tool_calls are emitted at the end.  Mirrors kuku2api's
		// "buffer, then parse" streaming behaviour without penalising plain text.
		var raw strings.Builder
		var sent int // bytes of raw already emitted to the client as content
		var gate int // bytes of raw confirmed free of the opening delimiter
		const tail = 64

		// Cancel the SSE stream as soon as this turn ends.  The upstream agent
		// loop (platform sandbox tool execution) only starts after turn/end; a
		// cancelled stream cuts it off — the sandbox never runs.  This is the
		// enforcement half of the "tools run on the client, not in the sandbox"
		// design; the directive (BuildDirective) is the prevention half.
		defer cs.Cancel()

		_, streamErr = session.Client.StreamEvents(ctx, cs, func(chunk upstream.AssistantChunk) {
			if chunk.IsDone {
				return // finish chunk is emitted after StreamEvents returns
			}

			delta := map[string]any{}
			if chunk.Text != "" {
				sb.WriteString(chunk.Text)
				raw.WriteString(chunk.Text)
				r := raw.String()
				// Never gate past the first opening delimiter: once it appears,
				// the tool-call block starts there and must not stream out.
				if idx := strings.Index(r, upstream.TCStart); idx >= 0 {
					gate = idx
				} else {
					limit := len(r) - tail
					if limit < 0 {
						limit = 0
					}
					// Back off so a partial delimiter (which always begins with the
					// same leading codepoint) also stays buffered.
					if part := partialDelimSuffix(r, upstream.TCStart); part > 0 {
						limit -= part
						if limit < 0 {
							limit = 0
						}
					}
					if limit > gate {
						gate = limit
					}
				}
				if gate > sent {
					// Never split a multi-byte UTF-8 rune: slicing mid-rune would
					// corrupt the text (json.Marshal replaces the broken bytes with
					// U+FFFD).  Align the cut back to a rune boundary.
					for gate > sent && !utf8.RuneStart(r[gate]) {
						gate--
					}
				}
				if gate > sent {
					out := r[sent:gate]
					sent = gate
					if out != "" {
						delta["content"] = out
					}
				}
			}
			if chunk.Reasoning != "" {
				delta["reasoning_content"] = chunk.Reasoning
			}
			if len(delta) > 0 {
				emitSSE(w, chatID, model, created, delta)
				flusher.Flush()
			}
		}, s.streamIdle)
		finishReason := "stop"
		// Parse the buffered reply back into standard tool_calls.  The tool-call
		// block (delimiters included) never reached the client, so only the
		// visible head text was streamed; a detected call yields finish=tool_calls.
		content, calls := parseToolCalls(sb.String())
		// Flush the held-back tail: for a direct answer it is the rest of the
		// visible text; when a call was found it is the head text that preceded
		// the (withheld) tool-call block.
		if len(calls) == 0 {
			if r := raw.String(); sent < len(r) {
				emitSSE(w, chatID, model, created, map[string]any{"content": r[sent:]})
				flusher.Flush()
			}
		} else if sent < len(content) {
			emitSSE(w, chatID, model, created, map[string]any{"content": content[sent:]})
			flusher.Flush()
		}
		if len(calls) > 0 {
			emitToolCallsSSE(w, chatID, model, created, calls)
			flusher.Flush()
			finishReason = "tool_calls"
		}
		if streamErr != nil {
			log.Printf("[STREAM] error: %v", streamErr)
			success = false
		} else {
			success = true
			if sessionKey != "" {
				s.pool.AppendHistory(sessionKey, session.SessionID, userText, content)
			}
		}

		emitFinish(w, chatID, model, created, finishReason)
		flusher.Flush()
		return
	}
	// Both attempts failed
	release(session, false)
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream quota exceeded, retry later"})
}

func (s *Server) nonStreamChat(w http.ResponseWriter, ctx context.Context, session *auth.Session, chatID, model string, created int64, msgs []openaiMessage, sessionKey string, release func(*auth.Session, bool), reacquire func(context.Context) (*auth.Session, error), toolsJSON string) {
	var result upstream.ChatResult
	var err error

	userText := lastUserMessage(msgs)

	for attempt := 0; attempt < 2; attempt++ {
		var cs *upstream.ChatStream
		items := s.buildItems(session, sessionKey, msgs, toolsJSON)
		cs, err = session.Client.StartChat(ctx, session.SessionID, session.ConversationID, items)
		if err != nil {
			if attempt == 0 && (upstream.IsQuotaError(err) || upstream.IsSessionNotFound(err)) {
				// Only a genuine quota error burns the fingerprint; a server-side
				// "not found" is routine idle-reap, not a limit hit (see MarkGone).
				if upstream.IsQuotaError(err) {
					session.MarkQuotaExceeded()
				} else {
					session.MarkGone()
				}
				release(session, false)
				session, err = reacquire(ctx)
				if err != nil {
					writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
					return
				}
				continue
			}
			if attempt == 0 && upstream.IsTransientError(err) {
				// Upstream harness stall: retry once on the same session instead of
				// surfacing a spurious 502 (see IsTransientError).
				log.Printf("[CHAT] transient upstream error, retrying: %v", err)
				select {
				case <-ctx.Done():
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			release(session, false)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"}})
			return
		}
		// Cancel the SSE stream as soon as the turn ends.  The upstream agent
		// loop (sandbox tool execution) only starts after turn/end; a cancelled
		// stream cuts it off — the sandbox never runs.  Tools execute on the
		// client, never in the sandbox.
		cancelStream := cs.Cancel
		result, err = session.Client.StreamEvents(ctx, cs, nil, s.streamIdle)
		cancelStream()
		if err != nil {
			log.Printf("[CHAT] stream error: %v", err)
			if attempt == 0 && upstream.IsTransientError(err) {
				continue // retry once; the failed stream was just cancelled
			}
		}
		break
	}
	if err != nil {
		release(session, false)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"}})
		return
	}

	success := true
	if sessionKey != "" {
		s.pool.AppendHistory(sessionKey, session.SessionID, userText, result.Text)
	}

	msg := map[string]any{"role": "assistant", "content": result.Text}
	if result.Reasoning != "" {
		msg["reasoning_content"] = result.Reasoning
	}
	// kuku2api-style prompt-layer tool parsing: the model declares tool calls
	// as a private-delimiter text block at the end of its reply.  Parse it
	// back into standard OpenAI tool_calls for the client to execute.  The
	// delimiters are stripped from the returned content; no native tool-call
	// block is ever surfaced (native tool-call blocks are always folded away).
	finish := "stop"
	if content, calls := parseToolCalls(result.Text); len(calls) > 0 {
		msg["content"] = nil
		msg["tool_calls"] = calls
		finish = "tool_calls"
		if sessionKey != "" {
			s.pool.AppendHistory(sessionKey, session.SessionID, userText, content)
		}
	}

	resp := map[string]any{
		"id":      chatID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}

	release(session, success)
	w.Header().Set("X-Session-Key", sessionKey)
	writeJSON(w, http.StatusOK, resp)
}

// --- Message conversion ---

// buildItems assembles the prompt for a session: the system directive
// (plain-text posture, or the kuku2api private-delimiter tool protocol when the
// client declared tools), an optional cache-hit replay of the previous dialog
// history, then the request messages.  Replay is recomputed per session so a
// fresh session created by a quota retry inherits the cached context
// automatically.
func (s *Server) buildItems(session *auth.Session, sessionKey string, msgs []openaiMessage, toolsJSON string) []upstream.ContentItem {
	items := []upstream.ContentItem{{Type: "text", Text: upstream.BuildDirective(toolsJSON)}}
	if sessionKey != "" {
		if replay := s.pool.ReplayHistory(sessionKey, session.SessionID); len(replay) > 0 {
			items = append(items, upstream.ContentItem{Type: "text", Text: formatReplay(replay)})
		}
	}
	return append(items, convertMessages(msgs)...)
}

func formatReplay(turns []auth.DialogTurn) string {
	var sb strings.Builder
	sb.WriteString("[Cache Hit - Previous Conversation Context (already happened, for reference only)]\n")
	for _, t := range turns {
		if t.Role == "assistant" {
			sb.WriteString("assistant: " + t.Text + "\n")
		} else {
			sb.WriteString("user: " + t.Text + "\n")
		}
	}
	sb.WriteString("[End of Previous Context - answer the latest user message only]\n")
	return sb.String()
}

func convertMessages(msgs []openaiMessage) []upstream.ContentItem {
	var items []upstream.ContentItem

	for _, msg := range msgs {
		switch msg.Role {
		case "system":
			text := extractContent(msg.Content)
			if text != "" {
				items = append(items, upstream.ContentItem{Type: "text", Text: "[System] " + text})
			}
		case "user":
			text := extractContent(msg.Content)
			if text != "" {
				items = append(items, upstream.ContentItem{Type: "text", Text: text})
			}
		case "assistant":
			text := extractContent(msg.Content)
			if text != "" {
				items = append(items, upstream.ContentItem{Type: "text", Text: text})
			}
			// Preserve a previous tool_calls turn as fenced text so the
			// upstream model sees which tool-call was issued (with its id),
			// enabling it to match a later tool result and continue.
			if len(msg.ToolCalls) > 0 {
				items = append(items, upstream.ContentItem{Type: "text", Text: formatAssistantToolCalls(msg.ToolCalls)})
			}
		case "tool":
			text := extractContent(msg.Content)
			if text == "" {
				continue
			}
			// Tool result echoed by the client.  The upstream session.prompt
			// only accepts plain {type,text} content items — it rejects
			// structured "tool" items — so we fold the result into a labeled
			// text turn, keeping tool_call_id so the upstream agent can
			// correlate it with the assistant's earlier tool call and continue.
			label := "[Tool Result]"
			if msg.ToolCallID != "" {
				label += " (tool_call_id=" + msg.ToolCallID + ")"
			}
			items = append(items, upstream.ContentItem{Type: "text", Text: label + " " + text})
		}
	}
	return items
}

// toolChoiceNone reports whether the client sent tool_choice:"none", in which
// case the tool protocol must not be injected (mirrors kuku2api).
func toolChoiceNone(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == `"none"`
}

// parseToolCalls parses the model's text output for the kuku2api-style
// private-delimiter tool-call protocol.
//
// It returns the remaining visible text (with the tool-call block stripped)
// and the parsed calls.  When no block is present the text is returned
// unchanged and calls is empty — meaning the model answered directly.  A
// block only counts if its arguments decode to a JSON object, so ordinary
// prose can never be mistaken for a call.
func parseToolCalls(text string) (string, []openaiToolCall) {
	if !strings.Contains(text, upstream.TCStart) {
		return text, nil
	}
	head := strings.SplitN(text, upstream.TCStart, 2)[0]

	var calls []openaiToolCall
	for i, blk := range strings.Split(text, upstream.TCStart)[1:] {
		blk = strings.SplitN(blk, upstream.TCEnd, 2)[0]
		if !strings.Contains(blk, upstream.TCNameS) || !strings.Contains(blk, upstream.TCArgsS) {
			continue
		}
		nameParts := strings.SplitN(blk, upstream.TCNameS, 2)
		if len(nameParts) < 2 {
			continue
		}
		name := strings.TrimSpace(strings.SplitN(nameParts[1], upstream.TCNameE, 2)[0])
		argsParts := strings.SplitN(blk, upstream.TCArgsS, 2)
		if len(argsParts) < 2 {
			continue
		}
		args := strings.TrimSpace(strings.SplitN(argsParts[1], upstream.TCArgsE, 2)[0])
		if name == "" {
			continue
		}
		// Arguments must be a JSON object; otherwise drop the block (avoids
		// mistaking body text for a call).
		var obj map[string]any
		if err := json.Unmarshal([]byte(args), &obj); err != nil {
			continue
		}
		calls = append(calls, openaiToolCall{
			ID:   fmt.Sprintf("call_%s_%d", randHex(16), i),
			Type: "function",
			Function: openaiToolCallFn{
				Name:      name,
				Arguments: args,
			},
		})
	}
	if len(calls) == 0 {
		return text, nil
	}
	return strings.TrimSpace(head), calls
}

// partialDelimSuffix returns the length of the longest proper prefix of delim
// that is a suffix of s.  Used to hold back a reply tail that might be the
// start of a tool-call delimiter split across stream chunks.
func partialDelimSuffix(s, delim string) int {
	max := len(delim) - 1
	if max > len(s) {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, delim[:n]) {
			return n
		}
	}
	return 0
}

func extractContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return string(bytes.Trim(raw, "\""))
}

func lastUserMessage(msgs []openaiMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			if t := extractContent(msgs[i].Content); t != "" {
				return t
			}
		}
	}
	return ""
}

// formatAssistantToolCalls renders a previous assistant tool_calls turn as the
// same private-delimiter protocol the model is asked to emit, so the upstream
// sees its own prior call and can correlate the subsequent tool result.
func formatAssistantToolCalls(calls []openaiToolCall) string {
	var sb strings.Builder
	sb.WriteString("[Assistant tool call issued earlier]\n")
	for _, c := range calls {
		sb.WriteString(upstream.TCStart + "\n")
		sb.WriteString(upstream.TCNameS + c.Function.Name + upstream.TCNameE + "\n")
		sb.WriteString(upstream.TCArgsS + c.Function.Arguments + upstream.TCArgsE + "\n")
		sb.WriteString(upstream.TCEnd + "\n")
	}
	return sb.String()
}

// --- SSE helpers ---

func emitSSE(w http.ResponseWriter, chatID, model string, created int64, delta map[string]any) {
	c := map[string]any{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": nil,
	}
	if delta != nil {
		c["delta"] = delta
	}

	out := map[string]any{
		"id":      chatID,
		"model":   model,
		"created": created,
		"object":  "chat.completion.chunk",
		"choices": []any{c},
	}
	d, _ := json.Marshal(out)
	w.Write([]byte("data: " + string(d) + "\n\n"))
}

// emitToolCallsSSE sends the complete tool_calls payload as a streaming chunk.
// Used when the model declared tool_calls via the private-delimiter text
// protocol instead of native tool-call blocks.
func emitToolCallsSSE(w http.ResponseWriter, chatID, model string, created int64, calls []openaiToolCall) {
	delta := map[string]any{
		"role":       "assistant",
		"content":    nil,
		"tool_calls": calls,
	}
	out := map[string]any{
		"id":      chatID,
		"model":   model,
		"created": created,
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": "tool_calls"}},
	}
	d, _ := json.Marshal(out)
	w.Write([]byte("data: " + string(d) + "\n\n"))
}

func emitFinish(w http.ResponseWriter, chatID, model string, created int64, finish string) {
	out := map[string]any{
		"id":      chatID,
		"model":   model,
		"created": created,
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	}
	d, _ := json.Marshal(out)
	w.Write([]byte("data: " + string(d) + "\n\n"))
	w.Write([]byte("data: [DONE]\n\n"))
}

// --- Health / Pool ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "poolSize": s.pool.Count()})
}

func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"pool": s.pool.Stats()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		b = []byte(time.Now().String())
	}
	return hex.EncodeToString(b)[:n]
}
