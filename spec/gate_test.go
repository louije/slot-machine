// Specification tests for the pre-promotion gate and the machine slot.
//
// These cover docs/design.md §5 ("Continuous Validation") and §4 (the branch
// model). Like the rest of spec/, they treat slot-machine as a black box: they
// drive it through its HTTP API and inspect the filesystem it manages.
package spec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gateRepo builds a repo whose HEAD is healthy, then adds one further commit
// containing whatever the caller wants to try to deploy.
func gateRepo(t *testing.T, appBin string, appPort, intPort int) (TestRepo, func(files map[string]string, msg string) string) {
	t.Helper()
	repo := setupTestRepo(t, appBin, appPort, intPort)

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo.Dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	commitWith := func(files map[string]string, msg string) string {
		t.Helper()
		// Start from the known-healthy commit so only the files under test differ.
		git("checkout", "--quiet", "--detach", repo.CommitA)
		for name, content := range files {
			full := filepath.Join(repo.Dir, name)
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
		git("add", "-A")
		git("commit", "-m", msg)
		out := git("rev-parse", "HEAD")
		return trimAll(out)
	}

	return repo, commitWith
}

func trimAll(s string) string {
	out := ""
	for _, r := range s {
		if r != '\n' && r != '\r' && r != ' ' && r != '\t' {
			out += string(r)
		}
	}
	return out
}

// writeGateContract writes a contract with gate settings applied.
func writeGateContract(t *testing.T, dir string, port, internalPort int, extra map[string]any) string {
	t.Helper()

	contract := map[string]any{
		"start_command":     "./start.sh",
		"port":              port,
		"internal_port":     internalPort,
		"health_endpoint":   "/healthz",
		"health_timeout_ms": 8000,
		"drain_timeout_ms":  2000,
		"agent_auth":        "none",
	}
	for k, v := range extra {
		contract[k] = v
	}

	data, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.contract.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// deployAndExpectGateFailure deploys a commit that must be refused, and asserts
// the live slot is untouched afterwards.
func deployAndExpectGateFailure(t *testing.T, apiPort int, commit, wantStage string) DeployResponse {
	t.Helper()

	before := status(t, apiPort)

	dr, code := deploy(t, apiPort, commit)
	if dr.Success {
		t.Fatalf("expected the deploy to be refused, but it succeeded (slot %s)", dr.Slot)
	}
	if code == 200 {
		t.Fatalf("expected a non-200 status for a refused deploy, got 200")
	}
	if dr.Stage != wantStage {
		t.Fatalf("stage = %q, want %q (error: %s)", dr.Stage, wantStage, dr.Error)
	}
	if dr.Error == "" {
		t.Fatal("a refused deploy must explain itself; error was empty")
	}

	after := status(t, apiPort)
	if after.LiveCommit != before.LiveCommit {
		t.Fatalf("the live commit changed on a refused deploy: %s → %s",
			before.LiveCommit, after.LiveCommit)
	}
	return dr
}

// ---------------------------------------------------------------------------
// Gate: protected paths
// ---------------------------------------------------------------------------

func TestGateRejectsProtectedPath(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo, commitWith := gateRepo(t, appBin, appPort, intPort)
	contract := writeGateContract(t, t.TempDir(), appPort, intPort, map[string]any{
		"protected_paths": []string{"config/secrets"},
	})

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	if dr, _ := deploy(t, apiPort, repo.CommitA); !dr.Success {
		t.Fatalf("baseline deploy failed: %s", dr.Error)
	}

	candidate := commitWith(map[string]string{
		"config/secrets/prod.txt": "anything at all\n",
	}, "touch a protected path")

	deployAndExpectGateFailure(t, apiPort, candidate, "gate")
}

// A protected prefix must match on path segments, not on raw string prefixes —
// "config" must not protect "configuration/".
func TestGateProtectedPathMatchesSegments(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo, commitWith := gateRepo(t, appBin, appPort, intPort)
	contract := writeGateContract(t, t.TempDir(), appPort, intPort, map[string]any{
		"protected_paths": []string{"config"},
	})

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	if dr, _ := deploy(t, apiPort, repo.CommitA); !dr.Success {
		t.Fatalf("baseline deploy failed: %s", dr.Error)
	}

	candidate := commitWith(map[string]string{
		"configuration/notes.txt": "not protected\n",
	}, "touch a similarly-named path")

	mustDeploy(t, apiPort, candidate)
}

// ---------------------------------------------------------------------------
// Gate: secrets
// ---------------------------------------------------------------------------

func TestGateRejectsAddedSecret(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo, commitWith := gateRepo(t, appBin, appPort, intPort)
	contract := writeGateContract(t, t.TempDir(), appPort, intPort, nil)

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	if dr, _ := deploy(t, apiPort, repo.CommitA); !dr.Success {
		t.Fatalf("baseline deploy failed: %s", dr.Error)
	}

	// A GitHub token shape, which is one of the built-in patterns.
	candidate := commitWith(map[string]string{
		"deploy.sh": "#!/bin/sh\nexport TOKEN=ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz" + "\n",
	}, "commit a credential by accident")

	deployAndExpectGateFailure(t, apiPort, candidate, "gate")
}

// ---------------------------------------------------------------------------
// Gate: diff size
// ---------------------------------------------------------------------------

func TestGateRejectsOversizedDiff(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo, commitWith := gateRepo(t, appBin, appPort, intPort)
	contract := writeGateContract(t, t.TempDir(), appPort, intPort, map[string]any{
		"max_diff_lines": 10,
	})

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	if dr, _ := deploy(t, apiPort, repo.CommitA); !dr.Success {
		t.Fatalf("baseline deploy failed: %s", dr.Error)
	}

	big := ""
	for i := 0; i < 50; i++ {
		big += fmt.Sprintf("line %d\n", i)
	}
	candidate := commitWith(map[string]string{"notes.txt": big}, "a large change")

	deployAndExpectGateFailure(t, apiPort, candidate, "gate")
}

// ---------------------------------------------------------------------------
// Gate: pre-deploy command
// ---------------------------------------------------------------------------

func TestPreDeployCommandBlocksPromotion(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo, commitWith := gateRepo(t, appBin, appPort, intPort)
	contract := writeGateContract(t, t.TempDir(), appPort, intPort, map[string]any{
		// Passes when the marker is absent, fails when present, so the same
		// daemon can show both outcomes.
		"pre_deploy_command": "test ! -f FAIL_THE_TESTS",
	})

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	if dr, _ := deploy(t, apiPort, repo.CommitA); !dr.Success {
		t.Fatalf("baseline deploy failed: %s", dr.Error)
	}

	candidate := commitWith(map[string]string{
		"FAIL_THE_TESTS": "the app's own checks fail on this commit\n",
	}, "a commit whose tests fail")

	dr := deployAndExpectGateFailure(t, apiPort, candidate, "verify")
	if !containsStr(dr.Error, "pre_deploy_command") {
		t.Fatalf("error should name the failing command, got: %s", dr.Error)
	}
}

// ---------------------------------------------------------------------------
// Gate: silent revert
// ---------------------------------------------------------------------------

// The one divergence case that blocks: deploying a commit whose tree is missing
// files the human branch has would delete them from production with no conflict
// and no error.
func TestGateRejectsSilentRevert(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo, _ := gateRepo(t, appBin, appPort, intPort)
	contract := writeGateContract(t, t.TempDir(), appPort, intPort, nil)

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo.Dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	// Baseline first, while main and the candidate still agree on which files
	// exist. Committing the human's file before this made the *baseline* a
	// silent revert as well, which the gate rightly refused — the test only
	// passed originally because startup left no live commit, so the gate was
	// skipped entirely.
	mustDeploy(t, apiPort, repo.CommitB)

	// Now a human commits a file on main that the candidate will not contain.
	git("checkout", "--quiet", "main")
	if err := os.WriteFile(filepath.Join(repo.Dir, "human-work.txt"), []byte("important\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "human work that must not vanish")

	// CommitA predates the human's file, so promoting it would remove it.
	dr := deployAndExpectGateFailure(t, apiPort, repo.CommitA, "gate")
	if !containsStr(dr.Error, "human-work.txt") {
		t.Fatalf("error should name the file that would be lost, got: %s", dr.Error)
	}
}

// ---------------------------------------------------------------------------
// The machine slot
// ---------------------------------------------------------------------------

func TestMachineSlotIsOnABranch(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	machineDir := filepath.Join(orch.DataDir, "machine")
	if _, err := os.Stat(machineDir); err != nil {
		t.Fatalf("machine slot was not created at %s: %v", machineDir, err)
	}

	// It must be a real branch, not a detached HEAD: commits made here need a
	// ref, or they become unreachable the moment anything moves.
	out, err := exec.Command("git", "-C", machineDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("reading machine slot HEAD: %v", err)
	}
	if got := trimAll(string(out)); got != "machine" {
		t.Fatalf("machine slot is on %q, want the machine branch", got)
	}

	sr := status(t, apiPort)
	if sr.MachineBranch != "machine" {
		t.Fatalf("status reports machine_branch %q", sr.MachineBranch)
	}
}

// A deploy must not disturb the agent's worktree — that separation is what lets
// the agent keep working across a promotion.
func TestDeployLeavesMachineSlotAlone(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	machineDir := filepath.Join(orch.DataDir, "machine")

	// Uncommitted work, exactly what an agent mid-task would have.
	scratch := filepath.Join(machineDir, "work-in-progress.txt")
	if err := os.WriteFile(scratch, []byte("half-finished\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if dr, _ := deploy(t, apiPort, repo.CommitA); !dr.Success {
		t.Fatalf("deploy failed: %s", dr.Error)
	}
	if dr, _ := deploy(t, apiPort, repo.CommitB); !dr.Success {
		t.Fatalf("second deploy failed: %s", dr.Error)
	}

	data, err := os.ReadFile(scratch)
	if err != nil {
		t.Fatalf("the agent's uncommitted work did not survive a deploy: %v", err)
	}
	if string(data) != "half-finished\n" {
		t.Fatalf("the agent's file was modified: %q", data)
	}

	// And the machine slot is still a machine-branch worktree, not a deploy slot.
	out, err := exec.Command("git", "-C", machineDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("reading machine slot HEAD after deploys: %v", err)
	}
	if got := trimAll(string(out)); got != "machine" {
		t.Fatalf("machine slot drifted to %q after deploys", got)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The chat is reachable when nothing works
// ---------------------------------------------------------------------------
//
// The public port is bound by the daemon, not by an app process, so it answers
// before anything has ever deployed successfully. This is the case that matters
// most: an operator whose deploy is broken needs the agent to fix it, and the
// listener used to appear only on a successful deploy — unreachable exactly when
// it was needed.

func TestChatReachableWithNoSuccessfulDeploy(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeGateContract(t, t.TempDir(), appPort, intPort, nil)

	// Put the unhealthy commit at HEAD, so even the daemon's own startup deploy
	// cannot succeed. This is the state an operator is actually in when they
	// need the agent: nothing works.
	gitIn(t, repo.Dir, "checkout", "--quiet", "main")
	gitIn(t, repo.Dir, "reset", "--hard", "--quiet", repo.CommitBad)

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	dr, _ := deploy(t, apiPort, repo.CommitBad)
	if dr.Success {
		t.Fatal("expected the bad commit to fail")
	}

	st := status(t, apiPort)
	if st.LiveCommit != "" {
		t.Fatalf("expected nothing live, got %s", st.LiveCommit)
	}
	if !st.ProxyListening {
		t.Fatal("the public port must be bound even with nothing live")
	}

	// The chat UI is served.
	code, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/chat", appPort))
	if code != 200 {
		t.Fatalf("GET /chat returned %d with nothing live; the agent must stay reachable", code)
	}
	if !containsStr(body, "/chat/config") {
		t.Fatal("/chat did not serve the chat page")
	}

	// And the agent API works, so a conversation can actually be started.
	code, _ = httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/agent/conversations", appPort))
	if code != 200 {
		t.Fatalf("GET /agent/conversations returned %d with nothing live", code)
	}

	// App paths, by contrast, say plainly that there is no live slot.
	code, _ = httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", appPort))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("app path returned %d, want 503 when nothing is live", code)
	}
}

// The API port must be reserved before any deploy allocates its slot ports.
//
// Slot ports come from the ephemeral range. While the API port was still
// unbound, a slot's app process could be handed it — and then `POST /deploy`
// reached the app instead of the daemon, whose reply decoded into an all-zero
// response: success=false with no stage and no error. It surfaced as an
// occasional inexplicable "deploy failed".
func TestAPIPortNotStolenByAppSlots(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeGateContract(t, t.TempDir(), appPort, intPort, nil)

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	// Several deploys, each allocating a fresh pair of dynamic ports.
	for _, c := range []string{repo.CommitA, repo.CommitB, repo.CommitC} {
		mustDeploy(t, apiPort, c)
	}

	// The API port must still be the daemon's, answering the daemon's contract.
	code, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", apiPort))
	if code != 200 {
		t.Fatalf("API port returned %d", code)
	}
	if !containsStr(body, `"status":"ok"`) {
		t.Fatalf("something other than the daemon is on the API port: %q", body)
	}

	// And a deploy response must be a deploy response, not another server's 200.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success && dr.Stage == "" && dr.Error == "" {
		t.Fatal("got an all-zero deploy response: the request did not reach the daemon")
	}
}
