package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	store      *agentStore
	mu         sync.Mutex
	sessions   map[string]*agentSession // keyed by conversation ID
	agentBin   string
	stagingDir string
	envFunc    func() []string
	authMode   string // "hmac", "trusted", "none"
	authSecret string // hex-encoded HMAC secret (for "hmac" mode)
}

type agentSession struct {
	events chan agentEvent
	done   chan struct{}
	cmd    *exec.Cmd
}

type agentEvent struct {
	ID   int64  // message ID (for SSE reconnection)
	Type string // SSE event type: system, assistant, done
	Data string // SSE data (JSON)
}

var titlePattern = regexp.MustCompile(`\[\[TITLE:\s*(.+?)\]\]`)

func (a *agentService) extractUser(r *http.Request) string {
	header := r.Header.Get("X-SlotMachine-User")
	switch a.authMode {
	case "hmac":
		idx := strings.LastIndex(header, ":")
		if idx < 1 {
			return ""
		}
		user, sig := header[:idx], header[idx+1:]
		mac := hmac.New(sha256.New, []byte(a.authSecret))
		mac.Write([]byte(user))
		expected := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(sig), []byte(expected)) {
			return ""
		}
		return user
	case "trusted":
		return header
	default:
		return ""
	}
}

func (a *agentService) buildSystemPrompt() string {
	var b strings.Builder
	b.WriteString("You are an AI assistant embedded in a web application via slot-machine.\n")
	b.WriteString("You are working in the application's source code directory.\n")

	agentMD, err := os.ReadFile(filepath.Join(a.stagingDir, "agent.md"))
	if err == nil && len(agentMD) > 0 {
		b.WriteString("\n")
		b.Write(agentMD)
		if agentMD[len(agentMD)-1] != '\n' {
			b.WriteString("\n")
		}
	}

	b.WriteString("\n## Conversation titling\n")
	b.WriteString("Include a conversation title in your responses using this format on its own line:\n")
	b.WriteString("[[TITLE: short descriptive title]]\n")
	b.WriteString("Include this in your first response. You may include it again to update the title if the topic changes.\n")

	return b.String()
}

func (a *agentService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/chat" {
		a.handleChat(w, r)
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

func (a *agentService) handleChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!DOCTYPE html>
<html><head><title>slot-machine</title></head>
<body><div id="chat"></div></body>
</html>`)
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

	a.mu.Lock()
	if _, running := a.sessions[convID]; running {
		a.mu.Unlock()
		http.Error(w, "agent already running", 409)
		return
	}
	session := &agentSession{
		events: make(chan agentEvent, 100),
		done:   make(chan struct{}),
	}
	a.sessions[convID] = session
	a.mu.Unlock()

	// Store user message first.
	a.store.addMessage(convID, "user", msg.Content)

	// Spawn agent process.
	bin := a.agentBin
	if bin == "" {
		bin = "claude"
	}

	args := []string{
		"--output-format", "stream-json",
		"-p", msg.Content,
		"--system-prompt", a.buildSystemPrompt(),
	}
	if conv.SessionID != "" {
		args = append(args, "--resume", conv.SessionID)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = a.stagingDir
	if a.envFunc != nil {
		cmd.Env = a.envFunc()
	}
	session.cmd = cmd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		a.mu.Lock()
		delete(a.sessions, convID)
		a.mu.Unlock()
		http.Error(w, "failed to create pipe", 500)
		return
	}

	if err := cmd.Start(); err != nil {
		a.mu.Lock()
		delete(a.sessions, convID)
		a.mu.Unlock()
		http.Error(w, "failed to start agent", 500)
		return
	}

	go a.processAgentOutput(convID, session, stdout, cmd)

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

func (a *agentService) processAgentOutput(convID string, session *agentSession, stdout io.ReadCloser, cmd *exec.Cmd) {
	defer func() {
		a.mu.Lock()
		delete(a.sessions, convID)
		a.mu.Unlock()
		close(session.done)
		close(session.events)
	}()

	scanner := bufio.NewScanner(stdout)
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
			text, _ := raw["text"].(string)

			// Extract and strip [[TITLE: ...]] markers.
			if m := titlePattern.FindStringSubmatch(text); m != nil {
				a.store.updateTitle(convID, strings.TrimSpace(m[1]))
				text = strings.TrimSpace(titlePattern.ReplaceAllString(text, ""))
			}

			if text == "" {
				continue // title-only message, nothing to forward
			}

			data, _ := json.Marshal(map[string]string{"content": text})
			sseType = "assistant"
			sseData = string(data)

		case "result":
			// Extract usage and accumulate on conversation.
			inputTok, _ := raw["input_tokens"].(float64)
			outputTok, _ := raw["output_tokens"].(float64)
			cacheRead, _ := raw["cache_read"].(float64)
			cacheWrite, _ := raw["cache_write"].(float64)
			a.store.addUsage(convID, int(inputTok), int(outputTok), int(cacheRead), int(cacheWrite))

			sseType = "done"
			sseData = line // raw result JSON

		default:
			continue
		}

		// Database first, then SSE.
		msgID, _ := a.store.addMessage(convID, sseType, sseData)
		session.events <- agentEvent{ID: msgID, Type: sseType, Data: sseData}
	}
	cmd.Wait()
}

func (a *agentService) handleStream(w http.ResponseWriter, r *http.Request, convID string) {
	a.mu.Lock()
	session, ok := a.sessions[convID]
	a.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flusher.Flush()

	for {
		select {
		case evt, open := <-session.events:
			if !open {
				return
			}
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", evt.ID, evt.Type, evt.Data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
