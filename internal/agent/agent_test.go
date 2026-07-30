package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slot-machine/internal/agent/store"
)

func TestExtractUser(t *testing.T) {
	t.Parallel()
	secret := "deadbeef1234"

	t.Run("hmac valid", func(t *testing.T) {
		a := &Service{authMode: "hmac", authSecret: secret}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte("alice"))
		sig := hex.EncodeToString(mac.Sum(nil))

		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-SlotMachine-User", "alice:"+sig)
		if got := a.extractUser(r); got != "alice" {
			t.Fatalf("got %q, want alice", got)
		}
	})

	t.Run("hmac invalid sig", func(t *testing.T) {
		a := &Service{authMode: "hmac", authSecret: secret}
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-SlotMachine-User", "alice:badsig")
		if got := a.extractUser(r); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("hmac missing header", func(t *testing.T) {
		a := &Service{authMode: "hmac", authSecret: secret}
		r := httptest.NewRequest("GET", "/", nil)
		if got := a.extractUser(r); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("trusted", func(t *testing.T) {
		a := &Service{authMode: "trusted"}
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-SlotMachine-User", "bob")
		if got := a.extractUser(r); got != "bob" {
			t.Fatalf("got %q, want bob", got)
		}
	})

	t.Run("none", func(t *testing.T) {
		a := &Service{authMode: "none"}
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-SlotMachine-User", "bob")
		if got := a.extractUser(r); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func TestTitlePattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input     string
		wantTitle string
		wantClean string
	}{
		{"[[TITLE: Hello World]]\nSome text", "Hello World", "Some text"},
		{"Some text [[TITLE: Updated]] more text", "Updated", "Some text  more text"},
		{"No title here", "", "No title here"},
		{"[[TITLE: Just a title]]", "Just a title", ""},
	}

	for _, tt := range tests {
		m := titlePattern.FindStringSubmatch(tt.input)
		if tt.wantTitle == "" {
			if m != nil {
				t.Errorf("input=%q: expected no match, got %v", tt.input, m)
			}
			continue
		}
		if m == nil {
			t.Errorf("input=%q: expected match", tt.input)
			continue
		}
		if got := strings.TrimSpace(m[1]); got != tt.wantTitle {
			t.Errorf("input=%q: title=%q, want %q", tt.input, got, tt.wantTitle)
		}
		clean := strings.TrimSpace(titlePattern.ReplaceAllString(tt.input, ""))
		if clean != tt.wantClean {
			t.Errorf("input=%q: clean=%q, want %q", tt.input, clean, tt.wantClean)
		}
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	t.Parallel()

	t.Run("no instruction files", func(t *testing.T) {
		a := &Service{workDir: t.TempDir(), machineBranch: "machine", humanBranch: "main"}
		prompt := a.buildSystemPrompt()
		if !strings.Contains(prompt, "slot-machine") {
			t.Fatal("missing slot-machine mention")
		}
		if !strings.Contains(prompt, "[[TITLE:") {
			t.Fatal("missing titling instruction")
		}
	})

	t.Run("AGENTS.slot-machine.md takes priority", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "AGENTS.slot-machine.md"), []byte("Slot-specific.\n"), 0644)
		os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Generic agent.\n"), 0644)
		os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Project context.\n"), 0644)
		a := &Service{workDir: dir, machineBranch: "machine", humanBranch: "main"}
		prompt := a.buildSystemPrompt()
		if !strings.Contains(prompt, "Slot-specific.") {
			t.Fatal("expected AGENTS.slot-machine.md content")
		}
		if strings.Contains(prompt, "Generic agent.") {
			t.Fatal("should not include AGENTS.md when AGENTS.slot-machine.md exists")
		}
	})

	t.Run("AGENTS.md used when no slot-machine variant", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Generic agent.\n"), 0644)
		os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Project context.\n"), 0644)
		a := &Service{workDir: dir, machineBranch: "machine", humanBranch: "main"}
		prompt := a.buildSystemPrompt()
		if !strings.Contains(prompt, "Generic agent.") {
			t.Fatal("expected AGENTS.md content")
		}
	})

	t.Run("CLAUDE.md as last resort", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Project context.\n"), 0644)
		a := &Service{workDir: dir, machineBranch: "machine", humanBranch: "main"}
		prompt := a.buildSystemPrompt()
		if !strings.Contains(prompt, "Project context.") {
			t.Fatal("expected CLAUDE.md content")
		}
	})
}

// The prompt is one command-line argument, and Linux caps a single argument at
// 128 KiB. Exceeding it is E2BIG — no agent at all — so the instruction file is
// bounded rather than trusted.
func TestBuildSystemPromptCapsInstructions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	huge := strings.Repeat("x", maxInstructionBytes*2)
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(huge), 0644); err != nil {
		t.Fatal(err)
	}

	a := &Service{workDir: dir, machineBranch: "machine", humanBranch: "main"}
	prompt := a.buildSystemPrompt()

	if len(prompt) > maxInstructionBytes+16*1024 {
		t.Fatalf("prompt is %d bytes; the instruction file was not capped", len(prompt))
	}
	// The base prompt must survive the truncation.
	if !strings.Contains(prompt, "[[TITLE:") {
		t.Fatal("truncation dropped slot-machine's own instructions")
	}
}

func TestChatConfigEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("special characters in title", func(t *testing.T) {
		a := &Service{
			authMode:   "none",
			chatTitle:  "Lou's App",
			chatAccent: "#ff0000",
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/chat/config", nil)
		a.handleChatConfig(w, r)

		body := w.Body.String()
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		// The title with an apostrophe must be valid JSON (no broken quotes).
		if !strings.Contains(body, `Lou's App`) {
			t.Fatalf("title not in response: %s", body)
		}
		if !strings.Contains(body, `"chatAccent":"#ff0000"`) {
			t.Fatalf("accent not in response: %s", body)
		}
	})

	t.Run("default title", func(t *testing.T) {
		a := &Service{authMode: "hmac", authSecret: "abc123"}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/chat/config", nil)
		a.handleChatConfig(w, r)

		body := w.Body.String()
		if !strings.Contains(body, `"chatTitle":"slot-machine"`) {
			t.Fatalf("expected default title, got: %s", body)
		}
		if !strings.Contains(body, `"authMode":"hmac"`) {
			t.Fatalf("expected authMode hmac, got: %s", body)
		}
		if !strings.Contains(body, `"authSecret":"abc123"`) {
			t.Fatalf("expected authSecret, got: %s", body)
		}
	})
}

func TestChatServesStaticHTML(t *testing.T) {
	t.Parallel()
	a := &Service{authMode: "none"}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/chat", nil)
	a.handleChat(w, r)

	body := w.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatal("missing DOCTYPE")
	}
	// Must NOT contain Go template syntax.
	if strings.Contains(body, "{{") {
		t.Fatal("chat.html still contains template syntax")
	}
}

func TestSendMessageStoresAndEnqueues(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mgr := NewManager(st)
	defer mgr.Stop()

	tmpDir := t.TempDir()
	a := &Service{
		db:         st,
		manager:    mgr,
		agentBin:   "true", // succeeds, does nothing
		workDir:    tmpDir,
		configPath: filepath.Join(tmpDir, "slot-machine.json"),
		dataDir:    tmpDir,
		authMode:   "none",
		timeout:    30 * time.Second,
	}

	convID := "conv-store-test"
	st.CreateConversation(convID, "test")

	body := strings.NewReader(`{"content":"hello"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/agent/conversations/"+convID+"/messages", body)
	a.handleSendMessage(w, r, convID)

	// Should be 200 (message stored + agent enqueued).
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Message stored in DB.
	msgs, _ := st.GetMessages(convID, 0)
	found := false
	for _, m := range msgs {
		if m.Type == "user" && m.Content == "hello" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected user message stored, got %+v", msgs)
	}
}

func TestReapOrphans(t *testing.T) {
	t.Parallel()
	s, _ := store.Open(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()

	s.CreateConversation("c1", "u")
	s.SetConversationStatus("c1", "running")
	s.CreateConversation("c2", "u")
	s.SetConversationStatus("c2", "running")
	s.CreateConversation("c3", "u") // idle, should not be touched

	mgr := NewManager(s)
	defer mgr.Stop()

	n, err := mgr.ReapOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 recovered, got %d", n)
	}

	c1, _ := s.GetConversation("c1")
	if c1.Status != "interrupted" {
		t.Fatalf("expected 'interrupted', got %q", c1.Status)
	}

	msgs, _ := s.GetMessages("c1", 0)
	found := false
	for _, m := range msgs {
		if m.Type == "system" && strings.Contains(m.Content, "interrupted") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected system message about interruption")
	}
}

// mockAgentBin writes a tiny shell script that stands in for the Claude CLI.
// The manager builds argv itself now, so a mock must tolerate arbitrary flags —
// which a bare "sleep" or "echo" with fixed args cannot.
func mockAgentBin(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mockagent")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---------------------------------------------------------------------------
// Store durability
// ---------------------------------------------------------------------------

// The regression this guards: the DSN previously used another driver's
// parameter syntax, which modernc.org/sqlite silently ignored, leaving the
// store in rollback-journal mode with no busy timeout.

func TestAgentManagerStartStop(t *testing.T) {
	t.Parallel()
	s, _ := store.Open(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()

	mgr := NewManager(s)
	mgr.Stop()
}

func TestAgentManagerRunAgent(t *testing.T) {
	t.Parallel()
	s, _ := store.Open(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()
	s.CreateConversation("c1", "user1")

	mgr := NewManager(s)
	defer mgr.Stop()

	// "echo" as a mock agent: prints its argv, emits no stream-json, exits 0.
	err := mgr.enqueue(turn{
		convID:  "c1",
		prompt:  "hello",
		bin:     "echo",
		dir:     t.TempDir(),
		timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for agent to finish.
	deadline := time.After(5 * time.Second)
	for {
		c, _ := s.GetConversation("c1")
		if c.Status == "idle" || c.Status == "error" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("agent did not finish in time")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestAgentManagerRejectsConcurrent(t *testing.T) {
	t.Parallel()
	s, _ := store.Open(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()
	s.CreateConversation("c1", "user1")

	mgr := NewManager(s)
	defer mgr.Stop()

	err := mgr.enqueue(turn{
		convID: "c1", bin: mockAgentBin(t, "sleep 10"), dir: t.TempDir(),
		timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A second message for the *same* conversation is a conflict.
	err = mgr.enqueue(turn{
		convID: "c1", bin: "echo", dir: t.TempDir(), timeout: 30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error for a second message on the same conversation, got nil")
	}
}

func TestAgentManagerCancel(t *testing.T) {
	t.Parallel()
	s, _ := store.Open(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()
	s.CreateConversation("c1", "user1")

	mgr := NewManager(s)
	defer mgr.Stop()

	mgr.enqueue(turn{
		convID: "c1", bin: mockAgentBin(t, "sleep 60"), dir: t.TempDir(),
		timeout: 30 * time.Second,
	})

	// Give the agent a moment to start.
	time.Sleep(100 * time.Millisecond)

	err := mgr.cancel("c1")
	if err != nil {
		t.Fatal(err)
	}

	// Wait for status to settle.
	deadline := time.After(5 * time.Second)
	for {
		c, _ := s.GetConversation("c1")
		if c.Status != "running" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("cancel did not finish in time")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Two conversations share one worktree, so only one agent may run at a time.
// The second must wait rather than being rejected or running concurrently.
func TestAgentManagerQueuesAcrossConversations(t *testing.T) {
	t.Parallel()
	s, _ := store.Open(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()
	s.CreateConversation("c1", "u")
	s.CreateConversation("c2", "u")

	mgr := NewManager(s)
	defer mgr.Stop()

	marker := filepath.Join(t.TempDir(), "concurrent")
	// Each run appends on entry and removes on exit; if two ever overlap the
	// file will hold two lines at once.
	script := "echo x >> " + marker + "; sleep 1; echo done"

	if err := mgr.enqueue(turn{
		convID: "c1", bin: mockAgentBin(t, script), dir: t.TempDir(), timeout: 30 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	// A different conversation is a queue, not a conflict.
	if err := mgr.enqueue(turn{
		convID: "c2", bin: mockAgentBin(t, script), dir: t.TempDir(), timeout: 30 * time.Second,
	}); err != nil {
		t.Fatalf("a second conversation should queue, not error: %v", err)
	}

	// Only one may hold the slot at any moment.
	deadline := time.After(20 * time.Second)
	sawC2 := false
	for {
		active := mgr.activeConv()
		if active == "c2" {
			sawC2 = true
		}
		c1, _ := s.GetConversation("c1")
		c2, _ := s.GetConversation("c2")
		if c1.Status == "idle" && c2.Status == "idle" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("queue did not drain (c1=%s c2=%s)", c1.Status, c2.Status)
		case <-time.After(25 * time.Millisecond):
		}
	}
	if !sawC2 {
		t.Fatal("c2 never became the active conversation; the queue did not drain to it")
	}
}

func TestAgentManagerTimesOutStuckRun(t *testing.T) {
	t.Parallel()
	s, _ := store.Open(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()
	s.CreateConversation("c1", "u")

	mgr := NewManager(s)
	defer mgr.Stop()

	mgr.enqueue(turn{
		convID: "c1", bin: mockAgentBin(t, "sleep 120"), dir: t.TempDir(),
		timeout: 500 * time.Millisecond,
	})

	deadline := time.After(20 * time.Second)
	for {
		c, _ := s.GetConversation("c1")
		if c.Status == "error" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("a stuck agent was never timed out (status %q)", c.Status)
		case <-time.After(25 * time.Millisecond):
		}
	}

	msgs, _ := s.GetMessages("c1", 0)
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Content, "ran longer than") {
			found = true
		}
	}
	if !found {
		t.Fatal("a timed-out run must say so in the conversation")
	}
}

// A terminal API failure must be explained, not reported as an exit code —
// and must not be retried, because retrying cannot help.

// A terminal API failure must be explained, not reported as an exit code —
// and must not be retried, because retrying cannot help.
func TestAgentManagerReportsTerminalFailure(t *testing.T) {
	t.Parallel()
	s, _ := store.Open(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()
	s.CreateConversation("c1", "u")

	mgr := NewManager(s)
	defer mgr.Stop()

	bin := mockAgentBin(t, `echo "API Error: 401 Invalid authentication credentials" >&2; exit 1`)
	mgr.enqueue(turn{convID: "c1", bin: bin, dir: t.TempDir(), timeout: 30 * time.Second})

	deadline := time.After(20 * time.Second)
	for {
		c, _ := s.GetConversation("c1")
		if c.Status == "error" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("run never settled (status %q)", c.Status)
		case <-time.After(25 * time.Millisecond):
		}
	}

	msgs, _ := s.GetMessages("c1", 0)
	joined := ""
	for _, m := range msgs {
		joined += m.Content
	}
	if !strings.Contains(joined, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("a 401 must be explained, got: %s", joined)
	}
	if strings.Contains(joined, "Retrying") {
		t.Fatal("a terminal failure must not be retried")
	}
}

// stderr is where the CLI explains itself. It used to go to /dev/null, which is
// why every failure surfaced as a bare exit code.

// stderr is where the CLI explains itself. It used to go to /dev/null, which is
// why every failure surfaced as a bare exit code.
func TestAgentManagerSurfacesStderr(t *testing.T) {
	t.Parallel()
	s, _ := store.Open(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()
	s.CreateConversation("c1", "u")

	mgr := NewManager(s)
	defer mgr.Stop()

	bin := mockAgentBin(t, `echo "something specific went wrong" >&2; exit 3`)
	mgr.enqueue(turn{convID: "c1", bin: bin, dir: t.TempDir(), timeout: 30 * time.Second})

	deadline := time.After(20 * time.Second)
	for {
		c, _ := s.GetConversation("c1")
		if c.Status == "error" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("run never settled (status %q)", c.Status)
		case <-time.After(25 * time.Millisecond):
		}
	}

	msgs, _ := s.GetMessages("c1", 0)
	joined := ""
	for _, m := range msgs {
		joined += m.Content
	}
	if !strings.Contains(joined, "something specific went wrong") {
		t.Fatalf("stderr must reach the conversation, got: %s", joined)
	}
}

// ---------------------------------------------------------------------------
// Tool policy
// ---------------------------------------------------------------------------

func TestClassifyFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		text string
		want failureKind
	}{
		{"API Error: 429 Too Many Requests", failureTransient},
		{"API Error: 503", failureTransient},
		{`{"type":"overloaded_error"}`, failureTransient},
		{"API Error: 401 Invalid authentication credentials", failureTerminal},
		{"You have reached your monthly spend limit", failureTerminal},
		{"You have hit your session limit", failureTerminal},
		{"command not found: bun", failureUnknown},
		{"", failureUnknown},
		// Terminal outranks transient when both appear in one buffer: retrying a
		// capped account turns one failure into three.
		{"API Error: 429 ... later: monthly spend limit reached", failureTerminal},
	}

	for _, tc := range cases {
		if got := classifyFailure(tc.text); got != tc.want {
			t.Errorf("classifyFailure(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestTerminalReasonIsActionable(t *testing.T) {
	t.Parallel()
	if r := terminalReason("monthly spend limit"); !strings.Contains(r, "spend limit") {
		t.Fatalf("unhelpful reason: %q", r)
	}
	if r := terminalReason("API Error: 401"); !strings.Contains(r, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("a 401 should point at the token, got: %q", r)
	}
}

// ---------------------------------------------------------------------------
// Agent argv
// ---------------------------------------------------------------------------

func TestBuildAgentArgs(t *testing.T) {
	t.Parallel()

	args := buildAgentArgs(turn{
		prompt:           "-rf looks like a flag",
		sessionID:        "sess-1",
		model:            "claude-opus-5",
		systemPromptPath: "/data/agent-system-prompt.md",
		allowedTools:     []string{"Bash", "Read"},
	}, true)

	joined := strings.Join(args, " ")

	// Append, don't replace: --system-prompt would discard the CLI's own tool
	// guidance along with everything else it puts in there.
	if strings.Contains(joined, "--system-prompt ") {
		t.Fatal("must use --append-system-prompt-file, not --system-prompt")
	}
	// By file, not inline: inline puts the whole prompt in argv, where ps can
	// read it and the OS caps its length.
	if !strings.Contains(joined, "--append-system-prompt-file /data/agent-system-prompt.md") {
		t.Fatalf("expected the prompt to be passed by file, got: %s", joined)
	}
	if strings.Contains(joined, "--append-system-prompt ") {
		t.Fatal("the prompt must not also be passed inline")
	}
	if !strings.Contains(joined, "--model claude-opus-5") {
		t.Fatal("model must be explicit, or the run inherits the server user's settings")
	}
	if !strings.Contains(joined, "--resume sess-1") {
		t.Fatal("missing --resume")
	}

	// The prompt must be last and behind a bare "--", so a message starting
	// with "-" is not parsed as a flag.
	if args[len(args)-1] != "-rf looks like a flag" {
		t.Fatalf("prompt must be the final argument, got %q", args[len(args)-1])
	}
	if args[len(args)-2] != "--" {
		t.Fatalf("prompt must be preceded by --, got %q", args[len(args)-2])
	}
	if args[len(args)-3] != "-p" {
		t.Fatalf("expected -p before --, got %q", args[len(args)-3])
	}
}

func TestBuildAgentArgsNoResumeWithoutSession(t *testing.T) {
	t.Parallel()
	args := buildAgentArgs(turn{prompt: "hi"}, false)
	if strings.Contains(strings.Join(args, " "), "--resume") {
		t.Fatal("must not pass --resume when starting a fresh session")
	}
}

// ---------------------------------------------------------------------------
// Queue and drain
// ---------------------------------------------------------------------------

// Two conversations share one worktree, so only one agent may run at a time.
// The second must wait rather than being rejected or running concurrently.

// The policy path must reach the CLI, or none of it applies.
func TestBuildAgentArgsPassesSettings(t *testing.T) {
	t.Parallel()
	args := buildAgentArgs(turn{prompt: "hi", settingsPath: "/data/agent-settings.json"}, false)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--settings /data/agent-settings.json") {
		t.Fatalf("expected --settings to be passed, got: %s", joined)
	}
}

// ---------------------------------------------------------------------------
// Self-update integrity
// ---------------------------------------------------------------------------

func TestWriteAgentPolicy(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	workDir := t.TempDir()
	a := &Service{
		dataDir:        dataDir,
		workDir:        workDir,
		configPath:     filepath.Join(t.TempDir(), "slot-machine.json"),
		deniedCommands: []string{"terraform"},
	}

	if err := a.writeAgentPolicy(); err != nil {
		t.Fatal(err)
	}

	// The policy must not live in the agent's worktree. There it was an
	// untracked file that `git add -A` would commit into the app's repo, with
	// absolute server paths in it.
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "settings.json")); err == nil {
		t.Fatal("policy must not be written inside the agent worktree")
	}

	data, err := os.ReadFile(a.agentPolicyPath())
	if err != nil {
		t.Fatalf("policy file not written to the data directory: %v", err)
	}

	var parsed struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("policy is not valid JSON: %v", err)
	}

	joined := strings.Join(parsed.Permissions.Deny, "\n")
	for _, want := range []string{
		"Bash(sudo:*)",
		"Bash(rm -rf /:*)",
		"Bash(git push --force:*)",
		"Bash(slot-machine start:*)",
		"Bash(terraform:*)", // from config
		"Write(" + a.configPath + ")",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("policy missing %q\ngot:\n%s", want, joined)
		}
	}

	// ~/.ssh must be denied in both the expanded and the tilde form: matching is
	// a prefix match against the command as typed, and an agent types the tilde.
	if !strings.Contains(joined, "Read(~/.ssh/**)") {
		t.Error("policy must deny the tilde form of ~/.ssh, which is how it gets typed")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if !strings.Contains(joined, "Read("+filepath.Join(home, ".ssh")+"/**)") {
			t.Error("policy must also deny the expanded form of ~/.ssh")
		}
	}
}

// The policy is regenerated every turn, so an agent that deletes it only
// escapes for the remainder of that turn.
func TestAgentPolicyRegenerated(t *testing.T) {
	t.Parallel()

	a := &Service{
		dataDir:    t.TempDir(),
		workDir:    t.TempDir(),
		configPath: filepath.Join(t.TempDir(), "slot-machine.json"),
	}
	if err := a.writeAgentPolicy(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.agentPolicyPath()); err != nil {
		t.Fatal(err)
	}
	if err := a.writeAgentPolicy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.agentPolicyPath()); err != nil {
		t.Fatalf("policy was not regenerated: %v", err)
	}
}

// The policy path must reach the CLI, or none of it applies.

func TestResolveClaudeFromEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\necho hi"), 0755)

	t.Setenv("SLOT_MACHINE_AGENT_BIN", bin)
	got := ResolveClaude(dir)
	if got != bin {
		t.Fatalf("expected %s, got %s", bin, got)
	}
}

func TestResolveClaudeFromDataDir(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, ".local", "bin")
	os.MkdirAll(binDir, 0755)
	bin := filepath.Join(binDir, "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\necho hi"), 0755)

	t.Setenv("SLOT_MACHINE_AGENT_BIN", "")
	got := ResolveClaude(dir)
	if got != bin {
		t.Fatalf("expected %s, got %s", bin, got)
	}
}

func TestResolveClaudeNotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SLOT_MACHINE_AGENT_BIN", "")
	got := ResolveClaude(dir)
	// May find claude in PATH if installed, so just check it doesn't crash.
	_ = got
}

// mockAgentBin writes a tiny shell script that stands in for the Claude CLI.
// The manager builds argv itself now, so a mock must tolerate arbitrary flags —
// which a bare "sleep" or "echo" with fixed args cannot.
