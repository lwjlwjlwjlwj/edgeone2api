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
	"net/http"
	"regexp"
	"strings"
	"time"

	"edgeone2api/internal/auth"
	"edgeone2api/internal/config"
	"edgeone2api/internal/toolcall"
	"edgeone2api/internal/upstream"
)

// Server is an OpenAI-compatible API server backed by DeepSeek Harness sessions
type Server struct {
	pool     *auth.Pool
	apiKey   string
	models   []string
	timeout  time.Duration
	modelMap map[string]config.ModelMapping
	tc       *toolcall.Client
}

// New creates a new server.  tc is the tool-call sidecar client (may be nil
// for a gateway that never declares tools).
func New(pool *auth.Pool, apiKey string, models []string, timeout time.Duration, modelMap map[string]config.ModelMapping, tc *toolcall.Client) *Server {
	if len(models) == 0 {
		models = []string{"@makers/deepseek-v4-flash", "@makers/deepseek-v4-pro"}
	}
	if modelMap == nil {
		modelMap = map[string]config.ModelMapping{}
	}
	return &Server{
		pool:     pool,
		apiKey:   apiKey,
		models:   models,
		timeout:  timeout,
		modelMap: modelMap,
		tc:       tc,
	}
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

// openaiToolCall mirrors the OpenAI tool_calls message field returned to the client
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

	// Session affinity: a client that sends X-Session-Key gets a bound,
	// stateful session for continuity.  Anonymous requests (no header) use a
	// stateless free-pool session and release it afterwards, so they can not
	// exhaust the pool.
	sessionKey := r.Header.Get("X-Session-Key")
	bound := sessionKey != ""

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	var session *auth.Session
	var release func(bool)
	if bound {
		session, err = s.pool.Bind(ctx, sessionKey)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
			return
		}
		release = func(success bool) { s.pool.ReleaseBind(sessionKey, session, success) }
	} else {
		session, err = s.pool.Acquire(ctx)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
			return
		}
		release = func(success bool) { s.pool.Release(session, success) }
	}

	// Resolve model mapping and apply selectModel if needed.
	mm, ok := s.modelMap[model]
	if !ok {
		// Fallback: treat the model string as-is with edgeone-makers provider
		mm = config.ModelMapping{Provider: "edgeone-makers", Model: model}
	}
	selKey := mm.Provider + "/" + mm.Model + "/" + mm.ReasoningEffort
	if req.ReasoningEffort != "" {
		selKey = mm.Provider + "/" + mm.Model + "/" + req.ReasoningEffort
	}
	if session.SelectedModel != selKey {
		re := req.ReasoningEffort
		if re == "" {
			re = mm.ReasoningEffort
		}
		if err := session.Client.SelectModel(ctx, session.SessionID, session.ConversationID, mm.Provider, mm.Model, re); err != nil {
			log.Printf("[SELECTMODEL] %s: %v", selKey, err)
		} else {
			session.SelectedModel = selKey
			log.Printf("[SELECTMODEL] session=%s model=%s", session.SessionID, selKey)
		}
	}

	chatID := "chatcmpl-" + randHex(24)
	created := time.Now().Unix()

	toolsJSON := ""
	declared := declaredToolSet{}
	if len(req.Tools) > 0 {
		if b, err := json.Marshal(req.Tools); err == nil {
			toolsJSON = string(b)
		}
		for _, t := range req.Tools {
			declared[t.Function.Name] = t
		}
	}

	if req.Stream {
		s.streamChat(w, ctx, session, chatID, model, created, req.Messages, sessionKey, release, func(ctx context.Context) (*auth.Session, error) {
			if bound {
				return s.pool.Bind(ctx, sessionKey)
			}
			return s.pool.Acquire(ctx)
		}, toolsJSON, declared)
	} else {
		s.nonStreamChat(w, ctx, session, chatID, model, created, req.Messages, sessionKey, release, func(ctx context.Context) (*auth.Session, error) {
			if bound {
				return s.pool.Bind(ctx, sessionKey)
			}
			return s.pool.Acquire(ctx)
		}, toolsJSON, declared)
	}
}

// toOpenAIToolCalls converts upstream-native assistant tool calls into the
// OpenAI tool_calls message shape returned to the client.
func toOpenAIToolCalls(tcs []upstream.AssistantToolCall) []openaiToolCall {
	if len(tcs) == 0 {
		return nil
	}
	calls := make([]openaiToolCall, 0, len(tcs))
	for i, tc := range tcs {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", randHex(6), i)
		}
		calls = append(calls, openaiToolCall{
			ID:   id,
			Type: "function",
			Function: openaiToolCallFn{
				Name:      tc.Name,
				Arguments: tc.Arguments,
			},
		})
	}
	return calls
}

func (s *Server) streamChat(w http.ResponseWriter, ctx context.Context, session *auth.Session, chatID, model string, created int64, msgs []openaiMessage, sessionKey string, release func(bool), reacquire func(context.Context) (*auth.Session, error), toolsJSON string, declared declaredToolSet) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		release(false)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}

	userText := lastUserMessage(msgs)

	// Try once with the current session; if quota error, retry with a fresh one.
	for attempt := 0; attempt < 2; attempt++ {
		items, err := s.buildItems(ctx, session, sessionKey, msgs, toolsJSON)
		if err != nil {
			release(false)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "tool pipeline error: " + err.Error(), "type": "upstream_error"}})
			return
		}
		cs, err := session.Client.StartChat(ctx, session.SessionID, session.ConversationID, items)
		if err != nil {
			if upstream.IsQuotaError(err) || upstream.IsSessionNotFound(err) {
				// Quota / session-destroyed: rope the session and retry once with a
				// fresh one (transparent rotation instead of failed response).
				session.MarkQuotaExceeded()
				release(false)
				session, err = reacquire(ctx)
				if err != nil {
					writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
					return
				}
				continue // retry
			}
			release(false)
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
			release(success)
		}()

		var sb strings.Builder
		var result upstream.ChatResult
		var streamErr error
		// nativeToolCall tracks whether upstream emitted real tool-call events
		// mid-stream (vs. the text-protocol tool_calls parsed after the turn).
		nativeToolCall := false
		toolSeen := make(map[int]bool) // index -> name delta sent
		// With tools declared, protocol envelopes (XYML/QNML markup, legacy
		// tool_calls JSON) are siphoned off the live stream and held until the
		// turn ends, where the sidecar parses them into tool_calls (with
		// truncation / parse-error recovery).  Plain prose streams live, so a
		// normal chat answer is never delayed.
		holdContent := toolsJSON != ""
		sieve := newStreamSieve()

		emitText := func(text string) {
			if text == "" {
				return
			}
			emitSSE(w, chatID, model, created, map[string]any{"content": text})
			flusher.Flush()
		}

		result, streamErr = session.Client.StreamEvents(ctx, cs, func(chunk upstream.AssistantChunk) {
			if chunk.IsDone {
				return // finish chunk is emitted after StreamEvents returns
			}

			delta := map[string]any{}
			if chunk.Text != "" {
				sb.WriteString(chunk.Text)
				if holdContent {
					if emitLive := sieve.feed(chunk.Text); emitLive != "" {
						emitText(emitLive)
					}
				} else {
					emitText(chunk.Text)
				}
			}
			if chunk.Reasoning != "" {
				delta["reasoning_content"] = chunk.Reasoning
			}
			if chunk.ToolCall != nil {
				tc := chunk.ToolCall
				nativeToolCall = true
				if holdContent {
					// Real tool-call events take over the turn: any text the
					// sieve still holds (prose tail or an unparsed envelope)
					// is drained to the client as content.
					if text := sieve.drainForNativeTool(); text != "" {
						emitText(text)
					}
				}
				if tc.Name != "" && !toolSeen[tc.Index] {
					toolSeen[tc.Index] = true
					delta["role"] = "assistant"
					delta["content"] = nil
					delta["tool_calls"] = []any{map[string]any{
						"index":    tc.Index,
						"id":       tc.ID,
						"type":     "function",
						"function": map[string]any{"name": translateToolCallName(tc.Name, declared), "arguments": tc.ArgumentsDelta},
					}}
				} else if tc.ArgumentsDelta != "" {
					delta["tool_calls"] = []any{map[string]any{
						"index":    tc.Index,
						"function": map[string]any{"arguments": tc.ArgumentsDelta},
					}}
				} else if tc.IsComplete && tc.Arguments != "" {
					delta["tool_calls"] = []any{map[string]any{
						"index":    tc.Index,
						"function": map[string]any{"arguments": tc.Arguments},
					}}
				}
			}
			if len(delta) > 0 {
				emitSSE(w, chatID, model, created, delta)
				flusher.Flush()
			}
		})
		logTools(sessionKey, result.ToolCalls)
		finishReason := "stop"
		if streamErr != nil {
			log.Printf("[STREAM] error: %v", streamErr)
			success = false
		} else {
			success = true
			if nativeToolCall || len(result.ToolCalls) > 0 {
				finishReason = "tool_calls"
			} else {
				heldText, rest := sieve.flush()
				if rest != "" {
					emitText(rest)
				}
				if calls, ok := s.resolveToolCalls(ctx, session, toolsJSON, declared, heldText); ok {
					emitToolCallsSSE(w, chatID, model, created, calls)
					flusher.Flush()
					finishReason = "tool_calls"
				} else if heldText != "" {
					// held content was not a tool declaration after all — stream it now
					emitText(heldText)
				}
			}
			if sessionKey != "" {
				s.pool.AppendHistory(sessionKey, session.SessionID, userText, sb.String())
			}
		}

		emitFinish(w, chatID, model, created, finishReason)
		flusher.Flush()
		return
	}
	// Both attempts failed
	release(false)
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream quota exceeded, retry later"})
}

func (s *Server) nonStreamChat(w http.ResponseWriter, ctx context.Context, session *auth.Session, chatID, model string, created int64, msgs []openaiMessage, sessionKey string, release func(bool), reacquire func(context.Context) (*auth.Session, error), toolsJSON string, declared declaredToolSet) {
	var result upstream.ChatResult
	var err error

	userText := lastUserMessage(msgs)

	for attempt := 0; attempt < 2; attempt++ {
		var cs *upstream.ChatStream
		items, buildErr := s.buildItems(ctx, session, sessionKey, msgs, toolsJSON)
		if buildErr != nil {
			release(false)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "tool pipeline error: " + buildErr.Error(), "type": "upstream_error"}})
			return
		}
		cs, err = session.Client.StartChat(ctx, session.SessionID, session.ConversationID, items)
		if err != nil {
			if upstream.IsQuotaError(err) || upstream.IsSessionNotFound(err) {
				session.MarkQuotaExceeded()
				release(false)
				session, err = reacquire(ctx)
				if err != nil {
					writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
					return
				}
				continue
			}
			release(false)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"}})
			return
		}
		result, err = session.Client.StreamEvents(ctx, cs, nil)
		logTools(sessionKey, result.ToolCalls)
		if err != nil {
			log.Printf("[CHAT] stream error: %v", err)
		}
		break
	}
	if err != nil {
		release(false)
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
	finish := "stop"
	// Prefer the upstream-native tool calls (real tool-call events), falling
	// back to the text-protocol tool calls the model emitted in its answer.
	// Both paths go through the translation layer so tool names match the
	// client's declared set.  When the text protocol fails to parse, the
	// sidecar recovery path issues one corrective turn before giving up.
	if calls := translateToolCalls(toOpenAIToolCalls(result.ToolCalls), declared); len(calls) > 0 {
		msg["content"] = nil
		msg["tool_calls"] = calls
		finish = "tool_calls"
	} else if calls, ok := s.resolveToolCalls(ctx, session, toolsJSON, declared, result.Text); ok {
		msg["content"] = nil
		msg["tool_calls"] = calls
		finish = "tool_calls"
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

	release(success)
	w.Header().Set("X-Session-Key", sessionKey)
	writeJSON(w, http.StatusOK, resp)
}

// --- Message conversion ---

// buildItems assembles the prompt for a session.  With declared tools the
// XYML instruction block (plus the detected CLI tool-profile block) becomes
// the system directive and the full history — including previous assistant
// tool_calls and tool results — is flattened to plain chat text by the
// sidecar, so the plain-LLM upstream never sees OpenAI tool-call structures.
// Without tools the request is a straightforward chat (native directive +
// converted messages).  A cache-hit replay of the previous dialog is
// recomputed per session so a fresh session created by a quota retry inherits
// the cached context automatically.
func (s *Server) buildItems(ctx context.Context, session *auth.Session, sessionKey string, msgs []openaiMessage, toolsJSON string) ([]upstream.ContentItem, error) {
	if toolsJSON == "" {
		items := []upstream.ContentItem{{Type: "text", Text: upstream.BuildDirective("")}}
		if sessionKey != "" {
			if replay := s.pool.ReplayHistory(sessionKey, session.SessionID); len(replay) > 0 {
				items = append(items, upstream.ContentItem{Type: "text", Text: formatReplay(replay)})
			}
		}
		return append(items, convertMessages(msgs)...), nil
	}

	if s.tc == nil {
		return nil, fmt.Errorf("tool sidecar unavailable")
	}
	ins, err := s.tc.BuildInstructions(ctx, []byte(toolsJSON))
	if err != nil {
		return nil, err
	}
	directive := ins.Instructions
	if ins.Profile.Block != "" {
		directive = directive + "\n\n" + ins.Profile.Block
	}
	items := []upstream.ContentItem{{Type: "text", Text: directive}}
	if sessionKey != "" {
		if replay := s.pool.ReplayHistory(sessionKey, session.SessionID); len(replay) > 0 {
			items = append(items, upstream.ContentItem{Type: "text", Text: formatReplay(replay)})
		}
	}
	msgsJSON, err := json.Marshal(msgs)
	if err != nil {
		return nil, fmt.Errorf("marshal messages: %w", err)
	}
	rendered, err := s.tc.RenderHistory(ctx, msgsJSON)
	if err != nil {
		return nil, err
	}
	for _, item := range rendered {
		if strings.TrimSpace(item.Content) == "" {
			continue
		}
		items = append(items, upstream.ContentItem{Type: "text", Text: item.Content})
	}
	return items, nil
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
			// upstream agent sees which tool-call was issued (with its id),
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

// formatAssistantToolCalls renders a previous assistant tool_calls turn as a
// stable text fragment (id + name + arguments) that the upstream agent can
// parse and correlate with the subsequent tool result.
func formatAssistantToolCalls(calls []openaiToolCall) string {
	var sb strings.Builder
	sb.WriteString("[Assistant Tool Calls - issued earlier]\n")
	for _, c := range calls {
		sb.WriteString("  id=" + c.ID + " name=" + c.Function.Name)
		if c.Function.Arguments != "" {
			sb.WriteString(" arguments=" + c.Function.Arguments)
		}
		sb.WriteString("\n")
	}
	return sb.String()
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

// logTools records any tool calls the upstream model actually emitted for a
// request.  The expected posture is zero native tool-call events: the
// directive makes the model declare tool calls as JSON text in the first
// turn instead of entering the platform agent loop.
func logTools(sessionKey string, calls []upstream.AssistantToolCall) {
	if len(calls) == 0 {
		log.Printf("[TOOLS] key=%s tool_calls=none", sessionKey)
		return
	}
	for _, tc := range calls {
		args := tc.Arguments
		if len(args) > 160 {
			args = args[:160] + "..."
		}
		log.Printf("[TOOLS] key=%s tool_call name=%s args=%s", sessionKey, tc.Name, args)
	}
}

// resolveToolCalls turns the model's text answer into client-facing tool
// calls, delegating to the tool sidecar (the ported toolforge engine):
//
//  1. parse the output with the full xyml engine (markup/XML/JSON/text-KV,
//     CDATA-aware) and translate native names to the client's declared set;
//  2. when nothing parses, ask the sidecar whether the output was truncated
//     or merely unparseable; if so, run ONE corrective turn (toolforge
//     recovery) and re-parse.
//
// ok is false when the text carries no tool-call attempt at all, meaning the
// caller should serve it as plain content.
func (s *Server) resolveToolCalls(ctx context.Context, session *auth.Session, toolsJSON string, declared declaredToolSet, text string) ([]openaiToolCall, bool) {
	if toolsJSON == "" || s.tc == nil || session == nil {
		return nil, false
	}
	if strings.TrimSpace(text) == "" {
		return nil, false
	}
	calls, reason, err := s.tc.RecoverToolCalls(ctx, text, []byte(toolsJSON))
	if err != nil {
		log.Printf("[TOOLCALL] parse failed: %v", err)
		return nil, false
	}
	if len(calls) > 0 {
		return translateToolCalls(sidecarCallsToOpenAI(calls), declared), true
	}
	if reason == "" {
		// output carries no tool-call attempt at all
		return nil, false
	}
	retry, err := s.tc.RetryMessage(ctx, text, reason)
	if err != nil || retry == "" {
		log.Printf("[TOOLCALL] retry message failed: %v", err)
		return nil, false
	}
	log.Printf("[TOOLCALL] recovery reason=%s — issuing corrective turn", reason)
	items := []upstream.ContentItem{{Type: "text", Text: retry}}
	cs, err := session.Client.StartChat(ctx, session.SessionID, session.ConversationID, items)
	if err != nil {
		log.Printf("[TOOLCALL] corrective turn start failed: %v", err)
		return nil, false
	}
	res, err := session.Client.StreamEvents(ctx, cs, nil)
	if err != nil {
		log.Printf("[TOOLCALL] corrective turn failed: %v", err)
		return nil, false
	}
	fixed, err := s.tc.ParseToolCalls(ctx, res.Text, []byte(toolsJSON))
	if err != nil || len(fixed) == 0 {
		log.Printf("[TOOLCALL] corrective turn still unparseable (%v, %d calls)", err, len(fixed))
		return nil, false
	}
	return translateToolCalls(sidecarCallsToOpenAI(fixed), declared), true
}

// sidecarCallsToOpenAI adapts sidecar tool calls (OpenAI JSON shape) into the
// server's native openaiToolCall value type.
func sidecarCallsToOpenAI(calls []toolcall.ToolCall) []openaiToolCall {
	out := make([]openaiToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, openaiToolCall{
			ID:   c.ID,
			Type: c.Type,
			Function: openaiToolCallFn{
				Name:      c.Function.Name,
				Arguments: c.Function.Arguments,
			},
		})
	}
	return out
}

// streamSieve mirrors toolforge's ToolSieve: it separates plain prose
// (streamed live) from tool-call envelopes (held until the turn ends, where
// the sidecar parses them).  A small look-back tail keeps a protocol marker
// split across chunk boundaries from leaking into the client stream.
type streamSieve struct {
	pending   strings.Builder
	held      strings.Builder
	capturing bool
}

// sieveTail is the look-back window for detecting a late protocol marker.
const sieveTail = 256

func newStreamSieve() *streamSieve {
	return &streamSieve{}
}

// feed consumes one text chunk and returns the portion that should be
// streamed to the client right now ("" while the siever is holding an
// envelope).  Prose always streams; when a tool marker appears the siever
// switches to capture mode and holds everything from the marker (or its
// leading code fence) onward.
func (s *streamSieve) feed(chunk string) string {
	if s.capturing {
		s.held.WriteString(chunk)
		return ""
	}
	s.pending.WriteString(chunk)
	candidate := s.pending.String()
	if m := firstToolMarker(candidate); m >= 0 {
		s.held.WriteString(candidate[m:])
		s.pending.Reset()
		s.capturing = true
		return candidate[:m]
	}
	if len(candidate) > sieveTail {
		safe := candidate[:len(candidate)-sieveTail]
		rest := candidate[len(candidate)-sieveTail:]
		s.pending.Reset()
		s.pending.WriteString(rest)
		return safe
	}
	return ""
}

// flush ends the turn: it returns the held envelope (to be parsed as tool
// calls) and any remaining buffered prose (to be streamed as content).
func (s *streamSieve) flush() (held string, rest string) {
	if s.capturing {
		s.capturing = false
		return s.held.String() + s.pending.String(), ""
	}
	rest = s.pending.String()
	s.pending.Reset()
	return "", rest
}

// drainForNativeTool drains whatever the sieve is still holding so the turn
// can switch back to native upstream tool-call events.
func (s *streamSieve) drainForNativeTool() string {
	var out strings.Builder
	if s.capturing {
		out.WriteString(s.held.String())
		s.held.Reset()
		s.capturing = false
	}
	out.WriteString(s.pending.String())
	s.pending.Reset()
	return out.String()
}

// toolMarkerRE matches the start of a tool-call envelope the model can emit:
// XYML/QNML protocol tags (<|XYML|tool_calls>, <:QNML:invoke ...>, <|invoke>)
// plus the legacy tool_calls JSON and function.name text contracts.
var toolMarkerRE = regexp.MustCompile(`(?i)<[|:][^>]*|<(?:tool_calls|tool_use|invoke|parameter)\b|\{\s*"?tool_calls"?\s*:|function\.name\s*:`)

// firstToolMarker returns the byte offset where the first tool-envelope
// marker begins in text, or -1 when the text is plain prose.  A leading code
// fence around the envelope is included in the capture range so it is held
// with the envelope instead of leaking as bare text.
func firstToolMarker(text string) int {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if strings.HasPrefix(trimmed, "```") {
		rest := trimmed[3:]
		for _, kw := range []string{"json", "xml", "text"} {
			if strings.HasPrefix(strings.ToLower(rest), kw) {
				rest = rest[len(kw):]
				break
			}
		}
		rest = strings.TrimLeft(rest, " \t\r\n")
		if m := toolMarkerRE.FindStringIndex(rest); m != nil {
			// capture from the fence (including any leading whitespace)
			return len(text) - len(trimmed)
		}
		return -1
	}
	if m := toolMarkerRE.FindStringIndex(text); m != nil {
		return m[0]
	}
	return -1
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

// emitToolCallsSSE sends the complete tool_calls payload as a streaming chunk
// with finish_reason="tool_calls", so streaming clients get the same
// tool_calls contract as the non-streaming path.
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
