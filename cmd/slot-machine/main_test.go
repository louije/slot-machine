package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShortHash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"abcdef1234567890", "abcdef12"},
		{"abcdef12", "abcdef12"},
		{"d4f80a3", "d4f80a3"}, // 7-char short hash (common git default)
		{"abc", "abc"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := shortHash(tt.in); got != tt.want {
			t.Errorf("shortHash(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	content := `# comment
FOO=bar
BAZ=qux

# another comment
EMPTY=
NOEQ
`
	os.WriteFile(path, []byte(content), 0644)

	env, err := loadEnvFile(path)
	if err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}

	want := []string{"FOO=bar", "BAZ=qux", "EMPTY="}
	if len(env) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(env), len(want), env)
	}
	for i, w := range want {
		if env[i] != w {
			t.Errorf("env[%d] = %q, want %q", i, env[i], w)
		}
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	t.Parallel()
	_, err := loadEnvFile("/nonexistent/.env")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestAtomicSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(dir, "live")

	// Create initial symlink.
	if err := atomicSymlink(link, "slot-a"); err != nil {
		t.Fatalf("atomicSymlink: %v", err)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "slot-a" {
		t.Fatalf("got %q, want slot-a", target)
	}

	// Overwrite atomically.
	if err := atomicSymlink(link, "slot-b"); err != nil {
		t.Fatalf("atomicSymlink overwrite: %v", err)
	}
	target, err = os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink after overwrite: %v", err)
	}
	if target != "slot-b" {
		t.Fatalf("got %q, want slot-b", target)
	}
}

func TestFindFreePort(t *testing.T) {
	t.Parallel()
	port, err := findFreePort()
	if err != nil {
		t.Fatalf("findFreePort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port %d out of range", port)
	}

	// Port should actually be available.
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d not available: %v", port, err)
	}
	l.Close()
}

func TestGitignoreContains(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")

	// Missing file.
	if gitignoreContains(path, ".slot-machine") {
		t.Fatal("expected false for missing file")
	}

	// File without entry.
	os.WriteFile(path, []byte("node_modules\n.env\n"), 0644)
	if gitignoreContains(path, ".slot-machine") {
		t.Fatal("expected false when entry absent")
	}

	// File with entry.
	os.WriteFile(path, []byte("node_modules\n.slot-machine\n.env\n"), 0644)
	if !gitignoreContains(path, ".slot-machine") {
		t.Fatal("expected true when entry present")
	}

	// Entry with surrounding whitespace.
	os.WriteFile(path, []byte("  .slot-machine  \n"), 0644)
	if !gitignoreContains(path, ".slot-machine") {
		t.Fatal("expected true with surrounding whitespace")
	}
}

func TestFileExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if fileExists(filepath.Join(dir, "nope")) {
		t.Fatal("expected false for nonexistent file")
	}
	path := filepath.Join(dir, "yes")
	os.WriteFile(path, []byte(""), 0644)
	if !fileExists(path) {
		t.Fatal("expected true for existing file")
	}
}

func TestReadStartScript(t *testing.T) {
	t.Parallel()

	t.Run("with start script", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "package.json"),
			[]byte(`{"scripts":{"start":"bun server/index.ts"}}`), 0644)
		got := readStartScript(dir, "bun")
		if got != "bun server/index.ts" {
			t.Fatalf("got %q, want bun server/index.ts", got)
		}
	})

	t.Run("with main field", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "package.json"),
			[]byte(`{"main":"server.js"}`), 0644)
		got := readStartScript(dir, "node")
		if got != "node server.js" {
			t.Fatalf("got %q, want node server.js", got)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{}`), 0644)
		got := readStartScript(dir, "node")
		if got != "node index.js" {
			t.Fatalf("got %q, want node index.js", got)
		}
	})

	t.Run("no package.json", func(t *testing.T) {
		dir := t.TempDir()
		got := readStartScript(dir, "bun")
		if got != "bun index.js" {
			t.Fatalf("got %q, want bun index.js", got)
		}
	})
}

func TestBuildEnvIncludesSlotMachine(t *testing.T) {
	t.Parallel()
	o := &orchestrator{cfg: config{}}
	env := o.buildEnv(3000, 3900)
	found := false
	for _, e := range env {
		if e == "SLOT_MACHINE=1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected SLOT_MACHINE=1 in env")
	}
}

func TestWriteJSON(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	writeJSON(w, 201, map[string]string{"ok": "yes"})
	if w.Code != 201 {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if body := w.Body.String(); body != "{\"ok\":\"yes\"}\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestDynamicProxyNoTarget(t *testing.T) {
	t.Parallel()
	p := newDynamicProxy("", nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	p.serveHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestDynamicProxyWithTarget(t *testing.T) {
	t.Parallel()

	// Start a test backend.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	// Extract port from backend URL.
	_, portStr, _ := net.SplitHostPort(backend.Listener.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	p := newDynamicProxy("", nil)
	p.port = port // set directly since addr="" means no listener management

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	p.serveHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestDynamicProxyLifecycle(t *testing.T) {
	t.Parallel()

	port, _ := findFreePort()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	p := newDynamicProxy(addr, nil)

	// No target — no listener.
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("expected connection refused with no target")
	}

	// Start a backend.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend"))
	}))
	defer backend.Close()
	_, bPortStr, _ := net.SplitHostPort(backend.Listener.Addr().String())
	var bPort int
	fmt.Sscanf(bPortStr, "%d", &bPort)

	// Set target — listener should start.
	p.setTarget(bPort)
	time.Sleep(50 * time.Millisecond) // let goroutine start

	resp, err := http.Get(fmt.Sprintf("http://%s/", addr))
	if err != nil {
		t.Fatalf("GET after setTarget: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Clear target — listener should stop.
	p.clearTarget()
	time.Sleep(50 * time.Millisecond)

	conn, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("expected connection refused after clearTarget")
	}
}

func TestOrchestratorServeHTTP(t *testing.T) {
	t.Parallel()

	o := &orchestrator{
		appProxy: newDynamicProxy("", nil),
		intProxy: newDynamicProxy("", nil),
	}

	t.Run("GET /", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		o.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("GET /status", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/status", nil)
		o.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})

	t.Run("404", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/nope", nil)
		o.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("expected 404, got %d", w.Code)
		}
	})

	t.Run("POST /deploy missing body", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/deploy", nil)
		o.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})
}

func TestStatusHandler(t *testing.T) {
	t.Parallel()

	now := time.Now()
	o := &orchestrator{
		appProxy: newDynamicProxy("", nil),
		intProxy: newDynamicProxy("", nil),
		liveSlot: &slot{
			name:   "slot-abc12345",
			commit: "abc1234567890",
			alive:  true,
		},
		prevSlot: &slot{
			name:   "slot-def12345",
			commit: "def1234567890",
		},
		lastDeploy: now,
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/status", nil)
	o.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	for _, want := range []string{"slot-abc12345", "abc1234567890", "slot-def12345", "def1234567890", "slot-staging"} {
		if !contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

func TestExtractUser(t *testing.T) {
	t.Parallel()
	secret := "deadbeef1234"

	t.Run("hmac valid", func(t *testing.T) {
		a := &agentService{authMode: "hmac", authSecret: secret}
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
		a := &agentService{authMode: "hmac", authSecret: secret}
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-SlotMachine-User", "alice:badsig")
		if got := a.extractUser(r); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("hmac missing header", func(t *testing.T) {
		a := &agentService{authMode: "hmac", authSecret: secret}
		r := httptest.NewRequest("GET", "/", nil)
		if got := a.extractUser(r); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("trusted", func(t *testing.T) {
		a := &agentService{authMode: "trusted"}
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-SlotMachine-User", "bob")
		if got := a.extractUser(r); got != "bob" {
			t.Fatalf("got %q, want bob", got)
		}
	})

	t.Run("none", func(t *testing.T) {
		a := &agentService{authMode: "none"}
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
		a := &agentService{stagingDir: t.TempDir()}
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
		a := &agentService{stagingDir: dir}
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
		a := &agentService{stagingDir: dir}
		prompt := a.buildSystemPrompt()
		if !strings.Contains(prompt, "Generic agent.") {
			t.Fatal("expected AGENTS.md content")
		}
	})

	t.Run("CLAUDE.md as last resort", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Project context.\n"), 0644)
		a := &agentService{stagingDir: dir}
		prompt := a.buildSystemPrompt()
		if !strings.Contains(prompt, "Project context.") {
			t.Fatal("expected CLAUDE.md content")
		}
	})
}

func TestChatConfigEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("special characters in title", func(t *testing.T) {
		a := &agentService{
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
		a := &agentService{authMode: "hmac", authSecret: "abc123"}
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
	a := &agentService{authMode: "none"}
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

func TestBuildEnvResolvesEnvFileRelativeToRepoDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=hunter2\n"), 0644)

	o := &orchestrator{
		cfg:     config{EnvFile: ".env"},
		repoDir: dir,
	}
	env := o.buildEnv(3000, 3900)
	found := false
	for _, e := range env {
		if e == "SECRET=hunter2" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected SECRET=hunter2 from .env resolved relative to repoDir")
	}
}

func TestSendMessageOnlyStoresDoesNotStartAgent(t *testing.T) {
	t.Parallel()
	store, err := openAgentStore(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}

	a := &agentService{
		store:    store,
		sessions: make(map[string]*agentSession),
		authMode: "none",
	}

	convID := "conv-store-test"
	store.createConversation(convID, "test")

	body := strings.NewReader(`{"content":"hello"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/agent/conversations/"+convID+"/messages", body)
	a.handleSendMessage(w, r, convID)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Message stored in DB.
	msgs, _ := store.getMessages(convID, 0)
	if len(msgs) != 1 || msgs[0].Type != "user" || msgs[0].Content != "hello" {
		t.Fatalf("expected user message stored, got %+v", msgs)
	}

	// No session created — agent not started.
	a.mu.Lock()
	_, running := a.sessions[convID]
	a.mu.Unlock()
	if running {
		t.Fatal("expected no session after POST /messages")
	}
}

func TestStreamRejectsIfAgentAlreadyRunning(t *testing.T) {
	t.Parallel()
	store, err := openAgentStore(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}

	a := &agentService{
		store:    store,
		sessions: make(map[string]*agentSession),
		authMode: "none",
	}

	convID := "conv-reject-test"
	store.createConversation(convID, "test")
	store.addMessage(convID, "user", "hello")

	// Simulate an active session.
	session := &agentSession{done: make(chan struct{})}
	a.mu.Lock()
	a.sessions[convID] = session
	a.mu.Unlock()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/agent/conversations/"+convID+"/stream", nil)
	a.handleStream(w, r, convID)

	if w.Code != 409 {
		t.Fatalf("expected 409 for concurrent stream, got %d", w.Code)
	}
}

func TestApplySharedDirs(t *testing.T) {
	t.Parallel()

	t.Run("symlinks slot dir to repo dir", func(t *testing.T) {
		repoDir := t.TempDir()
		slotDir := t.TempDir()

		// Repo has the canonical data with a file.
		os.MkdirAll(filepath.Join(repoDir, "data"), 0755)
		os.WriteFile(filepath.Join(repoDir, "data", "test.db"), []byte("content"), 0644)

		// Slot has a stale copy (from CoW clone).
		os.MkdirAll(filepath.Join(slotDir, "data"), 0755)
		os.WriteFile(filepath.Join(slotDir, "data", "stale.db"), []byte("stale"), 0644)

		o := &orchestrator{
			cfg:     config{SharedDirs: []string{"data"}},
			repoDir: repoDir,
		}
		o.applySharedDirs(slotDir)

		// Slot's data should now be a symlink.
		info, err := os.Lstat(filepath.Join(slotDir, "data"))
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("expected symlink")
		}

		// Slot should see the repo's file, not the stale copy.
		content, _ := os.ReadFile(filepath.Join(slotDir, "data", "test.db"))
		if string(content) != "content" {
			t.Fatal("expected repo file through symlink")
		}
		if _, err := os.Stat(filepath.Join(slotDir, "data", "stale.db")); err == nil {
			t.Fatal("stale file should not be visible")
		}
	})

	t.Run("seeds repo dir from slot checkout on first deploy", func(t *testing.T) {
		repoDir := t.TempDir()
		slotDir := t.TempDir()

		// Slot has data from the git checkout (first deploy).
		os.MkdirAll(filepath.Join(slotDir, "data"), 0755)
		os.WriteFile(filepath.Join(slotDir, "data", "seed.db"), []byte("seeded"), 0644)

		o := &orchestrator{
			cfg:     config{SharedDirs: []string{"data"}},
			repoDir: repoDir,
		}
		o.applySharedDirs(slotDir)

		// Repo's data dir should contain the seeded file.
		content, err := os.ReadFile(filepath.Join(repoDir, "data", "seed.db"))
		if err != nil || string(content) != "seeded" {
			t.Fatal("expected repo data dir to be seeded from slot checkout")
		}

		// Slot should symlink to it.
		info, _ := os.Lstat(filepath.Join(slotDir, "data"))
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("expected symlink")
		}
	})

	t.Run("creates empty repo dir if slot has no data", func(t *testing.T) {
		repoDir := t.TempDir()
		slotDir := t.TempDir()

		o := &orchestrator{
			cfg:     config{SharedDirs: []string{"data"}},
			repoDir: repoDir,
		}
		o.applySharedDirs(slotDir)

		// Repo's data dir should have been created (empty).
		info, err := os.Stat(filepath.Join(repoDir, "data"))
		if err != nil || !info.IsDir() {
			t.Fatal("expected repo data dir to be created")
		}

		// Slot should symlink to it.
		info, _ = os.Lstat(filepath.Join(slotDir, "data"))
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("expected symlink")
		}
	})

	t.Run("no shared dirs configured", func(t *testing.T) {
		slotDir := t.TempDir()
		os.MkdirAll(filepath.Join(slotDir, "data"), 0755)

		o := &orchestrator{cfg: config{}}
		o.applySharedDirs(slotDir)

		// data should still be a real directory.
		info, _ := os.Lstat(filepath.Join(slotDir, "data"))
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatal("should not create symlinks when not configured")
		}
	})

	t.Run("ignores absolute and dot paths", func(t *testing.T) {
		repoDir := t.TempDir()
		slotDir := t.TempDir()

		o := &orchestrator{
			cfg:     config{SharedDirs: []string{"/etc", ".", ".."}},
			repoDir: repoDir,
		}
		o.applySharedDirs(slotDir)

		// No symlinks should have been created in the slot.
		entries, _ := os.ReadDir(slotDir)
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				t.Fatalf("unexpected symlink: %s", e.Name())
			}
		}
	})
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		findSubstring(s, substr))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
