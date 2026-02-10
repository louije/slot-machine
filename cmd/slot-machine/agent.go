package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type agentService struct {
	store          *agentStore
	mu             sync.Mutex
	sessions       map[string]*agentSession // keyed by conversation ID
	agentBin       string
	stagingDir     string
	envFunc        func() []string
	authMode     string   // "hmac", "trusted", "none"
	authSecret   string   // hex-encoded HMAC secret (for "hmac" mode)
	allowedTools []string // claude --allowed-tools
	chatTitle      string
	chatAccent     string
}

type agentSession struct {
	done chan struct{}
	cmd  *exec.Cmd
}

var titlePattern = regexp.MustCompile(`\[\[TITLE:\s*(.+?)\]\]`)

func (a *agentService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/chat" {
		a.handleChat(w, r)
		return
	}
	if r.URL.Path == "/chat.css" {
		a.handleChatCSS(w, r)
		return
	}
	if r.URL.Path == "/chat/config" {
		a.handleChatConfig(w, r)
		return
	}

	// Auth check for /agent/* paths in hmac mode.
	if strings.HasPrefix(r.URL.Path, "/agent/") && a.authMode == "hmac" {
		if a.extractUser(r) == "" {
			http.Error(w, "unauthorized", 401)
			return
		}
	}

	if r.URL.Path == "/agent/conversations" {
		switch r.Method {
		case "GET":
			a.handleListConversations(w, r)
		case "POST":
			a.handleCreateConversation(w, r)
		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}

	// /agent/conversations/:id[/sub]
	rest := strings.TrimPrefix(r.URL.Path, "/agent/conversations/")
	if rest == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	convID := parts[0]
	if len(parts) == 1 {
		a.handleGetConversation(w, r, convID)
		return
	}
	switch parts[1] {
	case "messages":
		a.handleSendMessage(w, r, convID)
	case "stream":
		a.handleStream(w, r, convID)
	case "cancel":
		a.handleCancel(w, r, convID)
	default:
		http.NotFound(w, r)
	}
}

func (a *agentService) handleListConversations(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.listConversations()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if list == nil {
		list = []conversationRow{}
	}
	writeJSON(w, 200, list)
}

func (a *agentService) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	user := a.extractUser(r)

	// Fallback: allow user from body in "none" mode.
	if user == "" && a.authMode != "hmac" {
		var req struct {
			User string `json:"user"`
		}
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&req)
		}
		user = req.User
	}

	id := fmt.Sprintf("conv-%d", time.Now().UnixNano())
	conv, err := a.store.createConversation(id, user)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, conv)
}

func (a *agentService) handleGetConversation(w http.ResponseWriter, r *http.Request, convID string) {
	conv, err := a.store.getConversation(convID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if conv == nil {
		http.NotFound(w, r)
		return
	}

	msgs, err := a.store.getMessages(convID, 0)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, 200, map[string]any{
		"conversation": conv,
		"messages":     msgs,
	})
}

func (a *agentService) handleSendMessage(w http.ResponseWriter, r *http.Request, convID string) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}

	var msg struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "bad request", 400)
		return
	}

	// Verify conversation exists.
	conv, err := a.store.getConversation(convID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if conv == nil {
		http.NotFound(w, r)
		return
	}

	a.store.addMessage(convID, "user", msg.Content)
	w.WriteHeader(200)
}

func (a *agentService) handleCancel(w http.ResponseWriter, r *http.Request, convID string) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}

	a.mu.Lock()
	session, ok := a.sessions[convID]
	a.mu.Unlock()
	if !ok {
		http.Error(w, "no running agent", 404)
		return
	}

	if session.cmd != nil && session.cmd.Process != nil {
		session.cmd.Process.Kill()
	}
	<-session.done

	w.WriteHeader(200)
}

func (a *agentService) streamAgentOutput(w http.ResponseWriter, flusher http.Flusher, r *http.Request, convID string, stdout io.ReadCloser, cmd *exec.Cmd) {
	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB max line
		for scanner.Scan() {
			line := scanner.Text()
			var raw map[string]any
			if json.Unmarshal([]byte(line), &raw) != nil {
				continue
			}

			evtType, _ := raw["type"].(string)
			var sseType, sseData string

			switch evtType {
			case "system":
				if sub, _ := raw["subtype"].(string); sub == "init" {
					if sid, ok := raw["session_id"].(string); ok {
						a.store.updateSessionID(convID, sid)
					}
				}
				sseType = "system"
				sseData = line

			case "assistant":
				// Extract content blocks from message.
				// Real Claude: {"type":"assistant","message":{"content":[...]}}
				// Content blocks can be text or tool_use.
				var blocks []any
				if msg, ok := raw["message"].(map[string]any); ok {
					blocks, _ = msg["content"].([]any)
				}

				// Emit tool_use events for any tool calls in this message.
				for _, b := range blocks {
					block, ok := b.(map[string]any)
					if !ok {
						continue
					}
					if bt, _ := block["type"].(string); bt == "tool_use" {
						toolName, _ := block["name"].(string)
						toolID, _ := block["id"].(string)
						data, _ := json.Marshal(map[string]string{"tool": toolName, "id": toolID})
						msgID, _ := a.store.addMessage(convID, "tool_use", string(data))
						fmt.Fprintf(w, "id: %d\nevent: tool_use\ndata: %s\n\n", msgID, string(data))
						flusher.Flush()
					}
				}

				// Collect text from all text blocks.
				var text string
				for _, b := range blocks {
					block, ok := b.(map[string]any)
					if !ok {
						continue
					}
					if bt, _ := block["type"].(string); bt == "text" {
						if t, _ := block["text"].(string); t != "" {
							text += t
						}
					}
				}

				// Extract and strip [[TITLE: ...]] markers.
				if m := titlePattern.FindStringSubmatch(text); m != nil {
					a.store.updateTitle(convID, strings.TrimSpace(m[1]))
					text = strings.TrimSpace(titlePattern.ReplaceAllString(text, ""))
				}

				if text == "" {
					continue // tool-only or title-only message
				}

				data, _ := json.Marshal(map[string]string{"content": text})
				sseType = "assistant"
				sseData = string(data)

			case "user":
				// Tool results come as user events: {"type":"user","message":{"content":[{"type":"tool_result",...}]}}
				var blocks []any
				if msg, ok := raw["message"].(map[string]any); ok {
					blocks, _ = msg["content"].([]any)
				}
				for _, b := range blocks {
					block, ok := b.(map[string]any)
					if !ok {
						continue
					}
					if bt, _ := block["type"].(string); bt == "tool_result" {
						toolID, _ := block["tool_use_id"].(string)
						content, _ := block["content"].(string)
						data, _ := json.Marshal(map[string]string{"id": toolID, "output": content})
						msgID, _ := a.store.addMessage(convID, "tool_result", string(data))
						fmt.Fprintf(w, "id: %d\nevent: tool_result\ndata: %s\n\n", msgID, string(data))
						flusher.Flush()
					}
				}
				continue

			case "result":
				// Usage is nested: {"usage":{"input_tokens":N,"output_tokens":N,...}}
				var inputTok, outputTok, cacheRead, cacheWrite float64
				if usage, ok := raw["usage"].(map[string]any); ok {
					inputTok, _ = usage["input_tokens"].(float64)
					outputTok, _ = usage["output_tokens"].(float64)
					cacheRead, _ = usage["cache_read_input_tokens"].(float64)
					cacheWrite, _ = usage["cache_creation_input_tokens"].(float64)
				}
				a.store.addUsage(convID, int(inputTok), int(outputTok), int(cacheRead), int(cacheWrite))

				// Extract title from result text (may not appear in assistant events).
				if resultText, _ := raw["result"].(string); resultText != "" {
					if m := titlePattern.FindStringSubmatch(resultText); m != nil {
						a.store.updateTitle(convID, strings.TrimSpace(m[1]))
					}
				}

				sseType = "done"
				sseData = line

			default:
				continue
			}

			// Database first, then SSE.
			msgID, _ := a.store.addMessage(convID, sseType, sseData)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", msgID, sseType, sseData)
			flusher.Flush()
		}
		cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-r.Context().Done():
		cmd.Process.Kill()
		cmd.Wait()
	}
}

func (a *agentService) handleStream(w http.ResponseWriter, r *http.Request, convID string) {
	// Verify conversation exists and get last user message.
	conv, err := a.store.getConversation(convID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if conv == nil {
		http.NotFound(w, r)
		return
	}

	msgs, err := a.store.getMessages(convID, 0)
	if err != nil || len(msgs) == 0 {
		http.Error(w, "no messages", 400)
		return
	}
	// Find last user message.
	var lastUserMsg string
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Type == "user" {
			lastUserMsg = msgs[i].Content
			break
		}
	}
	if lastUserMsg == "" {
		http.Error(w, "no user message", 400)
		return
	}

	// Reject if agent already running for this conversation.
	a.mu.Lock()
	if _, running := a.sessions[convID]; running {
		a.mu.Unlock()
		http.Error(w, "agent already running", 409)
		return
	}
	session := &agentSession{
		done: make(chan struct{}),
	}
	a.sessions[convID] = session
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.sessions, convID)
		a.mu.Unlock()
		close(session.done)
	}()

	// Spawn agent process.
	bin := a.agentBin
	if bin == "" {
		bin = "claude"
	}
	tools := a.allowedTools
	if len(tools) == 0 {
		tools = []string{"Bash", "Edit", "Read", "Write", "Glob", "Grep"}
	}
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--allowed-tools", strings.Join(tools, ","),
		"-p", lastUserMsg,
		"--system-prompt", a.buildSystemPrompt(),
	}
	if conv.SessionID != "" {
		args = append(args, "--resume", conv.SessionID)
	}

	// Build extra PATH entries: the slot-machine binary's dir and
	// ~/.local/bin (common user-local install location for claude).
	var extraDirs []string
	if self, err := os.Executable(); err == nil {
		extraDirs = append(extraDirs, filepath.Dir(self))
	}
	if home, err := os.UserHomeDir(); err == nil {
		extraDirs = append(extraDirs, filepath.Join(home, ".local", "bin"))
	}

	// exec.Command resolves the binary using the daemon's PATH, which under
	// systemd won't include ~/.local/bin. Check extra dirs manually.
	if filepath.Base(bin) == bin {
		if _, err := exec.LookPath(bin); err != nil {
			for _, dir := range extraDirs {
				candidate := filepath.Join(dir, bin)
				if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
					bin = candidate
					break
				}
			}
		}
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = a.stagingDir
	if a.envFunc != nil {
		cmd.Env = a.envFunc()
	}
	// Prepend extra dirs to the subprocess PATH too.
	if len(extraDirs) > 0 {
		prefix := strings.Join(extraDirs, ":")
		for i, e := range cmd.Env {
			if strings.HasPrefix(e, "PATH=") {
				cmd.Env[i] = "PATH=" + prefix + ":" + e[5:]
				break
			}
		}
	}
	session.cmd = cmd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "failed to create pipe", 500)
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "agent start error: %v (bin=%s)\n", err, bin)
		http.Error(w, "failed to start agent", 500)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		cmd.Process.Kill()
		cmd.Wait()
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flusher.Flush()

	// Stream agent output directly to client.
	a.streamAgentOutput(w, flusher, r, convID, stdout, cmd)
}
