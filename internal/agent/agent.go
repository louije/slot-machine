package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"slot-machine/internal/agent/store"
)

// Service is the agent's HTTP surface: the chat UI, the conversation API, and
// the SSE stream. It is mounted on the app's public port by the proxy, so the
// chat lives at the app's own origin.
type Service struct {
	db      *store.Store
	manager *Manager

	agentBin   string
	workDir    string // the machine slot — the agent's worktree
	repoDir    string
	dataDir    string
	configPath string
	envFunc    func() []string

	authMode   string // "header" or "none"
	authHeader string // where the already-authenticated identity arrives

	accessMode     string // "app" or "allAuth"
	accessEndpoint string // asked on the live slot's internal port
	// livePort reports the live slot's INTERNAL_PORT, or 0 when nothing is
	// live. A function rather than a value because the live slot changes on
	// every deploy, and injected rather than imported so this package stays
	// independent of the orchestrator.
	livePort func() int

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

// ServeHTTP gates first and routes second.
//
// The order is the point. This used to dispatch /chat, /chat.css and
// /chat/config before the auth check, and only checked /agent/* — so the config
// route, which served the HMAC signing secret, was reachable by anyone. Every
// path this handler owns now passes through requireAccess, and there is no
// branch above it in which to forget one.
func (a *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireAccess(w, r)
	if !ok {
		return
	}

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

	if r.URL.Path == "/agent/conversations" {
		switch r.Method {
		case "GET":
			a.handleListConversations(w, r)
		case "POST":
			a.handleCreateConversation(w, r, user)
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

func (a *Service) handleListConversations(w http.ResponseWriter, r *http.Request) {
	list, err := a.db.ListConversations()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if list == nil {
		list = []store.Conversation{}
	}
	writeJSON(w, 200, list)
}

// handleCreateConversation records the conversation against the authenticated
// user, which the router has already established.
//
// It used to accept a "user" field from the request body when no header
// verified, which meant the attribution in the transcript was whatever the
// client typed. The identity now comes from one place only.
func (a *Service) handleCreateConversation(w http.ResponseWriter, r *http.Request, user string) {
	id := fmt.Sprintf("conv-%d", time.Now().UnixNano())
	conv, err := a.db.CreateConversation(id, user)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, conv)
}

func (a *Service) handleGetConversation(w http.ResponseWriter, r *http.Request, convID string) {
	conv, err := a.db.GetConversation(convID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if conv == nil {
		http.NotFound(w, r)
		return
	}

	msgs, err := a.db.GetMessages(convID, 0)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, 200, map[string]any{
		"conversation": conv,
		"messages":     msgs,
	})
}

func (a *Service) handleDeleteConversation(w http.ResponseWriter, r *http.Request, convID string) {
	if a.manager.isPending(convID) {
		writeJSON(w, 409, map[string]string{"error": "agent is still working on this conversation"})
		return
	}
	if err := a.db.DeleteConversation(convID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

func (a *Service) handleSendMessage(w http.ResponseWriter, r *http.Request, convID string) {
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

	conv, err := a.db.GetConversation(convID)
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
	t := turn{
		convID:       convID,
		prompt:       msg.Content,
		sessionID:    conv.SessionID,
		bin:          a.resolveBin(),
		dir:          a.workDir,
		env:          a.buildAgentEnv(),
		allowedTools: a.allowedTools,
		settingsPath: a.agentPolicyPath(),
		model:        a.model,
		timeout:      a.timeout,
	}

	if _, err := a.db.AddMessage(convID, "user", msg.Content); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Both regenerated per turn: the agent shares this machine's filesystem, so a
	// fresh copy each turn bounds any tampering to a single turn.
	if err := a.writeAgentPolicy(); err != nil {
		log.Printf("agent: writing tool policy: %v", err)
	}
	promptPath, err := a.writeSystemPrompt()
	if err != nil {
		// Without its context the agent does not know where it is, what branch
		// it is on, or how to deploy. Refuse rather than run it blind.
		log.Printf("agent: writing system prompt: %v", err)
		http.Error(w, "cannot prepare the agent's context", 500)
		return
	}
	t.systemPromptPath = promptPath

	if err := a.manager.enqueue(t); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, 200, map[string]any{
		"queued": a.manager.activeConv() != convID,
	})
}

func (a *Service) resolveBin() string {
	if a.agentBin != "" {
		return a.agentBin
	}
	return "claude"
}

func (a *Service) buildAgentEnv() []string {
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

func (a *Service) handleCancel(w http.ResponseWriter, r *http.Request, convID string) {
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
func (a *Service) handleStream(w http.ResponseWriter, r *http.Request, convID string) {
	conv, err := a.db.GetConversation(convID)
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

		msgs, err := a.db.GetMessages(convID, afterID)
		if err != nil {
			log.Printf("stream: reading messages for %s: %v", convID, err)
		}
		for _, m := range msgs {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", m.ID, m.Type, m.Content)
			afterID = m.ID
		}
		flusher.Flush()

		conv, err := a.db.GetConversation(convID)
		if err != nil {
			log.Printf("stream: reading conversation %s: %v", convID, err)
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

// Options configures a Service.
type Options struct {
	Store   *store.Store
	Manager *Manager

	// Bin is the Claude CLI. Empty falls back to "claude" on PATH.
	Bin string
	// WorkDir is the machine slot: the agent's own worktree, which the daemon
	// never rewrites.
	WorkDir string
	// DataDir holds the generated tool policy, deliberately outside WorkDir.
	DataDir string
	// ConfigPath is denied to the agent's file tools.
	ConfigPath string
	// Env supplies the app's environment to each turn, evaluated per turn so an
	// edited env_file is picked up without a restart.
	Env func() []string

	AuthMode       string
	AuthHeader     string
	AccessMode     string
	AccessEndpoint string
	// LivePort reports the live slot's internal port, or 0 if nothing is live.
	// Nil means nothing is ever live, which denies every request in "app" mode.
	LivePort       func() int
	AllowedTools   []string
	DeniedCommands []string
	Model          string
	Timeout        time.Duration
	MachineBranch  string
	HumanBranch    string
	ChatTitle      string
	ChatAccent     string
}

// NewService wires the agent's HTTP surface.
func NewService(opts Options) *Service {
	return &Service{
		db:             opts.Store,
		manager:        opts.Manager,
		agentBin:       opts.Bin,
		workDir:        opts.WorkDir,
		dataDir:        opts.DataDir,
		configPath:     opts.ConfigPath,
		envFunc:        opts.Env,
		authMode:       opts.AuthMode,
		authHeader:     opts.AuthHeader,
		accessMode:     opts.AccessMode,
		accessEndpoint: opts.AccessEndpoint,
		livePort:       opts.LivePort,
		allowedTools:   opts.AllowedTools,
		deniedCommands: opts.DeniedCommands,
		model:          opts.Model,
		timeout:        opts.Timeout,
		machineBranch:  opts.MachineBranch,
		humanBranch:    opts.HumanBranch,
		chatTitle:      opts.ChatTitle,
		chatAccent:     opts.ChatAccent,
	}
}
