package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"slot-machine/internal/agent/store"
	"slot-machine/internal/config"
)

// turn is one message to run through the agent.
//
// The manager builds the argv rather than the caller, so that session resolution
// can decide between --resume and a fresh session without the caller's help.
type turn struct {
	convID       string
	prompt       string
	sessionID    string
	bin          string
	dir          string
	env          []string
	allowedTools []string
	settingsPath string
	model        string
	systemPrompt string
	timeout      time.Duration
	attempt      int
}

type runningAgent struct {
	convID string
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager owns every agent subprocess. One runs at a time: they all share a
// single worktree, so two concurrent agents would edit the same files.
type Manager struct {
	db *store.Store

	mu      sync.Mutex
	queue   []turn
	current *runningAgent // nil when idle; at most one agent runs at a time
	notify  chan struct{} // closed and replaced on every event, for SSE wakeups

	wake   chan struct{}
	stopCh chan struct{}
	wg     sync.WaitGroup
}

const (
	// maxAttempts bounds transient-failure retries, including the first try.
	maxAttempts = 3
	// stderrTailBytes caps how much of the CLI's stderr we keep. It is where the
	// CLI explains its failures, and the tail is the part that matters.
	stderrTailBytes = 10_000
)

// NewManager starts the manager's scheduling loop.
func NewManager(st *store.Store) *Manager {
	m := &Manager{
		db:     st,
		notify: make(chan struct{}),
		wake:   make(chan struct{}, 1),
		stopCh: make(chan struct{}),
	}
	m.wg.Add(1)
	go m.loop()
	return m
}

// ---------------------------------------------------------------------------
// Event broadcast
//
// One channel for the whole manager, closed and replaced on each event. SSE
// handlers grab the current channel, then block on it. This replaces a
// sync.Cond woken by a 500ms ticker goroutine per stream — the same polling,
// but without the goroutines and without the up-to-500ms latency.
// ---------------------------------------------------------------------------

func (m *Manager) events() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notify
}

func (m *Manager) broadcast() {
	m.mu.Lock()
	close(m.notify)
	m.notify = make(chan struct{})
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Queue
// ---------------------------------------------------------------------------

// enqueue accepts a turn. At most one agent runs at a time across all
// conversations — they share a single worktree, so two concurrent agents would
// be editing the same files. Work beyond the first waits in FIFO order and is
// drained as the running agent finishes.
//
// Returns an error only if the same conversation is already queued or running;
// a busy *other* conversation is not an error, it is a queue.
func (m *Manager) enqueue(t turn) error {
	m.mu.Lock()
	if m.current != nil && m.current.convID == t.convID {
		m.mu.Unlock()
		return fmt.Errorf("agent already running for this conversation")
	}
	for _, q := range m.queue {
		if q.convID == t.convID {
			m.mu.Unlock()
			return fmt.Errorf("a message is already queued for this conversation")
		}
	}
	m.queue = append(m.queue, t)
	m.mu.Unlock()

	if err := m.db.SetConversationStatus(t.convID, "queued"); err != nil {
		log.Printf("store: setting queued status for %s: %v", t.convID, err)
	}
	m.broadcast()
	m.poke()
	return nil
}

func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) loop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stopCh:
			return
		case <-m.wake:
			m.drain()
		case <-time.After(time.Second):
			// Safety net: a missed poke must not strand the queue forever.
			m.drain()
		}
	}
}

// drain starts the next queued turn if the agent slot is free.
func (m *Manager) drain() {
	m.mu.Lock()
	if m.current != nil || len(m.queue) == 0 {
		m.mu.Unlock()
		return
	}
	t := m.queue[0]
	m.queue = m.queue[1:]

	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	ra := &runningAgent{convID: t.convID, cancel: cancel, done: make(chan struct{})}
	m.current = ra
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.runAgent(ctx, t, ra)

		m.mu.Lock()
		m.current = nil
		m.mu.Unlock()
		close(ra.done)
		m.broadcast()
		m.poke() // the slot is free — start whatever is next
	}()
}

// Stop cancels the running turn, drops the queue and waits for the loop to exit.
func (m *Manager) Stop() {
	close(m.stopCh)
	m.mu.Lock()
	if m.current != nil {
		m.current.cancel()
	}
	m.queue = nil
	m.mu.Unlock()
	m.wg.Wait()
}

// activeConv reports the conversation currently holding the agent slot.
func (m *Manager) activeConv() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return ""
	}
	return m.current.convID
}

// isPending reports whether a conversation is running or waiting in the queue,
// which is what tells an SSE handler to keep the stream open.
func (m *Manager) isPending(convID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.convID == convID {
		return true
	}
	for _, q := range m.queue {
		if q.convID == convID {
			return true
		}
	}
	return false
}

func (m *Manager) cancel(convID string) error {
	m.mu.Lock()
	// Cancelling something still in the queue just removes it.
	for i, q := range m.queue {
		if q.convID == convID {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			m.mu.Unlock()
			m.setStatus(convID, "cancelled")
			m.broadcast()
			return nil
		}
	}
	ra := m.current
	if ra == nil || ra.convID != convID {
		m.mu.Unlock()
		return fmt.Errorf("no running agent for %s", convID)
	}
	m.mu.Unlock()

	// Cancelling the context sends SIGTERM to the process group; WaitDelay
	// escalates to SIGKILL if it does not exit.
	ra.cancel()
	<-ra.done
	return nil
}

// ---------------------------------------------------------------------------
// Session resolution
// ---------------------------------------------------------------------------

// sessionFilePath returns where the Claude CLI stores a session transcript:
// ~/.claude/projects/<munged-cwd>/<session-id>.jsonl, with path separators and
// dots in the working directory replaced by dashes.
func sessionFilePath(home, workDir, sessionID string) string {
	munged := strings.NewReplacer("/", "-", ".", "-").Replace(workDir)
	return filepath.Join(home, ".claude", "projects", munged, sessionID+".jsonl")
}

// resolveResume decides whether --resume can be used. A stale session id fails
// opaquely inside the CLI and bricks the rest of the conversation, so verify the
// transcript is actually on disk first and fall back to a fresh session if not.
func (m *Manager) resolveResume(t turn) bool {
	if t.sessionID == "" {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if _, err := os.Stat(sessionFilePath(home, t.dir, t.sessionID)); err == nil {
		return true
	}

	log.Printf("agent: resume target %s missing on disk, starting fresh (%s)", t.sessionID, t.convID)
	if err := m.db.ClearSessionID(t.convID); err != nil {
		log.Printf("store: clearing session id for %s: %v", t.convID, err)
	}
	m.storeAndBroadcast(t.convID, "system", jsonContent(
		"Previous session transcript is gone; starting a fresh session. Earlier context is not available to the agent."))
	return false
}

func buildAgentArgs(t turn, resume bool) []string {
	tools := t.allowedTools
	if len(tools) == 0 {
		tools = config.DefaultAllowedTools
	}

	args := []string{"--output-format", "stream-json", "--verbose"}

	// Always explicit. Left unset, every run silently inherits whatever the
	// server user's ~/.claude/settings.json happens to say, which is not
	// something a deploy tool should ride on.
	if t.model != "" {
		args = append(args, "--model", t.model)
	}

	args = append(args, "--allowed-tools", strings.Join(tools, ","))

	// Tool policy from outside the agent's worktree. --settings only ever adds
	// to the project settings, so `deny` is the only direction that restricts —
	// which is exactly what this file carries.
	if t.settingsPath != "" {
		args = append(args, "--settings", t.settingsPath)
	}

	// Append rather than replace: --system-prompt discards the CLI's own tool
	// guidance, which is not ours to throw away. We are adding context, not
	// substituting for it.
	args = append(args, "--append-system-prompt", t.systemPrompt)

	if resume {
		args = append(args, "--resume", t.sessionID)
	}

	// "--" so a message beginning with "-" is not parsed as a flag.
	args = append(args, "-p", "--", t.prompt)
	return args
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

func (m *Manager) runAgent(ctx context.Context, t turn, ra *runningAgent) {
	resume := m.resolveResume(t)
	args := buildAgentArgs(t, resume)

	cmd := exec.CommandContext(ctx, t.bin, args...)
	cmd.Dir = t.dir
	if t.env != nil {
		cmd.Env = t.env
	}
	// The CLI spawns children (shells, tools); signal the whole group so a
	// cancel does not leave orphans holding the worktree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.failRun(t.convID, fmt.Sprintf("Could not start the agent: %v", err))
		return
	}
	// stderr must be drained, not discarded: it is where the CLI explains auth
	// failures, spend caps and bad flags. Previously it went to /dev/null, which
	// is why every failure surfaced as a bare exit code.
	//
	// A writer rather than StderrPipe: Wait closes a pipe as soon as the child
	// exits, so a reader goroutine racing Wait can lose the tail — which is
	// exactly the part worth keeping. Handing exec an io.Writer makes it own the
	// copy, and Wait does not return until that copy is done.
	stderr := &tailWriter{max: stderrTailBytes}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		m.failRun(t.convID, fmt.Sprintf("Could not start the agent: %v", err))
		return
	}

	m.setStatus(t.convID, "running")
	if err := m.db.SetConversationPID(t.convID, cmd.Process.Pid); err != nil {
		log.Printf("store: recording pid for %s: %v", t.convID, err)
	}
	m.broadcast()

	var lastText string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		if text := m.processLine(t.convID, scanner.Text()); text != "" {
			lastText = text
		}
	}

	waitErr := cmd.Wait()
	stderrTail := stderr.String()

	if err := m.db.SetConversationPID(t.convID, 0); err != nil {
		log.Printf("store: clearing pid for %s: %v", t.convID, err)
	}

	if waitErr == nil {
		m.setStatus(t.convID, "idle")
		return
	}

	// Timeout is not an API failure; report it as itself.
	if ctx.Err() == context.DeadlineExceeded {
		m.failRun(t.convID, fmt.Sprintf(
			"The agent ran longer than %s and was stopped. Send a new message to continue.",
			t.timeout))
		return
	}
	if ctx.Err() == context.Canceled {
		m.storeAndBroadcast(t.convID, "system", jsonContent("Cancelled."))
		m.setStatus(t.convID, "cancelled")
		return
	}

	diagnostic := lastText + "\n" + stderrTail
	switch classifyFailure(diagnostic) {
	case failureTerminal:
		// Retrying cannot help. Say what a human has to do about it.
		m.failRun(t.convID, "The agent could not run: "+terminalReason(diagnostic)+
			".\n\nThis will keep failing until it is resolved — retrying will not help.")

	case failureTransient:
		if t.attempt+1 < maxAttempts {
			delay := time.Duration(t.attempt+1) * 4 * time.Second
			m.storeAndBroadcast(t.convID, "system", jsonContent(fmt.Sprintf(
				"Claude's API is busy. Retrying in %s (attempt %d of %d).",
				delay, t.attempt+2, maxAttempts)))
			m.requeueAfter(t, delay)
			return
		}
		m.failRun(t.convID, fmt.Sprintf(
			"Claude's API stayed busy after %d attempts. Send a new message to try again.", maxAttempts))

	default:
		msg := fmt.Sprintf("The agent exited unexpectedly (%v).", waitErr)
		if stderrTail != "" {
			msg += "\n\n```\n" + stderrTail + "\n```"
		}
		m.failRun(t.convID, msg)
	}
}

// requeueAfter puts a transiently-failed turn back on the queue. The
// conversation stays in "queued" so the chat keeps streaming across the gap.
func (m *Manager) requeueAfter(t turn, delay time.Duration) {
	t.attempt++
	m.setStatus(t.convID, "queued")
	m.broadcast()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		select {
		case <-time.After(delay):
		case <-m.stopCh:
			return
		}
		m.mu.Lock()
		m.queue = append(m.queue, t)
		m.mu.Unlock()
		m.poke()
	}()
}

// failRun records why a turn failed and then marks it failed.
//
// The order matters and is not interchangeable. A terminal status is what tells
// every consumer to stop reading: the SSE handler emits the status event and
// closes the stream, and the chat UI stops listening. Flipping the status first
// leaves a window in which a client can observe "error" while the explanation is
// still unwritten — and then it is never delivered, so the user sees a failure
// with no reason. Writing the message first makes the invariant simple: if you
// can see a terminal status, the explanation is already there.
func (m *Manager) failRun(convID, message string) {
	m.storeAndBroadcast(convID, "system", jsonContent(message))
	m.setStatus(convID, "error")
}

func (m *Manager) setStatus(convID, status string) {
	if err := m.db.SetConversationStatus(convID, status); err != nil {
		log.Printf("store: setting status %q for %s: %v", status, convID, err)
	}
}

// tailWriter accepts everything written to it but retains only the last max
// bytes. Accepting everything is the point: a writer that blocked or errored
// would stall the child once its stderr buffer filled.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

func jsonContent(s string) string {
	data, _ := json.Marshal(map[string]string{"content": s})
	return string(data)
}

// ---------------------------------------------------------------------------
// Orphan recovery
// ---------------------------------------------------------------------------

// reapOrphans reconciles the database with reality at startup. Rows left in
// "running" belong to a previous daemon; if their process outlived us it is
// still holding the worktree and can still commit and deploy, so kill it rather
// than just relabelling the row.
// ReapOrphans reconciles the database with reality at startup.
func (m *Manager) ReapOrphans() (int, error) {
	rows, err := m.db.UnfinishedConversations()
	if err != nil {
		return 0, err
	}
	for _, c := range rows {
		if c.PID > 0 && processAlive(c.PID) {
			log.Printf("agent: killing orphaned agent for %s (pid %d)", c.ID, c.PID)
			syscall.Kill(-c.PID, syscall.SIGTERM)
		}
		m.setStatus(c.ID, "interrupted")
		if err := m.db.SetConversationPID(c.ID, 0); err != nil {
			log.Printf("store: clearing pid for %s: %v", c.ID, err)
		}
		m.storeAndBroadcast(c.ID, "system", jsonContent(
			"The agent was interrupted: slot-machine restarted while it was working. "+
				"Send a new message to continue."))
	}
	return len(rows), nil
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// ---------------------------------------------------------------------------
// Stream parsing
// ---------------------------------------------------------------------------

// processLine converts one stream-json line into stored events. It returns any
// assistant text seen, which feeds failure classification.
func (m *Manager) processLine(convID, line string) string {
	var raw map[string]any
	if json.Unmarshal([]byte(line), &raw) != nil {
		return ""
	}

	evtType, _ := raw["type"].(string)

	switch evtType {
	case "system":
		if sub, _ := raw["subtype"].(string); sub == "init" {
			if sid, ok := raw["session_id"].(string); ok {
				if err := m.db.UpdateSessionID(convID, sid); err != nil {
					log.Printf("store: recording session id for %s: %v", convID, err)
				}
			}
		}
		m.storeAndBroadcast(convID, "system", line)

	case "assistant":
		blocks := messageBlocks(raw)

		for _, block := range blocks {
			if bt, _ := block["type"].(string); bt == "tool_use" {
				toolName, _ := block["name"].(string)
				toolID, _ := block["id"].(string)
				data, _ := json.Marshal(map[string]string{"tool": toolName, "id": toolID})
				m.storeAndBroadcast(convID, "tool_use", string(data))
			}
		}

		var text string
		for _, block := range blocks {
			if bt, _ := block["type"].(string); bt == "text" {
				if t, _ := block["text"].(string); t != "" {
					text += t
				}
			}
		}

		if match := titlePattern.FindStringSubmatch(text); match != nil {
			if err := m.db.UpdateTitle(convID, strings.TrimSpace(match[1])); err != nil {
				log.Printf("store: updating title for %s: %v", convID, err)
			}
			text = strings.TrimSpace(titlePattern.ReplaceAllString(text, ""))
		}

		if text != "" {
			m.storeAndBroadcast(convID, "assistant", jsonContent(text))
		}
		return text

	case "user":
		for _, block := range messageBlocks(raw) {
			if bt, _ := block["type"].(string); bt == "tool_result" {
				toolID, _ := block["tool_use_id"].(string)
				content, _ := block["content"].(string)
				data, _ := json.Marshal(map[string]string{"id": toolID, "output": content})
				m.storeAndBroadcast(convID, "tool_result", string(data))
			}
		}

	case "result":
		var inputTok, outputTok, cacheRead, cacheWrite float64
		if usage, ok := raw["usage"].(map[string]any); ok {
			inputTok, _ = usage["input_tokens"].(float64)
			outputTok, _ = usage["output_tokens"].(float64)
			cacheRead, _ = usage["cache_read_input_tokens"].(float64)
			cacheWrite, _ = usage["cache_creation_input_tokens"].(float64)
		}
		if err := m.db.AddUsage(convID, int(inputTok), int(outputTok), int(cacheRead), int(cacheWrite)); err != nil {
			log.Printf("store: recording usage for %s: %v", convID, err)
		}

		resultText, _ := raw["result"].(string)
		if resultText != "" {
			if match := titlePattern.FindStringSubmatch(resultText); match != nil {
				if err := m.db.UpdateTitle(convID, strings.TrimSpace(match[1])); err != nil {
					log.Printf("store: updating title for %s: %v", convID, err)
				}
			}
		}

		m.storeAndBroadcast(convID, "done", line)
		return resultText
	}
	return ""
}

func messageBlocks(raw map[string]any) []map[string]any {
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return nil
	}
	list, _ := msg["content"].([]any)
	blocks := make([]map[string]any, 0, len(list))
	for _, b := range list {
		if block, ok := b.(map[string]any); ok {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func (m *Manager) storeAndBroadcast(convID, msgType, content string) {
	if _, err := m.db.AddMessage(convID, msgType, content); err != nil {
		// This is the failure the store's pragma fix exists to prevent. If it
		// happens anyway the event is lost, so it must not be lost silently.
		log.Printf("store: dropping %s event for %s: %v", msgType, convID, err)
	}
	m.broadcast()
}
