package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// agentWork describes one turn to run. The manager owns argv construction so
// that session resolution (below) can change it without the caller's help.
type agentWork struct {
	convID       string
	prompt       string
	sessionID    string
	bin          string
	dir          string
	env          []string
	allowedTools []string
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

type agentManager struct {
	store *agentStore

	mu      sync.Mutex
	queue   []agentWork
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

func newAgentManager(store *agentStore) *agentManager {
	m := &agentManager{
		store:  store,
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

func (m *agentManager) events() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notify
}

func (m *agentManager) broadcast() {
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
func (m *agentManager) enqueue(work agentWork) error {
	m.mu.Lock()
	if m.current != nil && m.current.convID == work.convID {
		m.mu.Unlock()
		return fmt.Errorf("agent already running for this conversation")
	}
	for _, q := range m.queue {
		if q.convID == work.convID {
			m.mu.Unlock()
			return fmt.Errorf("a message is already queued for this conversation")
		}
	}
	m.queue = append(m.queue, work)
	m.mu.Unlock()

	if err := m.store.setConversationStatus(work.convID, "queued"); err != nil {
		logf("store: setting queued status for %s: %v", work.convID, err)
	}
	m.broadcast()
	m.poke()
	return nil
}

func (m *agentManager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *agentManager) loop() {
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
func (m *agentManager) drain() {
	m.mu.Lock()
	if m.current != nil || len(m.queue) == 0 {
		m.mu.Unlock()
		return
	}
	work := m.queue[0]
	m.queue = m.queue[1:]

	ctx, cancel := context.WithTimeout(context.Background(), work.timeout)
	ra := &runningAgent{convID: work.convID, cancel: cancel, done: make(chan struct{})}
	m.current = ra
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.runAgent(ctx, work, ra)

		m.mu.Lock()
		m.current = nil
		m.mu.Unlock()
		close(ra.done)
		m.broadcast()
		m.poke() // the slot is free — start whatever is next
	}()
}

func (m *agentManager) stop() {
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
func (m *agentManager) activeConv() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return ""
	}
	return m.current.convID
}

// isPending reports whether a conversation is running or waiting in the queue,
// which is what tells an SSE handler to keep the stream open.
func (m *agentManager) isPending(convID string) bool {
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

func (m *agentManager) cancel(convID string) error {
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
func (m *agentManager) resolveResume(work agentWork) bool {
	if work.sessionID == "" {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if _, err := os.Stat(sessionFilePath(home, work.dir, work.sessionID)); err == nil {
		return true
	}

	logf("agent: resume target %s missing on disk, starting fresh (%s)", work.sessionID, work.convID)
	if err := m.store.clearSessionID(work.convID); err != nil {
		logf("store: clearing session id for %s: %v", work.convID, err)
	}
	m.storeAndBroadcast(work.convID, "system", jsonContent(
		"Previous session transcript is gone; starting a fresh session. Earlier context is not available to the agent."))
	return false
}

func buildAgentArgs(work agentWork, resume bool) []string {
	tools := work.allowedTools
	if len(tools) == 0 {
		tools = defaultAllowedTools
	}

	args := []string{"--output-format", "stream-json", "--verbose"}

	// Always explicit. Left unset, every run silently inherits whatever the
	// server user's ~/.claude/settings.json happens to say, which is not
	// something a deploy tool should ride on.
	if work.model != "" {
		args = append(args, "--model", work.model)
	}

	args = append(args, "--allowed-tools", strings.Join(tools, ","))

	// Append rather than replace: --system-prompt discards the CLI's own tool
	// guidance, which is not ours to throw away. We are adding context, not
	// substituting for it.
	args = append(args, "--append-system-prompt", work.systemPrompt)

	if resume {
		args = append(args, "--resume", work.sessionID)
	}

	// "--" so a message beginning with "-" is not parsed as a flag.
	args = append(args, "-p", "--", work.prompt)
	return args
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

func (m *agentManager) runAgent(ctx context.Context, work agentWork, ra *runningAgent) {
	resume := m.resolveResume(work)
	args := buildAgentArgs(work, resume)

	cmd := exec.CommandContext(ctx, work.bin, args...)
	cmd.Dir = work.dir
	if work.env != nil {
		cmd.Env = work.env
	}
	// The CLI spawns children (shells, tools); signal the whole group so a
	// cancel does not leave orphans holding the worktree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.failRun(work.convID, fmt.Sprintf("Could not start the agent: %v", err))
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
		m.failRun(work.convID, fmt.Sprintf("Could not start the agent: %v", err))
		return
	}

	m.setStatus(work.convID, "running")
	if err := m.store.setConversationPID(work.convID, cmd.Process.Pid); err != nil {
		logf("store: recording pid for %s: %v", work.convID, err)
	}
	m.broadcast()

	var lastText string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		if text := m.processLine(work.convID, scanner.Text()); text != "" {
			lastText = text
		}
	}

	waitErr := cmd.Wait()
	stderrTail := stderr.String()

	if err := m.store.setConversationPID(work.convID, 0); err != nil {
		logf("store: clearing pid for %s: %v", work.convID, err)
	}

	if waitErr == nil {
		m.setStatus(work.convID, "idle")
		return
	}

	// Timeout is not an API failure; report it as itself.
	if ctx.Err() == context.DeadlineExceeded {
		m.failRun(work.convID, fmt.Sprintf(
			"The agent ran longer than %s and was stopped. Send a new message to continue.",
			work.timeout))
		return
	}
	if ctx.Err() == context.Canceled {
		m.setStatus(work.convID, "cancelled")
		m.storeAndBroadcast(work.convID, "system", jsonContent("Cancelled."))
		return
	}

	diagnostic := lastText + "\n" + stderrTail
	switch classifyFailure(diagnostic) {
	case failureTerminal:
		// Retrying cannot help. Say what a human has to do about it.
		m.failRun(work.convID, "The agent could not run: "+terminalReason(diagnostic)+
			".\n\nThis will keep failing until it is resolved — retrying will not help.")

	case failureTransient:
		if work.attempt+1 < maxAttempts {
			delay := time.Duration(work.attempt+1) * 4 * time.Second
			m.storeAndBroadcast(work.convID, "system", jsonContent(fmt.Sprintf(
				"Claude's API is busy. Retrying in %s (attempt %d of %d).",
				delay, work.attempt+2, maxAttempts)))
			m.requeueAfter(work, delay)
			return
		}
		m.failRun(work.convID, fmt.Sprintf(
			"Claude's API stayed busy after %d attempts. Send a new message to try again.", maxAttempts))

	default:
		msg := fmt.Sprintf("The agent exited unexpectedly (%v).", waitErr)
		if stderrTail != "" {
			msg += "\n\n```\n" + stderrTail + "\n```"
		}
		m.failRun(work.convID, msg)
	}
}

// requeueAfter puts a transiently-failed turn back on the queue. The
// conversation stays in "queued" so the chat keeps streaming across the gap.
func (m *agentManager) requeueAfter(work agentWork, delay time.Duration) {
	work.attempt++
	m.setStatus(work.convID, "queued")
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
		m.queue = append(m.queue, work)
		m.mu.Unlock()
		m.poke()
	}()
}

func (m *agentManager) failRun(convID, message string) {
	m.setStatus(convID, "error")
	m.storeAndBroadcast(convID, "system", jsonContent(message))
}

func (m *agentManager) setStatus(convID, status string) {
	if err := m.store.setConversationStatus(convID, status); err != nil {
		logf("store: setting status %q for %s: %v", status, convID, err)
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
func (m *agentManager) reapOrphans() (int, error) {
	rows, err := m.store.unfinishedConversations()
	if err != nil {
		return 0, err
	}
	for _, c := range rows {
		if c.PID > 0 && processAlive(c.PID) {
			logf("agent: killing orphaned agent for %s (pid %d)", c.ID, c.PID)
			syscall.Kill(-c.PID, syscall.SIGTERM)
		}
		m.setStatus(c.ID, "interrupted")
		if err := m.store.setConversationPID(c.ID, 0); err != nil {
			logf("store: clearing pid for %s: %v", c.ID, err)
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
func (m *agentManager) processLine(convID, line string) string {
	var raw map[string]any
	if json.Unmarshal([]byte(line), &raw) != nil {
		return ""
	}

	evtType, _ := raw["type"].(string)

	switch evtType {
	case "system":
		if sub, _ := raw["subtype"].(string); sub == "init" {
			if sid, ok := raw["session_id"].(string); ok {
				if err := m.store.updateSessionID(convID, sid); err != nil {
					logf("store: recording session id for %s: %v", convID, err)
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
			if err := m.store.updateTitle(convID, strings.TrimSpace(match[1])); err != nil {
				logf("store: updating title for %s: %v", convID, err)
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
		if err := m.store.addUsage(convID, int(inputTok), int(outputTok), int(cacheRead), int(cacheWrite)); err != nil {
			logf("store: recording usage for %s: %v", convID, err)
		}

		resultText, _ := raw["result"].(string)
		if resultText != "" {
			if match := titlePattern.FindStringSubmatch(resultText); match != nil {
				if err := m.store.updateTitle(convID, strings.TrimSpace(match[1])); err != nil {
					logf("store: updating title for %s: %v", convID, err)
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

func (m *agentManager) storeAndBroadcast(convID, msgType, content string) {
	if _, err := m.store.addMessage(convID, msgType, content); err != nil {
		// This is the failure the store's pragma fix exists to prevent. If it
		// happens anyway the event is lost, so it must not be lost silently.
		logf("store: dropping %s event for %s: %v", msgType, convID, err)
	}
	m.broadcast()
}
