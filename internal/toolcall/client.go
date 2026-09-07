package toolcall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// ToolCall is a parsed function call in OpenAI format.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolProfile is the detected CLI tool profile (+ its prompt block).
type ToolProfile struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Rules       []string `json:"rules"`
	Block       string   `json:"block"`
}

// InstructionSet is the sidecar /instructions response.
type InstructionSet struct {
	Instructions string      `json:"instructions"`
	Profile      ToolProfile `json:"profile"`
	Error        string      `json:"error"`
}

// HistoryItem is one flattened chat message from /render_history.
type HistoryItem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client manages the Python sidecar process and exposes tool-call helpers.
type Client struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	baseURL string
	hc      *http.Client
	started bool
}

// NewClient creates a tool-call client bound to 127.0.0.1:17090.
func NewClient() *Client {
	return &Client{
		baseURL: "http://127.0.0.1:17090",
		hc:      &http.Client{Timeout: 60 * time.Second},
	}
}

// Start launches the Python sidecar server and waits until it is healthy.
// startTimeout bounds the health-check wait; the subprocess lives for the
// lifetime of the parent process unless Stop is called.
func (c *Client) Start(ctx context.Context, startTimeout time.Duration) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	serverPy, err := findServerPy()
	if err != nil {
		return err
	}
	python, err := findPython()
	if err != nil {
		return fmt.Errorf("python not found: %w", err)
	}

	cmd := exec.Command(python, serverPy)
	cmd.Stdout = os.Stdout
	stderr := &bytes.Buffer{}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderr)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sidecar: %w", err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.started = true
	c.mu.Unlock()

	// Wait for health in the background; log the process exit if it dies.
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		c.started = false
		c.cmd = nil
		c.mu.Unlock()
		if stderr.Len() > 0 {
			log.Printf("[toolcall] sidecar stderr:\n%s", stderr.String())
		}
		if err != nil {
			log.Printf("[toolcall] sidecar exited: %v", err)
		} else {
			log.Printf("[toolcall] sidecar exited")
		}
	}()

	deadline := startTimeout
	if deadline <= 0 {
		deadline = 30 * time.Second
	}
	if err := c.WaitForHealth(ctx, deadline); err != nil {
		c.Stop()
		return err
	}
	log.Printf("[toolcall] sidecar ready on %s", c.baseURL)
	return nil
}

// Stop terminates the Python sidecar process.
func (c *Client) Stop() {
	c.mu.Lock()
	cmd := c.cmd
	started := c.started
	c.started = false
	c.cmd = nil
	c.mu.Unlock()
	if !started || cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

// IsRunning reports whether the sidecar process is alive.
func (c *Client) IsRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started && c.cmd != nil && c.cmd.Process != nil
}

// WaitForHealth polls GET /health until it returns ok or the timeout expires.
func (c *Client) WaitForHealth(ctx context.Context, timeout time.Duration) error {
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !c.IsRunning() {
			return fmt.Errorf("sidecar process died before becoming healthy")
		}
		if dctx.Err() != nil {
			return fmt.Errorf("sidecar health check timeout")
		}
		resp, err := c.hc.Get(c.baseURL + "/health")
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode == 200 && bytes.Contains(body, []byte("ok")) {
				return nil
			}
		}
		select {
		case <-ticker.C:
		case <-dctx.Done():
			return fmt.Errorf("sidecar health check timeout")
		}
	}
}

// BuildInstructions returns the XYML instruction block for the given tools,
// together with the detected CLI tool-profile prompt block.
// toolsJSON must be a JSON-serialized list of OpenAI tool definitions.
func (c *Client) BuildInstructions(ctx context.Context, toolsJSON []byte) (InstructionSet, error) {
	var out InstructionSet
	if err := c.postJSON(ctx, "/instructions", map[string]any{"tools": rawJSON(toolsJSON)}, &out); err != nil {
		return out, err
	}
	if out.Error != "" {
		return out, fmt.Errorf("sidecar: %s", out.Error)
	}
	return out, nil
}

// RenderHistory flattens OpenAI-style messages (with assistant tool_calls and
// tool results) into plain chat messages for the plain-LLM upstream.
// messagesJSON must be a JSON-serialized OpenAI messages array.
func (c *Client) RenderHistory(ctx context.Context, messagesJSON []byte) ([]HistoryItem, error) {
	var out struct {
		Items []HistoryItem `json:"items"`
		Error string        `json:"error"`
	}
	if err := c.postJSON(ctx, "/render_history", map[string]any{"messages": rawJSON(messagesJSON)}, &out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, fmt.Errorf("sidecar: %s", out.Error)
	}
	return out.Items, nil
}

// ParseToolCalls extracts tool calls from model output text using the full
// xyml engine (markup/XML/JSON/text-KV with CDATA-aware recovery).
// toolsJSON must be the same JSON tool list passed to BuildInstructions.
func (c *Client) ParseToolCalls(ctx context.Context, text string, toolsJSON []byte) ([]ToolCall, error) {
	var out struct {
		ToolCalls []ToolCall `json:"tool_calls"`
		Error     string     `json:"error"`
	}
	if err := c.postJSON(ctx, "/parse", map[string]any{
		"text":  text,
		"tools": rawJSON(toolsJSON),
	}, &out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, fmt.Errorf("sidecar: %s", out.Error)
	}
	return out.ToolCalls, nil
}

// RecoverToolCalls parses model output and, when parsing fails, reports why:
// reason is "truncated", "parse_failed" or "" (no tool attempt).  A non-empty
// reason means the caller should issue a corrective retry turn.
func (c *Client) RecoverToolCalls(ctx context.Context, text string, toolsJSON []byte) ([]ToolCall, string, error) {
	var out struct {
		ToolCalls []ToolCall `json:"tool_calls"`
		Reason    string     `json:"reason"`
		Error     string     `json:"error"`
	}
	if err := c.postJSON(ctx, "/recover", map[string]any{
		"text":  text,
		"tools": rawJSON(toolsJSON),
	}, &out); err != nil {
		return nil, "", err
	}
	if out.Error != "" {
		return nil, "", fmt.Errorf("sidecar: %s", out.Error)
	}
	return out.ToolCalls, out.Reason, nil
}

// RetryMessage builds the corrective user message for a retry turn after a
// truncated / unparseable tool-call output.
func (c *Client) RetryMessage(ctx context.Context, originalOutput, reason string) (string, error) {
	var out struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := c.postJSON(ctx, "/retry_message", map[string]any{
		"original_output": originalOutput,
		"reason":          reason,
	}, &out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", fmt.Errorf("sidecar: %s", out.Error)
	}
	return out.Message, nil
}

// --- helpers ---

type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	if bytes.TrimSpace(r)[0] == '[' {
		return r, nil
	}
	return json.Marshal(r)
}

func (c *Client) postJSON(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if !c.IsRunning() {
		return fmt.Errorf("sidecar not running")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("sidecar %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("sidecar %s read: %w", path, err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("sidecar %s: status %d: %s", path, resp.StatusCode, string(data))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("sidecar %s decode: %w", path, err)
	}
	return nil
}

func findServerPy() (string, error) {
	// Resolve the directory of this file at runtime (internal/toolcall/server.py).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve sidecar script location")
	}
	candidate := filepath.Join(filepath.Dir(thisFile), "server.py")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	// Fallback: $CWD/internal/toolcall/server.py (tests, `go run`)
	wd, err := os.Getwd()
	if err == nil {
		candidate = filepath.Join(wd, "internal", "toolcall", "server.py")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("server.py not found")
}

func findPython() (string, error) {
	for _, name := range []string{"python", "python3", "py"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no python interpreter on PATH")
}