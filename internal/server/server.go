package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
}

type openaiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
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

	items := convertMessages(req.Messages)

	// Session affinity: every request gets a session key for continuity.
	sessionKey := r.Header.Get("X-Session-Key")
	if sessionKey == "" {
		sessionKey = "auto-" + randHex(16)
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	session, err := s.pool.Bind(ctx, sessionKey)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
		return
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

	if req.Stream {
		s.streamChat(w, ctx, session, chatID, model, created, items, sessionKey)
	} else {
		s.nonStreamChat(w, ctx, session, chatID, model, created, items, sessionKey)
	}
}

func (s *Server) streamChat(w http.ResponseWriter, ctx context.Context, session *auth.Session, chatID, model string, created int64, items []upstream.ContentItem, sessionKey string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.pool.ReleaseBind(sessionKey, session, false)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}

	// Try once with the bound session; if quota error, retry with a fresh one.
	for attempt := 0; attempt < 2; attempt++ {
		cs, err := session.Client.StartChat(ctx, session.SessionID, session.ConversationID, items)
		if err != nil {
			if upstream.IsQuotaError(err) {
				session.MarkQuotaExceeded()
				s.pool.ReleaseBind(sessionKey, session, false)
				session, err = s.pool.Bind(ctx, sessionKey)
				if err != nil {
					writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
					return
				}
				continue // retry
			}
			s.pool.ReleaseBind(sessionKey, session, false)
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
			s.pool.ReleaseBind(sessionKey, session, success)
		}()

		_, streamErr := session.Client.StreamEvents(ctx, cs, func(chunk upstream.AssistantChunk) {
			if chunk.IsDone {
				emitFinish(w, chatID, model, created, "stop")
				flusher.Flush()
				return
			}

			delta := map[string]any{}
			if chunk.Text != "" {
				delta["content"] = chunk.Text
			}
			if chunk.Reasoning != "" {
				delta["reasoning_content"] = chunk.Reasoning
			}
			if len(delta) > 0 {
				emitSSE(w, chatID, model, created, delta)
				flusher.Flush()
			}
		})
		if streamErr != nil {
			log.Printf("[STREAM] error: %v", streamErr)
			success = false
		} else {
			success = true
		}

		emitFinish(w, chatID, model, created, "stop")
		flusher.Flush()
		return
	}
	// Both attempts failed
	s.pool.ReleaseBind(sessionKey, session, false)
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream quota exceeded, retry later"})
}

func (s *Server) nonStreamChat(w http.ResponseWriter, ctx context.Context, session *auth.Session, chatID, model string, created int64, items []upstream.ContentItem, sessionKey string) {
	var result upstream.ChatResult
	var err error

	for attempt := 0; attempt < 2; attempt++ {
		var cs *upstream.ChatStream
		cs, err = session.Client.StartChat(ctx, session.SessionID, session.ConversationID, items)
		if err != nil {
			if upstream.IsQuotaError(err) {
				session.MarkQuotaExceeded()
				s.pool.ReleaseBind(sessionKey, session, false)
				session, err = s.pool.Bind(ctx, sessionKey)
				if err != nil {
					writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "no available session: " + err.Error(), "type": "server_error"}})
					return
				}
				continue
			}
			s.pool.ReleaseBind(sessionKey, session, false)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"}})
			return
		}
		result, err = session.Client.StreamEvents(ctx, cs, nil)
		if err != nil {
			log.Printf("[CHAT] stream error: %v", err)
		}
		break
	}
	if err != nil {
		s.pool.ReleaseBind(sessionKey, session, false)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"}})
		return
	}

	success := true

	msg := map[string]any{"role": "assistant", "content": result.Text}

	resp := map[string]any{
		"id":      chatID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}

	s.pool.ReleaseBind(sessionKey, session, success)
	w.Header().Set("X-Session-Key", sessionKey)
	writeJSON(w, http.StatusOK, resp)
}

// --- Message conversion ---

func convertMessages(msgs []openaiMessage) []upstream.ContentItem {
	var items []upstream.ContentItem

	items = append(items, upstream.ContentItem{Type: "text", Text: "[System Directive]\nYou are an AI assistant accessed through an API. Answer the user's question directly and concisely.\n---\n"})

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
		case "tool":
			// Tool calls are not supported; skip tool role messages
			continue
		}
	}
	return items
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