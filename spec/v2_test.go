// v2 specification tests for slot-machine.
//
// These tests validate v2 behaviors that v1 does not support:
// zero-downtime deploys, symlink-based state persistence, staging slot,
// garbage collection, and artifact preservation.
//
// Expected: all FAIL against a v1 implementation (except TestRollbackThenDeploy,
// which validates behavior that v1 already supports).
package spec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// v2 helpers
// ---------------------------------------------------------------------------

type statusV2Response struct {
	LiveSlot       string `json:"live_slot"`
	LiveCommit     string `json:"live_commit"`
	PreviousSlot   string `json:"previous_slot"`
	PreviousCommit string `json:"previous_commit"`
	StagingDir     string `json:"staging_dir"`
	LastDeployTime string `json:"last_deploy_time"`
	Healthy        bool   `json:"healthy"`
}

func statusV2(t *testing.T, apiPort int) statusV2Response {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/status", apiPort))
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	var sr statusV2Response
	json.NewDecoder(resp.Body).Decode(&sr)
	return sr
}

// ---------------------------------------------------------------------------
// Test 18: Zero downtime during deploy
// ---------------------------------------------------------------------------
//
// Deploys commit A, then starts a slow deploy of commit B. While B is
// booting, the public port must continue responding (A still serving).
// v1 drains A before starting B — the port goes down during the gap.

func TestZeroDowntime(t *testing.T) {
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
		t.Fatal("deploy A failed")
	}
	waitForHealth(t, appPort, 5*time.Second)

	// Start deploying the slow commit (3s boot delay) asynchronously.
	slowResult := deployAsync(t, apiPort, repo.CommitSlow)

	// Wait for the deploy to be in progress (v1 will have drained A by now).
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
//
// After a deploy, GET /status must include a staging_dir field pointing to
// the workspace directory. v1 has no staging concept.

func TestStatusIncludesStagingDir(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

	st := statusV2(t, apiPort)
	if st.StagingDir == "" {
		t.Fatal("expected staging_dir in status response, got empty string")
	}
}

// ---------------------------------------------------------------------------
// Test 20: Staging preserves artifacts from promoted slot
// ---------------------------------------------------------------------------
//
// After a deploy with a setup_command that creates a file, the new staging
// directory should contain that file (inherited via CoW clone of the
// promoted slot). v1 has no staging directory.

func TestStagingPreservesArtifacts(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)

	contractDir := t.TempDir()
	cfg := map[string]any{
		"start_command":     "./start.sh",
		"setup_command":     "touch .setup-marker",
		"port":              appPort,
		"internal_port":     intPort,
		"health_endpoint":   "/healthz",
		"health_timeout_ms": 5000,
		"drain_timeout_ms":  10000,
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	contractPath := filepath.Join(contractDir, "app.contract.json")
	os.WriteFile(contractPath, data, 0644)

	orch := startOrchestrator(t, bin, contractPath, repo.Dir, apiPort)

	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

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
//
// After deploys, the data directory must contain `live` and `prev` symlinks
// pointing to the correct slot directories. v1 uses in-memory state only.

func TestSymlinksOnDisk(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)

	// Deploy A.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy A failed")
	}

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
//
// After deploying and restarting the daemon (same data dir), the status
// must still show the previously deployed commit. v1 stores all state in
// memory — after restart, it forgets everything.

func TestDaemonRestart(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

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
	cmd1 := startDaemon()
	waitForHealth(t, apiPort, 5*time.Second)

	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}
	st := status(t, apiPort)
	if st.LiveCommit != repo.CommitA {
		t.Fatalf("expected live_commit=%s, got %s", repo.CommitA, st.LiveCommit)
	}

	// Stop daemon.
	stopDaemon(cmd1)
	time.Sleep(500 * time.Millisecond)

	// Second run: same data dir — state should persist.
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
// After three deploys (A → B → Slow), the first deploy's slot directory
// should be garbage collected. Only live + prev slot dirs remain.
// v1 uses fixed slot names (slot-a, slot-b) and never GCs.

func TestGarbageCollection(t *testing.T) {
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)

	// Deploy A.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy A failed")
	}

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

	// Deploy Slow (third deploy triggers GC of A).
	dr, _ = deploy(t, apiPort, repo.CommitSlow)
	if !dr.Success {
		t.Fatal("deploy Slow failed")
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
// This tests the full cycle and should pass on both v1 and v2.

func TestRollbackThenDeploy(t *testing.T) {
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

	// Rollback to A.
	rr, code := rollback(t, apiPort)
	if code != 200 || !rr.Success {
		t.Fatalf("rollback failed: code=%d", code)
	}
	st := status(t, apiPort)
	if st.LiveCommit != repo.CommitA {
		t.Fatalf("after rollback: expected live=%s, got %s", repo.CommitA, st.LiveCommit)
	}

	// Deploy Slow — should work after rollback.
	dr, _ = deploy(t, apiPort, repo.CommitSlow)
	if !dr.Success {
		t.Fatal("deploy after rollback failed")
	}
	st = status(t, apiPort)
	if st.LiveCommit != repo.CommitSlow {
		t.Fatalf("after post-rollback deploy: expected live=%s, got %s", repo.CommitSlow, st.LiveCommit)
	}
}

// TestDynamicPorts is not included separately — TestZeroDowntime already
// validates that two slot processes can run simultaneously (which requires
// dynamic port assignment). A dedicated test would need access to the
// internal port assignments, which are not exposed in the current API.
