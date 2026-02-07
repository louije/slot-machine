// Specification tests for slot-machine.
//
// These scenarios validate any implementation of the slot-machine spec. The
// binary is a black box — we only interact with it through its CLI and HTTP API.
//
// Run:
//
//	go build -o spec/testapp/testapp ./spec/testapp/
//	go build -o slot-machine ./cmd/slot-machine/
//	ORCHESTRATOR_BIN=$(pwd)/slot-machine go test -v -count=1 ./spec/
//
// Each test gets its own git repo, config, data dir, and daemon instance.
// Nothing is shared between tests.
package spec

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testappBinary returns the absolute path to the compiled testapp binary.
// It expects the binary at testharness/testapp/testapp relative to the module root,
// or you can override it with the TESTAPP_BIN env var.
func testappBinary(t *testing.T) string {
	t.Helper()

	if bin := os.Getenv("TESTAPP_BIN"); bin != "" {
		abs, err := filepath.Abs(bin)
		if err != nil {
			t.Fatalf("resolving TESTAPP_BIN: %v", err)
		}
		return abs
	}

	// Try to find it relative to the test file.
	// When running `go test ./spec/`, the working dir is spec/.
	candidates := []string{
		"testapp/testapp",
		"spec/testapp/testapp",
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	t.Fatal("testapp binary not found — run: go build -o spec/testapp/testapp ./spec/testapp/")
	return ""
}

// ---------------------------------------------------------------------------
// Test 1: Deploy — health check passes
// ---------------------------------------------------------------------------
//
// Deploys a commit, verifies the orchestrator reports it as live and healthy,
// and confirms the app actually responds on its public port.
func TestDeployHealthy(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Deploy commit A.
	dr, code := deploy(t, apiPort, repo.CommitA)
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !dr.Success {
		t.Fatal("deploy reported success=false")
	}

	// Check status.
	st := status(t, apiPort)
	if st.LiveCommit != repo.CommitA {
		t.Fatalf("expected live_commit=%s, got %s", repo.CommitA, st.LiveCommit)
	}
	if !st.Healthy {
		t.Fatal("expected healthy=true")
	}

	// Verify the app's public port responds.
	statusCode, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", appPort))
	if statusCode != 200 {
		t.Fatalf("app public port returned %d, expected 200", statusCode)
	}
}

// ---------------------------------------------------------------------------
// Test 2: Deploy — health check fails
// ---------------------------------------------------------------------------
//
// Deploys a commit where the app starts with health check returning 503.
// The orchestrator should detect the failure, kill the process, and leave
// no live commit (or keep the previous one).
func TestDeployUnhealthy(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Deploy the "bad" commit — app starts with --start-unhealthy.
	dr, _ := deploy(t, apiPort, repo.CommitBad)
	if dr.Success {
		t.Fatal("expected deploy to fail, but success=true")
	}

	// Status should show no live commit (nothing was deployed before).
	st := status(t, apiPort)
	if st.LiveCommit != "" {
		t.Fatalf("expected empty live_commit after failed deploy, got %s", st.LiveCommit)
	}

	// The failed process should have been killed — port should not respond.
	waitForDown(t, appPort, 5*time.Second)
}

// ---------------------------------------------------------------------------
// Test 3: Deploy then rollback
// ---------------------------------------------------------------------------
//
// Deploys two commits, then rolls back. After rollback, the first commit
// should be live and the app should respond.
func TestDeployThenRollback(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Deploy A, then B.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy A failed")
	}

	dr, _ = deploy(t, apiPort, repo.CommitB)
	if !dr.Success {
		t.Fatal("deploy B failed")
	}

	// Status should show B live.
	st := status(t, apiPort)
	if st.LiveCommit != repo.CommitB {
		t.Fatalf("expected live_commit=%s, got %s", repo.CommitB, st.LiveCommit)
	}

	// Rollback.
	rr, code := rollback(t, apiPort)
	if code != 200 {
		t.Fatalf("rollback returned %d", code)
	}
	if !rr.Success {
		t.Fatal("rollback reported success=false")
	}

	// Status should show A live again.
	st = status(t, apiPort)
	if st.LiveCommit != repo.CommitA {
		t.Fatalf("after rollback: expected live_commit=%s, got %s", repo.CommitA, st.LiveCommit)
	}

	// App should respond on the public port.
	statusCode, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", appPort))
	if statusCode != 200 {
		t.Fatalf("app public port returned %d after rollback, expected 200", statusCode)
	}
}

// ---------------------------------------------------------------------------
// Test 4: Deploy twice — only one previous slot
// ---------------------------------------------------------------------------
//
// Deploys A, then B, then A again. After each deploy, the previous_commit
// should only reflect the immediately prior deploy, not anything older.
func TestOnlyOnePreviousSlot(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Deploy A.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy A failed")
	}

	// Deploy B.
	dr, _ = deploy(t, apiPort, repo.CommitB)
	if !dr.Success {
		t.Fatal("deploy B failed")
	}

	st := status(t, apiPort)
	if st.PreviousCommit != repo.CommitA {
		t.Fatalf("after A→B: expected previous_commit=%s, got %s", repo.CommitA, st.PreviousCommit)
	}

	// Deploy A again.
	dr, _ = deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy A (second time) failed")
	}

	st = status(t, apiPort)
	if st.PreviousCommit != repo.CommitB {
		t.Fatalf("after A→B→A: expected previous_commit=%s, got %s", repo.CommitB, st.PreviousCommit)
	}
}

// ---------------------------------------------------------------------------
// Test 5: Rollback with no previous slot
// ---------------------------------------------------------------------------
//
// Tries to rollback on a fresh orchestrator that has never deployed anything.
// Should get an error response.
func TestRollbackNoPrevious(t *testing.T) {
	bin := orchestratorBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	// We still need a valid repo and contract even though we won't deploy.
	appBin := testappBinary(t)
	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Attempt rollback with nothing deployed.
	_, code := rollback(t, apiPort)
	if code >= 200 && code < 300 {
		t.Fatalf("expected error status code for rollback with no previous, got %d", code)
	}
}

// ---------------------------------------------------------------------------
// Test 6: Deploy while deploy in progress
// ---------------------------------------------------------------------------
//
// Starts a deploy with a slow-booting app (3s boot delay), then immediately
// tries a second deploy. The second should be rejected (409 or similar).
func TestConcurrentDeployRejected(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Start deploying the slow commit (3s boot delay) asynchronously.
	slowResult := deployAsync(t, apiPort, repo.CommitSlow)

	// Give the orchestrator a moment to start processing the first deploy.
	time.Sleep(500 * time.Millisecond)

	// Try a second deploy while the first is still booting.
	dr, code := deploy(t, apiPort, repo.CommitA)

	// The second deploy should be rejected.
	if code >= 200 && code < 300 && dr.Success {
		t.Fatalf("expected second deploy to be rejected, but got success (status %d)", code)
	}

	// Wait for the first deploy to finish (it may succeed or we don't care).
	select {
	case <-slowResult:
		// done
	case <-time.After(15 * time.Second):
		t.Fatal("slow deploy timed out")
	}
}

// ---------------------------------------------------------------------------
// Test 7: Process crashes after promotion
// ---------------------------------------------------------------------------
//
// Deploys a commit successfully, then crashes the app process. The orchestrator's
// status should reflect that the app is no longer healthy.
func TestProcessCrashDetected(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Deploy commit A.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

	st := status(t, apiPort)
	if !st.Healthy {
		t.Fatal("expected healthy=true after deploy")
	}

	// Crash the app by calling /control/crash on the internal port.
	httpPost(t, fmt.Sprintf("http://127.0.0.1:%d/control/crash", intPort))

	// Wait for the process to actually die.
	waitForDown(t, appPort, 5*time.Second)

	// Give the orchestrator a moment to detect the crash.
	// It may poll health or detect process exit — either way, wait briefly.
	time.Sleep(2 * time.Second)

	// Status should now reflect unhealthy.
	st = status(t, apiPort)
	if st.Healthy {
		t.Fatal("expected healthy=false after crash, but got true")
	}
}

// ---------------------------------------------------------------------------
// Test 8: Drain timeout exceeded
// ---------------------------------------------------------------------------
//
// Deploys a commit, makes it ignore SIGTERM (via /control/hang), then deploys
// a second commit. The orchestrator must force-kill the old process after the
// drain timeout and successfully promote the new one.
func TestDrainTimeoutForceKill(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)

	// Use a short drain timeout (1 second) so the test doesn't take forever.
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 1000)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Deploy commit A.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy A failed")
	}

	// Make the app ignore SIGTERM — it will only die to SIGKILL.
	httpPost(t, fmt.Sprintf("http://127.0.0.1:%d/control/hang", intPort))

	// Deploy commit B — the orchestrator will try to drain A, but it won't stop
	// gracefully. After drain_timeout_ms (1s), it should SIGKILL the old process.
	dr, code := deploy(t, apiPort, repo.CommitB)
	if code != 200 {
		t.Fatalf("deploy B returned %d", code)
	}
	if !dr.Success {
		t.Fatal("deploy B failed — old process may not have been force-killed")
	}

	// Status should show commit B as live.
	st := status(t, apiPort)
	if st.LiveCommit != repo.CommitB {
		t.Fatalf("expected live_commit=%s, got %s", repo.CommitB, st.LiveCommit)
	}
}

// ===========================================================================
// CLI UX tests
// ===========================================================================

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
// Test 9: No args — prints usage
// ---------------------------------------------------------------------------

func TestNoArgs(t *testing.T) {
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
// Test 10: Unknown command
// ---------------------------------------------------------------------------

func TestUnknownCommand(t *testing.T) {
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
// Test 11: start without config
// ---------------------------------------------------------------------------

func TestStartMissingConfig(t *testing.T) {
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
// Test 12: init — bun project detection
// ---------------------------------------------------------------------------

func TestInitBunProject(t *testing.T) {
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
// Test 13: init — appends .slot-machine to .gitignore (idempotent)
// ---------------------------------------------------------------------------

func TestInitAppendsGitignore(t *testing.T) {
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
// Test 14: deploy with no running daemon
// ---------------------------------------------------------------------------

func TestDeployNoRunningDaemon(t *testing.T) {
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

// ===========================================================================
// Feature tests
// ===========================================================================

// ---------------------------------------------------------------------------
// Test 15: env_file vars are passed to the app
// ---------------------------------------------------------------------------

func TestEnvFilePassedToApp(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)

	// Write a contract with env_file pointing to a custom .env.
	contractDir := t.TempDir()
	envPath := filepath.Join(contractDir, "test.env")
	os.WriteFile(envPath, []byte("MY_TEST_VAR=hello_from_env\n"), 0644)

	contract := map[string]any{
		"start_command":     "./start.sh",
		"port":              appPort,
		"internal_port":     intPort,
		"health_endpoint":   "/healthz",
		"health_timeout_ms": 5000,
		"drain_timeout_ms":  10000,
		"env_file":          envPath,
	}
	data, _ := json.MarshalIndent(contract, "", "  ")
	contractPath := filepath.Join(contractDir, "app.contract.json")
	os.WriteFile(contractPath, data, 0644)

	orch := startOrchestrator(t, bin, contractPath, repo.Dir, apiPort)
	_ = orch

	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

	// Query the testapp's /env endpoint on the internal port.
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/env?key=MY_TEST_VAR", intPort))
	if err != nil {
		t.Fatalf("GET /env: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if string(body) != "hello_from_env" {
		t.Fatalf("expected MY_TEST_VAR=hello_from_env, got: %s", string(body))
	}
}

// ---------------------------------------------------------------------------
// Test 16: setup_command runs before start
// ---------------------------------------------------------------------------

func TestSetupCommandRuns(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)

	// Write a contract with setup_command that creates a marker file.
	contractDir := t.TempDir()
	contract := map[string]any{
		"start_command":     "./start.sh",
		"setup_command":     "touch .setup-done",
		"port":              appPort,
		"internal_port":     intPort,
		"health_endpoint":   "/healthz",
		"health_timeout_ms": 5000,
		"drain_timeout_ms":  10000,
	}
	data, _ := json.MarshalIndent(contract, "", "  ")
	contractPath := filepath.Join(contractDir, "app.contract.json")
	os.WriteFile(contractPath, data, 0644)

	orch := startOrchestrator(t, bin, contractPath, repo.Dir, apiPort)

	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

	// Check that .setup-done exists in the slot directory.
	// The slot dir is inside the data dir: <dataDir>/slot-a/.setup-done
	marker := filepath.Join(orch.DataDir, "slot-a", ".setup-done")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Fatalf("setup_command did not run: %s not found", marker)
	}
}

// ---------------------------------------------------------------------------
// Test 17: Daemon shutdown drains managed processes
// ---------------------------------------------------------------------------

func TestDaemonShutdownDrainsProcesses(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	// Start orchestrator manually (not via startOrchestrator, which registers
	// cleanup that would race with our explicit SIGTERM).
	dataDir := t.TempDir()
	cmd := exec.Command(bin,
		"start",
		"--config", contract,
		"--repo", repo.Dir,
		"--data", dataDir,
		"--port", fmt.Sprintf("%d", apiPort),
		"--no-proxy",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting daemon: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()
	})

	waitForHealth(t, apiPort, 5*time.Second)

	// Deploy so there's a running app process.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

	// Verify app is up.
	waitForHealth(t, appPort, 5*time.Second)

	// Send SIGTERM to the daemon.
	cmd.Process.Signal(syscall.SIGTERM)

	// Wait for the daemon to exit.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not exit after SIGTERM")
	}

	// App port should be down — no orphan processes.
	waitForDown(t, appPort, 5*time.Second)
}
