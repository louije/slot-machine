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
	"sync"
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

	// findFreePort is TOCTOU by construction: it binds :0, reads the port back
	// and closes the listener, so anything else on the machine — including a
	// parallel test doing the same thing — can claim it in the gap. Retry rather
	// than fail the build on a legitimate collision.
	var lastErr error
	for i := 0; i < 5; i++ {
		port, err := findFreePort()
		if err != nil {
			t.Fatalf("findFreePort: %v", err)
		}
		if port <= 0 || port > 65535 {
			t.Fatalf("port %d out of range", port)
		}

		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			l.Close()
			return
		}
		lastErr = err
	}
	t.Fatalf("findFreePort never returned a bindable port: %v", lastErr)
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

	// Re-binding after a clear must work: listen() retries briefly so an
	// in-flight close does not make the next deploy lose its own port.
	p.setTarget(bPort)
	time.Sleep(50 * time.Millisecond)

	resp, err = http.Get(fmt.Sprintf("http://%s/", addr))
	if err != nil {
		t.Fatalf("proxy did not come back after clearTarget: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after re-target, got %d", resp.StatusCode)
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
		a := &agentService{workDir: t.TempDir(), machineBranch: "machine", humanBranch: "main"}
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
		a := &agentService{workDir: dir, machineBranch: "machine", humanBranch: "main"}
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
		a := &agentService{workDir: dir, machineBranch: "machine", humanBranch: "main"}
		prompt := a.buildSystemPrompt()
		if !strings.Contains(prompt, "Generic agent.") {
			t.Fatal("expected AGENTS.md content")
		}
	})

	t.Run("CLAUDE.md as last resort", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Project context.\n"), 0644)
		a := &agentService{workDir: dir, machineBranch: "machine", humanBranch: "main"}
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

func TestSendMessageStoresAndEnqueues(t *testing.T) {
	store, err := openAgentStore(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()

	mgr := newAgentManager(store)
	defer mgr.stop()

	tmpDir := t.TempDir()
	a := &agentService{
		store:      store,
		manager:    mgr,
		agentBin:   "true", // succeeds, does nothing
		workDir:    tmpDir,
		configPath: filepath.Join(tmpDir, "slot-machine.json"),
		dataDir:    tmpDir,
		authMode:   "none",
		timeout:    30 * time.Second,
	}

	convID := "conv-store-test"
	store.createConversation(convID, "test")

	body := strings.NewReader(`{"content":"hello"}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/agent/conversations/"+convID+"/messages", body)
	a.handleSendMessage(w, r, convID)

	// Should be 200 (message stored + agent enqueued).
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Message stored in DB.
	msgs, _ := store.getMessages(convID, 0)
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

func TestStoreStatusMigration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s1, err := openAgentStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	s1.close()

	s2, err := openAgentStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.close()

	conv, _ := s2.createConversation("c1", "user1")
	if conv.Status != "idle" {
		t.Fatalf("expected status 'idle', got %q", conv.Status)
	}
}

func TestSetConversationStatus(t *testing.T) {
	t.Parallel()
	s, _ := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.close()

	s.createConversation("c1", "user1")

	if err := s.setConversationStatus("c1", "running"); err != nil {
		t.Fatal(err)
	}
	conv, _ := s.getConversation("c1")
	if conv.Status != "running" {
		t.Fatalf("expected 'running', got %q", conv.Status)
	}
}

func TestReapOrphans(t *testing.T) {
	t.Parallel()
	s, _ := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.close()

	s.createConversation("c1", "u")
	s.setConversationStatus("c1", "running")
	s.createConversation("c2", "u")
	s.setConversationStatus("c2", "running")
	s.createConversation("c3", "u") // idle, should not be touched

	mgr := newAgentManager(s)
	defer mgr.stop()

	n, err := mgr.reapOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 recovered, got %d", n)
	}

	c1, _ := s.getConversation("c1")
	if c1.Status != "interrupted" {
		t.Fatalf("expected 'interrupted', got %q", c1.Status)
	}

	msgs, _ := s.getMessages("c1", 0)
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

func TestAgentManagerStartStop(t *testing.T) {
	t.Parallel()
	s, _ := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.close()

	mgr := newAgentManager(s)
	mgr.stop()
}

func TestAgentManagerRunAgent(t *testing.T) {
	t.Parallel()
	s, _ := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.close()
	s.createConversation("c1", "user1")

	mgr := newAgentManager(s)
	defer mgr.stop()

	// "echo" as a mock agent: prints its argv, emits no stream-json, exits 0.
	err := mgr.enqueue(agentWork{
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
		c, _ := s.getConversation("c1")
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
	s, _ := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.close()
	s.createConversation("c1", "user1")

	mgr := newAgentManager(s)
	defer mgr.stop()

	err := mgr.enqueue(agentWork{
		convID: "c1", bin: mockAgentBin(t, "sleep 10"), dir: t.TempDir(),
		timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A second message for the *same* conversation is a conflict.
	err = mgr.enqueue(agentWork{
		convID: "c1", bin: "echo", dir: t.TempDir(), timeout: 30 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error for a second message on the same conversation, got nil")
	}
}

func TestAgentManagerCancel(t *testing.T) {
	t.Parallel()
	s, _ := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.close()
	s.createConversation("c1", "user1")

	mgr := newAgentManager(s)
	defer mgr.stop()

	mgr.enqueue(agentWork{
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
		c, _ := s.getConversation("c1")
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

func TestResolveClaudeFromEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\necho hi"), 0755)

	t.Setenv("SLOT_MACHINE_AGENT_BIN", bin)
	got := resolveClaude(dir)
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
	got := resolveClaude(dir)
	if got != bin {
		t.Fatalf("expected %s, got %s", bin, got)
	}
}

func TestResolveClaudeNotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SLOT_MACHINE_AGENT_BIN", "")
	got := resolveClaude(dir)
	// May find claude in PATH if installed, so just check it doesn't crash.
	_ = got
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
func TestStorePragmasApplied(t *testing.T) {
	t.Parallel()
	s, err := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	var journal string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}

	var busy int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy == 0 {
		t.Fatal("busy_timeout is 0; concurrent writes will fail with SQLITE_BUSY")
	}
}

// Concurrent readers and writers must not lose events. Before the pragma fix
// this produced well over a thousand SQLITE_BUSY errors, every one of them
// discarded by the caller.
func TestStoreSurvivesConcurrentAccess(t *testing.T) {
	t.Parallel()
	s, err := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	if _, err := s.createConversation("c1", "u"); err != nil {
		t.Fatal(err)
	}

	const writers, readers, iterations = 3, 3, 120

	var wg sync.WaitGroup
	errCh := make(chan error, (writers+readers)*iterations*2)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if _, err := s.addMessage("c1", "assistant", `{"content":"hello"}`); err != nil {
					errCh <- err
				}
			}
		}()
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if _, err := s.getMessages("c1", 0); err != nil {
					errCh <- err
				}
				if _, err := s.getConversation("c1"); err != nil {
					errCh <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	n := 0
	var first error
	for err := range errCh {
		if first == nil {
			first = err
		}
		n++
	}
	if n > 0 {
		t.Fatalf("%d store errors under concurrent access; first: %v", n, first)
	}

	msgs, err := s.getMessages("c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != writers*iterations {
		t.Fatalf("stored %d messages, want %d — writes were lost", len(msgs), writers*iterations)
	}
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "slot-machine.json")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A config without the optional timeouts must not inherit Go zero values:
// health_timeout_ms of 0 fails every deploy, drain_timeout_ms of 0 turns a
// graceful shutdown into an immediate SIGKILL.
func TestConfigAppliesDocumentedDefaults(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, `{"start_command":"./run.sh","port":3000}`)

	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HealthTimeoutMs != 10000 {
		t.Fatalf("health_timeout_ms = %d, want 10000", cfg.HealthTimeoutMs)
	}
	if cfg.DrainTimeoutMs != 5000 {
		t.Fatalf("drain_timeout_ms = %d, want 5000", cfg.DrainTimeoutMs)
	}
	if cfg.APIPort != 9100 {
		t.Fatalf("api_port = %d, want 9100", cfg.APIPort)
	}
	if cfg.HealthEndpoint != "/healthz" {
		t.Fatalf("health_endpoint = %q", cfg.HealthEndpoint)
	}
	if cfg.MachineBranch != "machine" || cfg.HumanBranch != "main" {
		t.Fatalf("branches = %q/%q", cfg.MachineBranch, cfg.HumanBranch)
	}
	if cfg.AgentTimeoutS == 0 {
		t.Fatal("agent_timeout_s must have a default; an agent with no timeout can hang forever")
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, body, wantErr string
	}{
		{"missing start_command", `{"port":3000}`, "start_command"},
		{"missing port", `{"start_command":"./run.sh"}`, "port is required"},
		{"health endpoint without slash", `{"start_command":"x","port":3000,"health_endpoint":"healthz"}`, "must start with /"},
		{"unknown auth mode", `{"start_command":"x","port":3000,"agent_auth":"maybe"}`, "agent_auth"},
		{"port collides with api_port", `{"start_command":"x","port":9100}`, "must differ"},
		{"branches collide", `{"start_command":"x","port":3000,"machine_branch":"main"}`, "own branch"},
		{"bad secret pattern", `{"start_command":"x","port":3000,"secret_patterns":["(unclosed"]}`, "not a valid regexp"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Failure classification
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

	args := buildAgentArgs(agentWork{
		prompt:       "-rf looks like a flag",
		sessionID:    "sess-1",
		model:        "claude-opus-5",
		systemPrompt: "context",
		allowedTools: []string{"Bash", "Read"},
	}, true)

	joined := strings.Join(args, " ")

	// Append, don't replace: --system-prompt would discard the CLI's own tool
	// guidance along with everything else it puts in there.
	if strings.Contains(joined, "--system-prompt ") {
		t.Fatal("must use --append-system-prompt, not --system-prompt")
	}
	if !strings.Contains(joined, "--append-system-prompt") {
		t.Fatal("missing --append-system-prompt")
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
	args := buildAgentArgs(agentWork{prompt: "hi"}, false)
	if strings.Contains(strings.Join(args, " "), "--resume") {
		t.Fatal("must not pass --resume when starting a fresh session")
	}
}

// ---------------------------------------------------------------------------
// Queue and drain
// ---------------------------------------------------------------------------

// Two conversations share one worktree, so only one agent may run at a time.
// The second must wait rather than being rejected or running concurrently.
func TestAgentManagerQueuesAcrossConversations(t *testing.T) {
	t.Parallel()
	s, _ := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.close()
	s.createConversation("c1", "u")
	s.createConversation("c2", "u")

	mgr := newAgentManager(s)
	defer mgr.stop()

	marker := filepath.Join(t.TempDir(), "concurrent")
	// Each run appends on entry and removes on exit; if two ever overlap the
	// file will hold two lines at once.
	script := "echo x >> " + marker + "; sleep 1; echo done"

	if err := mgr.enqueue(agentWork{
		convID: "c1", bin: mockAgentBin(t, script), dir: t.TempDir(), timeout: 30 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	// A different conversation is a queue, not a conflict.
	if err := mgr.enqueue(agentWork{
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
		c1, _ := s.getConversation("c1")
		c2, _ := s.getConversation("c2")
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
	s, _ := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.close()
	s.createConversation("c1", "u")

	mgr := newAgentManager(s)
	defer mgr.stop()

	mgr.enqueue(agentWork{
		convID: "c1", bin: mockAgentBin(t, "sleep 120"), dir: t.TempDir(),
		timeout: 500 * time.Millisecond,
	})

	deadline := time.After(20 * time.Second)
	for {
		c, _ := s.getConversation("c1")
		if c.Status == "error" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("a stuck agent was never timed out (status %q)", c.Status)
		case <-time.After(25 * time.Millisecond):
		}
	}

	msgs, _ := s.getMessages("c1", 0)
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
func TestAgentManagerReportsTerminalFailure(t *testing.T) {
	t.Parallel()
	s, _ := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.close()
	s.createConversation("c1", "u")

	mgr := newAgentManager(s)
	defer mgr.stop()

	bin := mockAgentBin(t, `echo "API Error: 401 Invalid authentication credentials" >&2; exit 1`)
	mgr.enqueue(agentWork{convID: "c1", bin: bin, dir: t.TempDir(), timeout: 30 * time.Second})

	deadline := time.After(20 * time.Second)
	for {
		c, _ := s.getConversation("c1")
		if c.Status == "error" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("run never settled (status %q)", c.Status)
		case <-time.After(25 * time.Millisecond):
		}
	}

	msgs, _ := s.getMessages("c1", 0)
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
func TestAgentManagerSurfacesStderr(t *testing.T) {
	t.Parallel()
	s, _ := openAgentStore(filepath.Join(t.TempDir(), "test.db"))
	defer s.close()
	s.createConversation("c1", "u")

	mgr := newAgentManager(s)
	defer mgr.stop()

	bin := mockAgentBin(t, `echo "something specific went wrong" >&2; exit 3`)
	mgr.enqueue(agentWork{convID: "c1", bin: bin, dir: t.TempDir(), timeout: 30 * time.Second})

	deadline := time.After(20 * time.Second)
	for {
		c, _ := s.getConversation("c1")
		if c.Status == "error" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("run never settled (status %q)", c.Status)
		case <-time.After(25 * time.Millisecond):
		}
	}

	msgs, _ := s.getMessages("c1", 0)
	joined := ""
	for _, m := range msgs {
		joined += m.Content
	}
	if !strings.Contains(joined, "something specific went wrong") {
		t.Fatalf("stderr must reach the conversation, got: %s", joined)
	}
}
