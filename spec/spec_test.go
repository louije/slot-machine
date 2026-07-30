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

func TestDeployHealthy(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
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

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Establish a known-good live slot, so "untouched" is something we can see.
	mustDeploy(t, apiPort, repo.CommitA)
	before := status(t, apiPort)

	// Deploy the "bad" commit — app starts with --start-unhealthy.
	dr, code := deploy(t, apiPort, repo.CommitBad)
	if dr.Success {
		t.Fatal("expected deploy to fail, but success=true")
	}
	if dr.Stage != "probe" {
		t.Fatalf("stage = %q, want %q (error: %s)", dr.Stage, "probe", dr.Error)
	}
	if code == 200 {
		t.Fatal("expected a non-200 status for a refused deploy")
	}

	// The live slot is untouched, and still serving.
	after := status(t, apiPort)
	if after.LiveCommit != before.LiveCommit {
		t.Fatalf("live commit changed on a failed deploy: %s → %s",
			before.LiveCommit, after.LiveCommit)
	}
	statusCode, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", appPort))
	if statusCode != 200 {
		t.Fatalf("the previous version stopped serving after a failed deploy: got %d", statusCode)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Deploy then rollback
// ---------------------------------------------------------------------------

func TestDeployThenRollback(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy A, then B.
	dr := mustDeploy(t, apiPort, repo.CommitA)

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
	mustRollback(t, apiPort)

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

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy A.
	dr := mustDeploy(t, apiPort, repo.CommitA)

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

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	// We still need a valid repo and contract even though we won't deploy.
	appBin := testappBinary(t)
	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
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

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
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

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy commit A.
	mustDeploy(t, apiPort, repo.CommitA)

	st := status(t, apiPort)
	if !st.Healthy {
		t.Fatal("expected healthy=true after deploy")
	}

	// Crash the app by calling /control/crash on the internal port.
	httpPost(t, fmt.Sprintf("http://127.0.0.1:%d/control/crash", intPort))

	// The public port stays bound and starts answering 503. It must not start
	// refusing connections: a client cannot distinguish that from the machine
	// being gone, and releasing the port lets something else claim it while the
	// app is down.
	waitForStatusCode(t, appPort, http.StatusServiceUnavailable, 5*time.Second)

	// Status should now reflect unhealthy.
	st = status(t, apiPort)
	if st.Healthy {
		t.Fatal("expected healthy=false after crash, but got true")
	}
	// ...and the port is still ours, which is what makes the next deploy able
	// to reclaim it without a race.
	if !st.ProxyListening {
		t.Fatal("expected the proxy to still be listening after an app crash")
	}
}

// ---------------------------------------------------------------------------
// Test 8: Drain timeout exceeded
// ---------------------------------------------------------------------------

func TestDrainTimeoutForceKill(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)

	// Use a short drain timeout (1 second) so the test doesn't take forever.
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 1000)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy commit A.
	dr := mustDeploy(t, apiPort, repo.CommitA)

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

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

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

	orch := startOrchestrator(t, bin, contractPath, repo.Dir, apiPort, release)
	_ = orch

	mustDeploy(t, apiPort, repo.CommitA)

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
// Test 15b: SLOT_MACHINE env var is set
// ---------------------------------------------------------------------------

func TestSlotMachineEnvVar(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	mustDeploy(t, apiPort, repo.CommitA)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/env?key=SLOT_MACHINE", intPort))
	if err != nil {
		t.Fatalf("GET /env: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if string(body) != "1" {
		t.Fatalf("expected SLOT_MACHINE=1, got: %q", string(body))
	}
}

// ---------------------------------------------------------------------------
// Test 16: setup_command runs before start
// ---------------------------------------------------------------------------

func TestSetupCommandRuns(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

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

	orch := startOrchestrator(t, bin, contractPath, repo.Dir, apiPort, release)

	dr := mustDeploy(t, apiPort, repo.CommitA)

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

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

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
	cmd.Env = daemonEnv(t)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	release()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting daemon: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()
	})

	waitForHealth(t, apiPort, 5*time.Second)

	// Deploy so there's a running app process.
	mustDeploy(t, apiPort, repo.CommitA)

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

// ---------------------------------------------------------------------------
// Test 18: Zero downtime during deploy
// ---------------------------------------------------------------------------
//
// Deploys commit A, then starts a slow deploy of commit B. While B is
// booting, the public port must continue responding (A still serving).

func TestZeroDowntime(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy commit A.
	mustDeploy(t, apiPort, repo.CommitA)
	waitForHealth(t, appPort, 5*time.Second)

	// Start deploying the slow commit (3s boot delay) asynchronously.
	slowResult := deployAsync(t, apiPort, repo.CommitSlow)

	// Wait for the deploy to be in progress.
	time.Sleep(1 * time.Second)

	// The public port must still respond during deploy.
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", appPort))
	if err != nil {
		t.Fatalf("zero downtime violated: port %d not responding during deploy: %v", appPort, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("zero downtime violated: port %d returned %d during deploy", appPort, resp.StatusCode)
	}

	// Wait for the slow deploy to finish.
	select {
	case result := <-slowResult:
		if result.Err != nil {
			t.Fatalf("slow deploy: %v", result.Err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("slow deploy timed out")
	}
}

// ---------------------------------------------------------------------------
// Test 19: Status includes staging directory
// ---------------------------------------------------------------------------

func TestStatusIncludesStagingDir(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	mustDeploy(t, apiPort, repo.CommitA)

	st := status(t, apiPort)
	if st.StagingDir == "" {
		t.Fatal("expected staging_dir in status response, got empty string")
	}
}

// ---------------------------------------------------------------------------
// Test 20: Staging preserves artifacts from promoted slot
// ---------------------------------------------------------------------------

func TestStagingPreservesArtifacts(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)

	contractDir := t.TempDir()
	cfg := map[string]any{
		"start_command":     "./start.sh",
		"setup_command":     "touch .setup-marker",
		"port":              appPort,
		"internal_port":     intPort,
		"health_endpoint":   "/healthz",
		"health_timeout_ms": 3000,
		"drain_timeout_ms":  2000,
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	contractPath := filepath.Join(contractDir, "app.contract.json")
	os.WriteFile(contractPath, data, 0644)

	orch := startOrchestrator(t, bin, contractPath, repo.Dir, apiPort, release)

	mustDeploy(t, apiPort, repo.CommitA)

	// The staging directory should exist and contain the marker.
	stagingDir := filepath.Join(orch.DataDir, "slot-staging")
	marker := filepath.Join(stagingDir, ".setup-marker")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("staging should preserve artifacts from promoted slot: %s not found: %v", marker, err)
	}
}

// ---------------------------------------------------------------------------
// Test 21: Symlinks on disk
// ---------------------------------------------------------------------------

func TestSymlinksOnDisk(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	// Deploy A.
	dr := mustDeploy(t, apiPort, repo.CommitA)

	// Check live symlink exists and references commit A.
	liveLink := filepath.Join(orch.DataDir, "live")
	target, err := os.Readlink(liveLink)
	if err != nil {
		t.Fatalf("expected live symlink at %s: %v", liveLink, err)
	}
	if !strings.Contains(target, repo.CommitA[:8]) {
		t.Fatalf("live symlink %s does not reference commit %s", target, repo.CommitA[:8])
	}

	// Deploy B.
	dr, _ = deploy(t, apiPort, repo.CommitB)
	if !dr.Success {
		t.Fatal("deploy B failed")
	}

	// live → commit B.
	target, err = os.Readlink(liveLink)
	if err != nil {
		t.Fatalf("live symlink missing after second deploy: %v", err)
	}
	if !strings.Contains(target, repo.CommitB[:8]) {
		t.Fatalf("live symlink %s does not reference commit %s", target, repo.CommitB[:8])
	}

	// prev → commit A.
	prevLink := filepath.Join(orch.DataDir, "prev")
	target, err = os.Readlink(prevLink)
	if err != nil {
		t.Fatalf("expected prev symlink at %s: %v", prevLink, err)
	}
	if !strings.Contains(target, repo.CommitA[:8]) {
		t.Fatalf("prev symlink %s does not reference commit %s", target, repo.CommitA[:8])
	}
}

// ---------------------------------------------------------------------------
// Test 22: Daemon restart preserves state
// ---------------------------------------------------------------------------

func TestDaemonRestart(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contractPath := writeTestContract(t, t.TempDir(), appPort, intPort, 0)
	dataDir := t.TempDir()

	startDaemon := func() *exec.Cmd {
		t.Helper()
		cmd := exec.Command(bin,
			"start",
			"--config", contractPath,
			"--repo", repo.Dir,
			"--data", dataDir,
			"--port", fmt.Sprintf("%d", apiPort),
			"--no-proxy",
		)
		cmd.Env = daemonEnv(t)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting daemon: %v", err)
		}
		return cmd
	}

	stopDaemon := func(cmd *exec.Cmd) {
		t.Helper()
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Signal(syscall.SIGKILL)
			<-done
		}
	}

	// First run: deploy A.
	release()
	cmd1 := startDaemon()
	waitForHealth(t, apiPort, 5*time.Second)

	mustDeploy(t, apiPort, repo.CommitA)
	st := status(t, apiPort)
	if st.LiveCommit != repo.CommitA {
		t.Fatalf("expected live_commit=%s, got %s", repo.CommitA, st.LiveCommit)
	}

	// Stop daemon.
	stopDaemon(cmd1)
	time.Sleep(500 * time.Millisecond)

	// Second run: same data dir — state should persist (ports already released).
	cmd2 := startDaemon()
	defer stopDaemon(cmd2)
	waitForHealth(t, apiPort, 5*time.Second)

	st = status(t, apiPort)
	if st.LiveCommit != repo.CommitA {
		t.Fatalf("after restart: expected live_commit=%s, got %s (state not persisted)", repo.CommitA, st.LiveCommit)
	}
}

// ---------------------------------------------------------------------------
// Test 23: Garbage collection
// ---------------------------------------------------------------------------
//
// After three deploys (A → B → C), the first deploy's slot directory
// should be garbage collected. Only live + prev slot dirs remain.

func TestGarbageCollection(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	// Deploy A.
	dr := mustDeploy(t, apiPort, repo.CommitA)

	// A's slot dir should use hash-based naming.
	aSlotDir := filepath.Join(orch.DataDir, fmt.Sprintf("slot-%s", repo.CommitA[:8]))
	if _, err := os.Stat(aSlotDir); err != nil {
		t.Fatalf("after deploy A: expected %s to exist (hash-based slot naming): %v", aSlotDir, err)
	}

	// Deploy B.
	dr, _ = deploy(t, apiPort, repo.CommitB)
	if !dr.Success {
		t.Fatal("deploy B failed")
	}

	// Deploy C (third deploy triggers GC of A).
	dr, _ = deploy(t, apiPort, repo.CommitC)
	if !dr.Success {
		t.Fatal("deploy C failed")
	}

	// A should be garbage collected.
	if _, err := os.Stat(aSlotDir); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be garbage collected after three deploys", aSlotDir)
	}

	// B should still exist (it's prev).
	bSlotDir := filepath.Join(orch.DataDir, fmt.Sprintf("slot-%s", repo.CommitB[:8]))
	if _, err := os.Stat(bSlotDir); err != nil {
		t.Fatalf("prev slot %s should still exist: %v", bSlotDir, err)
	}
}

// ---------------------------------------------------------------------------
// Test 24: Rollback then deploy
// ---------------------------------------------------------------------------
//
// After a rollback, deploying a new commit should work normally.

func TestRollbackThenDeploy(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy A, then B.
	dr := mustDeploy(t, apiPort, repo.CommitA)
	dr, _ = deploy(t, apiPort, repo.CommitB)
	if !dr.Success {
		t.Fatal("deploy B failed")
	}

	// Rollback to A.
	rr, code := rollback(t, apiPort)
	if code != 200 || !rr.Success {
		t.Fatalf("rollback failed: code=%d", code)
	}
	st := status(t, apiPort)
	if st.LiveCommit != repo.CommitA {
		t.Fatalf("after rollback: expected live=%s, got %s", repo.CommitA, st.LiveCommit)
	}

	// Deploy C — should work after rollback.
	dr, _ = deploy(t, apiPort, repo.CommitC)
	if !dr.Success {
		t.Fatal("deploy after rollback failed")
	}
	st = status(t, apiPort)
	if st.LiveCommit != repo.CommitC {
		t.Fatalf("after post-rollback deploy: expected live=%s, got %s", repo.CommitC, st.LiveCommit)
	}
}

// ---------------------------------------------------------------------------
// Test 25: Re-deploy same commit
// ---------------------------------------------------------------------------
//
// Deploying the same commit that is already live should succeed and use a
// proper hash-based slot name (not "slot-staging").

func TestRedeploySameCommit(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy A, then B.
	dr := mustDeploy(t, apiPort, repo.CommitA)
	dr, _ = deploy(t, apiPort, repo.CommitB)
	if !dr.Success {
		t.Fatal("deploy B failed")
	}

	// Re-deploy B (same commit as live).
	dr, _ = deploy(t, apiPort, repo.CommitB)
	if !dr.Success {
		t.Fatal("re-deploy B failed")
	}

	expectedSlot := fmt.Sprintf("slot-%s", repo.CommitB[:8])
	if dr.Slot != expectedSlot {
		t.Fatalf("re-deploy slot = %q, want %q (should not be slot-staging)", dr.Slot, expectedSlot)
	}

	st := status(t, apiPort)
	if st.LiveCommit != repo.CommitB {
		t.Fatalf("expected live=%s, got %s", repo.CommitB, st.LiveCommit)
	}
	if st.LiveSlot != expectedSlot {
		t.Fatalf("expected live_slot=%s, got %s", expectedSlot, st.LiveSlot)
	}
}

// ---------------------------------------------------------------------------
// Test 26: Deploy with short commit hash
// ---------------------------------------------------------------------------
//
// Git short hashes (7 chars) are common. The orchestrator must not panic
// when the commit string is shorter than 8 characters.

func TestDeployShortHash(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy with a 7-char short hash (git's default).
	shortCommit := repo.CommitA[:7]
	dr, code := deploy(t, apiPort, shortCommit)
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !dr.Success {
		t.Fatal("deploy with short hash failed")
	}

	// The commit is resolved to its full hash before anything else happens, so
	// a slot is named by its commit rather than by however the caller spelled
	// it. Naming the slot from the raw input meant the same commit could land in
	// two differently-named slots depending on whether you passed 7 characters
	// or 40, which then made the re-deploy and rollback paths disagree about
	// what was already on disk.
	if dr.Commit != repo.CommitA {
		t.Fatalf("commit = %q, want the resolved full hash %q", dr.Commit, repo.CommitA)
	}
	expectedSlot := fmt.Sprintf("slot-%s", repo.CommitA[:8])
	if dr.Slot != expectedSlot {
		t.Fatalf("slot = %q, want %q", dr.Slot, expectedSlot)
	}
}

// ---------------------------------------------------------------------------
// An app that boots too slowly fails its health check
// ---------------------------------------------------------------------------
//
// The rest of the suite uses a generous health_timeout_ms so that a loaded
// machine cannot masquerade as a failing deploy. That leaves the timeout itself
// uncovered, so cover it here deliberately, with a window short enough that the
// outcome is about the app and not about the runner.

func TestDeploySlowAppFailsHealthCheck(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeGateContract(t, t.TempDir(), appPort, intPort, map[string]any{
		"health_timeout_ms": 800, // CommitSlow sleeps 3s before serving
	})

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	// Establish a healthy live slot first, so we can prove it survives.
	mustDeploy(t, apiPort, repo.CommitA)

	dr, code := deploy(t, apiPort, repo.CommitSlow)
	if dr.Success {
		t.Fatal("expected the slow app to fail its health check")
	}
	if code == 200 {
		t.Fatalf("expected a non-200 status, got 200")
	}
	if dr.Stage != "probe" {
		t.Fatalf("stage = %q, want %q (error: %s)", dr.Stage, "probe", dr.Error)
	}
	if !strings.Contains(dr.Error, "/healthz") {
		t.Fatalf("the error should name the endpoint that did not pass, got: %s", dr.Error)
	}

	// The live slot is untouched and still serving.
	st := status(t, apiPort)
	if st.LiveCommit != repo.CommitA {
		t.Fatalf("live commit changed after a failed health check: %s", st.LiveCommit)
	}
	statusCode, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", appPort))
	if statusCode != 200 {
		t.Fatalf("the previous version stopped serving: got %d", statusCode)
	}
}
