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
	store   *agentStore
	manager *agentManager

	agentBin   string
	workDir    string // the machine slot — the agent's worktree
	repoDir    string
	dataDir    string
	configPath string
	envFunc    func() []string

	authMode       string // "hmac", "trusted", "none"
	authSecret     string // hex-encoded HMAC secret (for "hmac" mode)
	allowedTools   []string
	deniedCommands []string
	model          string
	timeout        time.Duration
	machineBranch  string
	humanBranch    string
	chatTitle      string
	chatAccent     string
}

var titlePattern = regexp.MustCompile(`\[\[TITLE:\s*(.+?)\]\]`)

// streamKeepalive bounds how long an SSE connection can sit silent. Agents can
// think for a long time between events, and an intermediary that sees nothing
// may close the connection.
const streamKeepalive = 15 * time.Second

func (a *agentService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/chat":
		a.handleChat(w, r)
		return
	case "/chat.css":
		a.handleChatCSS(w, r)
		return
	case "/chat/config":
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
		switch r.Method {
		case "GET":
			a.handleGetConversation(w, r, convID)
		case "DELETE":
			a.handleDeleteConversation(w, r, convID)
		default:
			http.Error(w, "method not allowed", 405)
		}
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

func (a *agentService) handleDeleteConversation(w http.ResponseWriter, r *http.Request, convID string) {
	if a.manager.isPending(convID) {
		writeJSON(w, 409, map[string]string{"error": "agent is still working on this conversation"})
		return
	}
	if err := a.store.deleteConversation(convID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
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
	if strings.TrimSpace(msg.Content) == "" {
		http.Error(w, "empty message", 400)
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

	// Refuse before storing, so a rejected message does not sit in the
	// transcript looking like it was accepted.
	work := agentWork{
		convID:       convID,
		prompt:       msg.Content,
		sessionID:    conv.SessionID,
		bin:          a.resolveBin(),
		dir:          a.workDir,
		env:          a.buildAgentEnv(),
		allowedTools: a.allowedTools,
		settingsPath: a.agentPolicyPath(),
		model:        a.model,
		systemPrompt: a.buildSystemPrompt(),
		timeout:      a.timeout,
	}

	if _, err := a.store.addMessage(convID, "user", msg.Content); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Regenerated per turn: the agent shares this machine's filesystem, so a
	// fresh copy each turn bounds any tampering to a single turn.
	if err := a.writeAgentPolicy(); err != nil {
		logf("agent: writing tool policy: %v", err)
	}

	if err := a.manager.enqueue(work); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, 200, map[string]any{
		"queued": a.manager.activeConv() != convID,
	})
}

func (a *agentService) resolveBin() string {
	if a.agentBin != "" {
		return a.agentBin
	}
	return "claude"
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

// handleStream serves the conversation as SSE.
//
// The loop grabs the manager's broadcast channel *before* reading messages, so
// an event landing between the read and the wait cannot be missed. It follows a
// conversation that is queued as well as one that is running — with a shared
// agent slot, waiting for your turn is a normal state and the stream should stay
// open through it.
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
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()

	// Replay missed events. Last-Event-ID covers EventSource's automatic
	// reconnect; ?after= covers a first connect with a known offset.
	var afterID int64
	if lastID := r.Header.Get("Last-Event-ID"); lastID != "" {
		fmt.Sscanf(lastID, "%d", &afterID)
	} else if after := r.URL.Query().Get("after"); after != "" {
		fmt.Sscanf(after, "%d", &afterID)
	}

	for {
		events := a.manager.events()

		msgs, err := a.store.getMessages(convID, afterID)
		if err != nil {
			logf("stream: reading messages for %s: %v", convID, err)
		}
		for _, m := range msgs {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", m.ID, m.Type, m.Content)
			afterID = m.ID
		}
		flusher.Flush()

		conv, err := a.store.getConversation(convID)
		if err != nil {
			logf("stream: reading conversation %s: %v", convID, err)
		}
		status := "idle"
		if conv != nil {
			status = conv.Status
		}
		if status != "running" && status != "queued" && !a.manager.isPending(convID) {
			fmt.Fprintf(w, "event: status\ndata: {\"status\":%q}\n\n", status)
			flusher.Flush()
			return
		}

		select {
		case <-events:
		case <-r.Context().Done():
			return
		case <-time.After(streamKeepalive):
			// SSE comment: keeps intermediaries from timing out a quiet stream.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
