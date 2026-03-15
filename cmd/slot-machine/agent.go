package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type agentService struct {
	store        *agentStore
	manager      *agentManager
	agentBin     string
	stagingDir   string
	configPath   string
	dataDir      string
	envFunc      func() []string
	authMode     string   // "hmac", "trusted", "none"
	authSecret   string   // hex-encoded HMAC secret (for "hmac" mode)
	allowedTools []string // claude --allowed-tools
	chatTitle    string
	chatAccent   string
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

	// Generate deny rules before spawning agent.
	a.generateDenySettings()

	// Build agent invocation.
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
		"-p", msg.Content,
		"--system-prompt", a.buildSystemPrompt(),
	}
	if conv.SessionID != "" {
		args = append(args, "--resume", conv.SessionID)
	}

	env := a.buildAgentEnv()

	err = a.manager.enqueue(agentWork{
		convID:    convID,
		message:   msg.Content,
		sessionID: conv.SessionID,
		bin:       bin,
		args:      args,
		dir:       a.stagingDir,
		env:       env,
	})
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}

	w.WriteHeader(200)
}

func (a *agentService) buildAgentEnv() []string {
	var env []string
	if a.envFunc != nil {
		env = a.envFunc()
	}
	var extraDirs []string
	if self, err := os.Executable(); err == nil {
		extraDirs = append(extraDirs, filepath.Dir(self))
	}
	if home, err := os.UserHomeDir(); err == nil {
		extraDirs = append(extraDirs, filepath.Join(home, ".local", "bin"))
	}
	if len(extraDirs) > 0 {
		prefix := strings.Join(extraDirs, ":")
		for i, e := range env {
			if strings.HasPrefix(e, "PATH=") {
				env[i] = "PATH=" + prefix + ":" + e[5:]
				break
			}
		}
	}
	env = append(env, "DISABLE_AUTOUPDATER=1")
	return env
}

func (a *agentService) handleCancel(w http.ResponseWriter, r *http.Request, convID string) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if err := a.manager.cancel(convID); err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	w.WriteHeader(200)
}

func (a *agentService) handleStream(w http.ResponseWriter, r *http.Request, convID string) {
	conv, err := a.store.getConversation(convID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if conv == nil {
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

	// Replay missed events. Accept Last-Event-ID header (auto-reconnect)
	// or ?after= query param (initial connect with known offset).
	var afterID int64
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		fmt.Sscanf(lastID, "%d", &afterID)
	} else if after := r.URL.Query().Get("after"); after != "" {
		fmt.Sscanf(after, "%d", &afterID)
	}

	msgs, _ := a.store.getMessages(convID, afterID)
	for _, m := range msgs {
		fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", m.ID, m.Type, m.Content)
		afterID = m.ID
	}
	flusher.Flush()

	// Subscribe to live broadcast if agent is running.
	ra := a.manager.getRunning(convID)
	if ra == nil {
		fmt.Fprintf(w, "event: status\ndata: {\"status\":%q}\n\n", conv.Status)
		flusher.Flush()
		return
	}

	// Live subscription loop with ticker-based wakeup.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ticker.C:
				ra.cond.Broadcast()
			case <-ra.done:
				ra.cond.Broadcast()
				return
			}
		}
	}()

	ra.mu.Lock()
	lastSeq := ra.eventSeq
	ra.mu.Unlock()

	for {
		if r.Context().Err() != nil {
			return
		}

		ra.mu.Lock()
		for ra.eventSeq == lastSeq && r.Context().Err() == nil {
			ra.cond.Wait()
		}
		lastSeq = ra.eventSeq
		ra.mu.Unlock()

		if r.Context().Err() != nil {
			return
		}

		newMsgs, _ := a.store.getMessages(convID, afterID)
		for _, msg := range newMsgs {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", msg.ID, msg.Type, msg.Content)
			afterID = msg.ID
		}
		flusher.Flush()

		select {
		case <-ra.done:
			finalMsgs, _ := a.store.getMessages(convID, afterID)
			for _, msg := range finalMsgs {
				fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", msg.ID, msg.Type, msg.Content)
			}
			conv, _ := a.store.getConversation(convID)
			status := "idle"
			if conv != nil {
				status = conv.Status
			}
			fmt.Fprintf(w, "event: status\ndata: {\"status\":%q}\n\n", status)
			flusher.Flush()
			return
		default:
		}
	}
}
