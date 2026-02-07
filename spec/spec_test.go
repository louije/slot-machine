// Specification tests for slot-machine.
//
// These scenarios validate any implementation of the slot-machine spec. The
// binary is a black box — we only interact with it through its HTTP API.
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

func TestDeployHealthy(t *testing.T) {
	t.Parallel()
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

func TestDeployUnhealthy(t *testing.T) {
	t.Parallel()
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

func TestDeployThenRollback(t *testing.T) {
	t.Parallel()
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

func TestOnlyOnePreviousSlot(t *testing.T) {
	t.Parallel()
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

func TestRollbackNoPrevious(t *testing.T) {
	t.Parallel()
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

func TestConcurrentDeployRejected(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Start deploying the slow commit asynchronously.
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

func TestProcessCrashDetected(t *testing.T) {
	t.Parallel()
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

	// Give the orchestrator a moment to detect the crash via process exit.
	time.Sleep(500 * time.Millisecond)

	// Status should now reflect unhealthy.
	st = status(t, apiPort)
	if st.Healthy {
		t.Fatal("expected healthy=false after crash, but got true")
	}
}

// ---------------------------------------------------------------------------
// Test 8: Drain timeout exceeded
// ---------------------------------------------------------------------------

func TestDrainTimeoutForceKill(t *testing.T) {
	t.Parallel()
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

// ---------------------------------------------------------------------------
// Test 15: env_file vars are passed to the app
// ---------------------------------------------------------------------------

func TestEnvFilePassedToApp(t *testing.T) {
	t.Parallel()
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
		"health_timeout_ms": 3000,
		"drain_timeout_ms":  2000,
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
	t.Parallel()
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
		"health_timeout_ms": 3000,
		"drain_timeout_ms":  2000,
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
	marker := filepath.Join(orch.DataDir, dr.Slot, ".setup-done")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Fatalf("setup_command did not run: %s not found", marker)
	}
}

// ---------------------------------------------------------------------------
// Test 17: Daemon shutdown drains managed processes
// ---------------------------------------------------------------------------

func TestDaemonShutdownDrainsProcesses(t *testing.T) {
	t.Parallel()
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
