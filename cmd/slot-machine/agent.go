package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
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

func (a *agentService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/chat" {
		a.handleChat(w, r)
		return
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
	var req struct {
		User string `json:"user"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req) // best-effort
	}

	id := fmt.Sprintf("conv-%d", time.Now().UnixNano())
	conv, err := a.store.createConversation(id, req.User)
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
