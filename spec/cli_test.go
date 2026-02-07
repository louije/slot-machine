// CLI UX tests for the Go slot-machine binary.
//
// These test the command-line interface — argument parsing, init scaffolding,
// error messages. They are specific to the Go implementation and don't apply
// to alternative implementations (Rust, etc.).
package spec

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runBinary runs the slot-machine binary with the given args and working dir.
// Returns stdout, stderr, and exit code.
func runBinary(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	bin := orchestratorBinary(t)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("running binary: %v", err)
		}
	}
	return stdout.String(), stderr.String(), exitCode
}

// ---------------------------------------------------------------------------
// Test: No args — prints usage
// ---------------------------------------------------------------------------

func TestNoArgs(t *testing.T) {
	t.Parallel()
	_ = orchestratorBinary(t)
	_, stderr, code := runBinary(t, t.TempDir())
	if code == 0 {
		t.Fatal("expected non-zero exit code with no args")
	}
	if !strings.Contains(stderr, "usage") {
		t.Fatalf("expected stderr to mention 'usage', got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Test: Unknown command
// ---------------------------------------------------------------------------

func TestUnknownCommand(t *testing.T) {
	t.Parallel()
	_ = orchestratorBinary(t)
	_, stderr, code := runBinary(t, t.TempDir(), "badcmd")
	if code == 0 {
		t.Fatal("expected non-zero exit code for unknown command")
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Fatalf("expected stderr to mention 'unknown command', got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Test: start without config
// ---------------------------------------------------------------------------

func TestStartMissingConfig(t *testing.T) {
	t.Parallel()
	_ = orchestratorBinary(t)
	dir := t.TempDir()
	_, stderr, code := runBinary(t, dir, "start")
	if code == 0 {
		t.Fatal("expected non-zero exit code when config is missing")
	}
	if !strings.Contains(stderr, "slot-machine.json") {
		t.Fatalf("expected stderr to mention 'slot-machine.json', got: %s", stderr)
	}
	if !strings.Contains(stderr, "init") {
		t.Fatalf("expected stderr to suggest 'init', got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// Test: init — bun project detection
// ---------------------------------------------------------------------------

func TestInitBunProject(t *testing.T) {
	t.Parallel()
	_ = orchestratorBinary(t)
	dir := t.TempDir()

	// Create bun.lock and package.json.
	os.WriteFile(filepath.Join(dir, "bun.lock"), []byte(""), 0644)
	pkg := map[string]any{
		"scripts": map[string]string{"start": "bun server/index.ts"},
	}
	data, _ := json.Marshal(pkg)
	os.WriteFile(filepath.Join(dir, "package.json"), data, 0644)

	// Create .env to test env_file detection.
	os.WriteFile(filepath.Join(dir, ".env"), []byte("FOO=bar\n"), 0644)

	stdout, _, code := runBinary(t, dir, "init")
	if code != 0 {
		t.Fatalf("init exited %d", code)
	}
	if !strings.Contains(stdout, "slot-machine.json") {
		t.Fatalf("expected stdout to mention slot-machine.json, got: %s", stdout)
	}

	// Verify generated config.
	cfgData, err := os.ReadFile(filepath.Join(dir, "slot-machine.json"))
	if err != nil {
		t.Fatalf("reading generated config: %v", err)
	}
	var cfg map[string]any
	json.Unmarshal(cfgData, &cfg)

	if cfg["setup_command"] != "bun install --frozen-lockfile" {
		t.Fatalf("expected bun setup_command, got: %v", cfg["setup_command"])
	}
	if cfg["start_command"] != "bun server/index.ts" {
		t.Fatalf("expected start_command from package.json, got: %v", cfg["start_command"])
	}
	if cfg["env_file"] != ".env" {
		t.Fatalf("expected env_file=.env, got: %v", cfg["env_file"])
	}
	if cfg["health_endpoint"] != "/healthz" {
		t.Fatalf("expected health_endpoint=/healthz, got: %v", cfg["health_endpoint"])
	}
}

// ---------------------------------------------------------------------------
// Test: init — appends .slot-machine to .gitignore (idempotent)
// ---------------------------------------------------------------------------

func TestInitAppendsGitignore(t *testing.T) {
	t.Parallel()
	_ = orchestratorBinary(t)
	dir := t.TempDir()

	// Minimal setup so init doesn't fail.
	os.WriteFile(filepath.Join(dir, "bun.lock"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"start":"bun index.ts"}}`), 0644)

	// First init.
	runBinary(t, dir, "init")
	data, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	count := strings.Count(string(data), ".slot-machine")
	if count != 1 {
		t.Fatalf("expected 1 .slot-machine entry, got %d", count)
	}

	// Second init — should not duplicate.
	runBinary(t, dir, "init")
	data, _ = os.ReadFile(filepath.Join(dir, ".gitignore"))
	count = strings.Count(string(data), ".slot-machine")
	if count != 1 {
		t.Fatalf("expected 1 .slot-machine entry after second init, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// Test: deploy with no running daemon
// ---------------------------------------------------------------------------

func TestDeployNoRunningDaemon(t *testing.T) {
	t.Parallel()
	_ = orchestratorBinary(t)
	dir := t.TempDir()

	// Write a minimal config so the client can read api_port.
	cfg := map[string]any{"api_port": freePort(t)}
	data, _ := json.Marshal(cfg)
	os.WriteFile(filepath.Join(dir, "slot-machine.json"), data, 0644)

	// Also need a git repo for HEAD resolution.
	exec.Command("git", "init", dir).Run()
	exec.Command("git", "-C", dir, "commit", "--allow-empty", "-m", "init").Run()

	_, stderr, code := runBinary(t, dir, "deploy")
	if code == 0 {
		t.Fatal("expected non-zero exit code when daemon is not running")
	}
	if !strings.Contains(stderr, "cannot reach") {
		t.Fatalf("expected stderr to mention connection failure, got: %s", stderr)
	}
}
