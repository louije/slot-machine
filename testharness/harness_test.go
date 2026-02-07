// Integration tests for the orchestrator spec.
//
// These 8 scenarios validate any implementation of the orchestrator against the
// spec in orchestrator-spec.md. The orchestrator binary is a black box — we only
// interact with it through its HTTP API.
//
// Run:
//   go build -o testharness/testapp/testapp ./testharness/testapp/
//   go build -o slot-machine ./cmd/slot-machine/
//   ORCHESTRATOR_BIN=$(pwd)/slot-machine go test -v -count=1 ./testharness/
//
// Each test gets its own git repo, contract, data dir, and orchestrator instance.
// Nothing is shared between tests.
package testharness

import (
	"fmt"
	"os"
	"path/filepath"
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
	// When running `go test ./testharness/`, the working dir is testharness/.
	candidates := []string{
		"testapp/testapp",
		"testharness/testapp/testapp",
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
	t.Fatal("testapp binary not found — run: go build -o testharness/testapp/testapp ./testharness/testapp/")
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
