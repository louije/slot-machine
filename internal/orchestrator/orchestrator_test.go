package orchestrator

import (
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slot-machine/internal/config"
	"slot-machine/internal/proxy"
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
		if got := ShortHash(tt.in); got != tt.want {
			t.Errorf("ShortHash(%q) = %q, want %q", tt.in, got, tt.want)
		}
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

func TestBuildEnvIncludesSlotMachine(t *testing.T) {
	t.Parallel()
	o := &Orchestrator{cfg: config.Config{}}
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

func TestOrchestratorServeHTTP(t *testing.T) {
	t.Parallel()

	o := &Orchestrator{
		appProxy: proxy.New("", nil),
		intProxy: proxy.New("", nil),
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
	o := &Orchestrator{
		appProxy: proxy.New("", nil),
		intProxy: proxy.New("", nil),
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
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
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

		o := &Orchestrator{
			cfg:     config.Config{SharedDirs: []string{"data"}},
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

		o := &Orchestrator{
			cfg:     config.Config{SharedDirs: []string{"data"}},
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

		o := &Orchestrator{
			cfg:     config.Config{SharedDirs: []string{"data"}},
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

		o := &Orchestrator{cfg: config.Config{}}
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

		o := &Orchestrator{
			cfg:     config.Config{SharedDirs: []string{"/etc", ".", ".."}},
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

func TestBuildEnvResolvesEnvFileRelativeToRepoDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=hunter2\n"), 0644)

	o := &Orchestrator{
		cfg:     config.Config{EnvFile: ".env"},
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
