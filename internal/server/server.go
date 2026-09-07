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
	"strings"
	"time"

	"edgeone2api/internal/auth"
	"edgeone2api/internal/config"
	"edgeone2api/internal/upstream"
)

// Server is an OpenAI-compatible API server backed by DeepSeek Harness sessions
type Server struct {
	pool     *auth.Pool
	apiKey   string
	models   []string
	timeout  time.Duration
	modelMap map[string]config.ModelMapping
}

// New creates a new server
func New(pool *auth.Pool, apiKey string, models []string, timeout time.Duration, modelMap map[string]config.ModelMapping) *Server {
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
		items := s.buildItems(session, sessionKey, msgs, toolsJSON)
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
		// mid-stream (vs. the text-protocol tool_calls JSON parsed after the turn).
		nativeToolCall := false
		toolSeen := make(map[int]bool) // index -> name delta sent
		// With tools declared, JSON-looking content is held back until the
		// turn ends: a malformed tool_calls JSON is repaired or reprised
		// instead of being streamed to the client as broken text.  Plain
		// prose diverges immediately and streams live as usual.
		holdContent := toolsJSON != ""
		live := false
		var held strings.Builder

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
				if holdContent && !live {
					candidate := held.String() + chunk.Text
					if jsonish(candidate) && held.Len() <= maxToolCallBuf {
						held.WriteString(chunk.Text)
					} else {
						live = true
						emitText(held.String())
						held.Reset()
						emitText(chunk.Text)
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
				if holdContent && !live {
					live = true
					emitText(held.String())
					held.Reset()
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
			} else if calls := translateToolCalls(parseToolCalls(held.String()), declared); calls != nil {
				emitToolCallsSSE(w, chatID, model, created, calls)
				flusher.Flush()
				finishReason = "tool_calls"
			} else if calls := s.retryToolCallsOnce(ctx, session, toolsJSON, declared, held.String()); calls != nil {
				emitToolCallsSSE(w, chatID, model, created, calls)
				flusher.Flush()
				finishReason = "tool_calls"
			} else if held.Len() > 0 {
				// held content was not a tool declaration after all — stream it now
				emitText(held.String())
				held.Reset()
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
		items := s.buildItems(session, sessionKey, msgs, toolsJSON)
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
	// back to the declared-tool JSON text protocol when the model emitted a
	// tool_calls JSON as plain text instead.  Both paths go through the tool
	// translation layer so tool names match the client's declared set.
	if calls := translateToolCalls(toOpenAIToolCalls(result.ToolCalls), declared); len(calls) > 0 {
		msg["content"] = nil
		msg["tool_calls"] = calls
		finish = "tool_calls"
	} else if calls := translateToolCalls(parseToolCalls(result.Text), declared); calls != nil {
		msg["content"] = nil
		msg["tool_calls"] = calls
		finish = "tool_calls"
	} else if calls := s.retryToolCallsOnce(ctx, session, toolsJSON, declared, result.Text); calls != nil {
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

// buildItems assembles the prompt for a session: the tool-definition
// directive (with declared tools when any), an optional cache-hit replay of
// the previous dialog history, then the request messages.  Replay is
// recomputed per session so a fresh session created by a quota retry inherits
// the cached context automatically.
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

// maxToolCallBuf caps how much JSON-looking content is held back before it is
// streamed live.  Tool-call declarations are short, so this is generous; a
// genuinely long final answer crossing the limit is flushed and streamed.
const maxToolCallBuf = 16 * 1024

// retryToolCallsOnce asks the session for one corrected tool_calls JSON after
// the model's first attempt failed to parse (e.g. an unescaped arguments
// field).  It only fires when tools were declared and the broken text looked
// like a JSON document; returns nil otherwise.  This is the last-resort
// recovery on top of the tolerant parser.
func (s *Server) retryToolCallsOnce(ctx context.Context, session *auth.Session, toolsJSON string, declared declaredToolSet, broken string) []openaiToolCall {
	if toolsJSON == "" || session == nil || !jsonish(broken) {
		return nil
	}
	correction := "[Protocol Error Recovery]\n" +
		"Your previous answer did not follow the Tool Calling Protocol: it was not valid JSON (usually an unescaped arguments field or a wrong structure).\n" +
		"Output ONLY the corrected tool_calls JSON object now, and nothing else (no code fence, no explanation, no preamble).\n" +
		`Remember: arguments is a JSON OBJECT (one key per parameter, values are ordinary JSON), never a JSON-encoded string; inside string values escape quotes as \" and backslashes as \\.` + "\n" +
		"Previous broken answer for reference (truncated):\n" + truncateBroken(broken)
	items := []upstream.ContentItem{{Type: "text", Text: correction}}
	cs, err := session.Client.StartChat(ctx, session.SessionID, session.ConversationID, items)
	if err != nil {
		log.Printf("[RETRY] corrective prompt failed: %v", err)
		return nil
	}
	res, err := session.Client.StreamEvents(ctx, cs, nil)
	if err != nil {
		log.Printf("[RETRY] corrective turn failed: %v", err)
		return nil
	}
	return translateToolCalls(parseToolCalls(res.Text), declared)
}

func truncateBroken(s string) string {
	const maxLen = 2000
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// parseToolCalls tries to interpret the model's text output as a tool_calls
// JSON (the declared-tool protocol).  It tolerates a surrounding code
// fence, both the object-form arguments (preferred protocol) and the legacy
// JSON-string form, and repairs the malformed "arguments":"{...}" shape the
// model occasionally emits.  Returns nil when the text is not a tool_calls
// JSON, meaning the model answered directly.
func parseToolCalls(text string) []openaiToolCall {
	t := stripJSONFence(text)
	if t == "" {
		return nil
	}
	if calls := tryParseToolCalls(t); calls != nil {
		return calls
	}
	if repaired := unwrapWrappedArguments(t); repaired != t {
		if calls := tryParseToolCalls(repaired); calls != nil {
			return calls
		}
	}
	return nil
}

// parsedToolCallCandidate mirrors the model-emitted tool_calls JSON with a
// flexible arguments field (object or JSON-string).
type parsedToolCallCandidate struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func stripJSONFence(text string) string {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "```json")
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimSuffix(t, "```")
	return strings.TrimSpace(t)
}

func tryParseToolCalls(t string) []openaiToolCall {
	var parsed struct {
		ToolCalls []parsedToolCallCandidate `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(t), &parsed); err != nil {
		return nil
	}
	if len(parsed.ToolCalls) == 0 {
		return nil
	}
	calls := make([]openaiToolCall, 0, len(parsed.ToolCalls))
	for i, c := range parsed.ToolCalls {
		if c.Function.Name == "" {
			continue
		}
		args, err := normalizeToolArguments(c.Function.Arguments)
		if err != nil {
			return nil
		}
		typ := c.Type
		if typ == "" {
			typ = "function"
		}
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", randHex(6), i)
		}
		calls = append(calls, openaiToolCall{
			ID:   id,
			Type: typ,
			Function: openaiToolCallFn{
				Name:      c.Function.Name,
				Arguments: args,
			},
		})
	}
	return calls
}

// normalizeToolArguments converts the model-emitted arguments value into the
// client-facing JSON string.  Object form (preferred protocol) is
// compact-marshaled; string form (legacy) is passed through untouched.
func normalizeToolArguments(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", err
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unwrapWrappedArguments repairs the malformed shape the model occasionally
// emits for the arguments field:
//
//	"arguments":"{...valid json object...}"
//
// The wrapping quotes are stripped so the object parses as a real JSON value.
// The inner object must already be well-formed; anything else is left as-is.
// Returns the repaired text, or the input unchanged when nothing matched.
func unwrapWrappedArguments(text string) string {
	out := text
	offset := 0
	const key = `"arguments"`
	for {
		idx := strings.Index(out[offset:], key)
		if idx < 0 {
			break
		}
		idx += offset
		i := skipJSONSpace(out, idx+len(key))
		if i >= len(out) || out[i] != ':' {
			offset = idx + len(key)
			continue
		}
		j := skipJSONSpace(out, i+1)
		if j+1 >= len(out) || out[j] != '"' || out[j+1] != '{' {
			offset = j + 1
			continue
		}
		end := findJSONObjectEnd(out, j+1)
		if end <= j+1 {
			offset = j + 1
			continue
		}
		k := skipJSONSpace(out, end+1)
		if k >= len(out) || out[k] != '"' {
			offset = k
			continue
		}
		// strip the wrapping quotes: out[j] is the opening quote, out[k] the closing one
		out = out[:j] + out[j+1:k] + out[k+1:]
		offset = j
	}
	return out
}

func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func skipJSONSpace(s string, i int) int {
	for i < len(s) && isJSONSpace(s[i]) {
		i++
	}
	return i
}

// findJSONObjectEnd returns the index of the '}' matching the '{' at start,
// or -1 when the object is unbalanced.  JSON-aware: quoted strings and their
// escapes are skipped, so braces inside string values do not count.
func findJSONObjectEnd(s string, start int) int {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// jsonish reports whether the text looks like a JSON document declaration
// (after removing a code fence), i.e. starts with '{' or '['.
func jsonish(text string) bool {
	t := strings.TrimSpace(stripJSONFence(text))
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
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
