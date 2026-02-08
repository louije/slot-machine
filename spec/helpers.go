// Helpers for the slot-machine specification tests.
//
// These functions set up git repos, start/stop the binary under test, and make
// HTTP calls to verify behavior. The implementation is a black box — we only
// interact with it through its HTTP API.
package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// Orchestrator holds a handle to a running orchestrator subprocess.
type Orchestrator struct {
	Cmd     *exec.Cmd
	APIPort int
	DataDir string
}

// DeployResponse matches the JSON returned by POST /deploy.
type DeployResponse struct {
	Success        bool   `json:"success"`
	Slot           string `json:"slot"`
	Commit         string `json:"commit"`
	PreviousCommit string `json:"previous_commit"`
}

// RollbackResponse matches the JSON returned by POST /rollback.
type RollbackResponse struct {
	Success bool   `json:"success"`
	Slot    string `json:"slot"`
	Commit  string `json:"commit"`
}

// StatusResponse matches the JSON returned by GET /status.
type StatusResponse struct {
	LiveSlot       string `json:"live_slot"`
	LiveCommit     string `json:"live_commit"`
	PreviousSlot   string `json:"previous_slot"`
	PreviousCommit string `json:"previous_commit"`
	StagingDir     string `json:"staging_dir"`
	LastDeployTime string `json:"last_deploy_time"`
	Healthy        bool   `json:"healthy"`
}

// ---------------------------------------------------------------------------
// Port allocation
// ---------------------------------------------------------------------------

// freePort finds an available TCP port by binding to :0 and immediately closing.
// The OS assigns an ephemeral port which is then released for use.
//
// Deprecated: use reservePorts instead to avoid port collisions between
// parallel tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// reservePorts allocates n TCP ports and keeps the listeners open to prevent
// other parallel tests from getting the same ports. Call the returned release
// function immediately before starting the subprocess that needs to bind.
func reservePorts(t *testing.T, n int) (ports []int, release func()) {
	t.Helper()
	listeners := make([]net.Listener, n)
	ports = make([]int, n)
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for j := 0; j < i; j++ {
				listeners[j].Close()
			}
			t.Fatalf("reservePorts: %v", err)
		}
		listeners[i] = l
		ports[i] = l.Addr().(*net.TCPAddr).Port
	}
	return ports, func() {
		for _, l := range listeners {
			l.Close()
		}
	}
}

// ---------------------------------------------------------------------------
// Git setup
// ---------------------------------------------------------------------------

// TestRepo holds paths and commit hashes for a test git repository.
type TestRepo struct {
	Dir        string // path to the git repo
	CommitA    string // first "good" commit
	CommitB    string // second "good" commit
	CommitC    string // third "good" commit
	CommitBad  string // commit where the app starts unhealthy
	CommitSlow string // commit where the app has a 3-second boot delay
}

// setupTestRepo creates a temp directory, initializes a git repo, and makes
// several commits with different start scripts wrapping the testapp binary.
//
// testappBin is the absolute path to the compiled testapp binary. The function
// copies it into the repo and creates start scripts that invoke it with various
// flags. Each commit represents a different "version" of the app.
//
// The returned TestRepo has four commit hashes the tests can deploy.
func setupTestRepo(t *testing.T, testappBin string, appPort, internalPort int) TestRepo {
	t.Helper()

	dir := t.TempDir() // automatically cleaned up when the test ends

	// Helper to run git commands in the repo dir.
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return string(bytes.TrimSpace(out))
	}

	git("init")
	git("checkout", "-b", "main")
	// Disable GPG signing — the test machine may have it enabled globally.
	git("config", "commit.gpgsign", "false")

	// Copy the testapp binary into the repo.
	srcBin, err := os.ReadFile(testappBin)
	if err != nil {
		t.Fatalf("reading testapp binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testapp"), srcBin, 0755); err != nil {
		t.Fatalf("writing testapp binary: %v", err)
	}

	// Helper to write a start script and commit.
	writeStartAndCommit := func(scriptContent, message string) string {
		t.Helper()
		scriptPath := filepath.Join(dir, "start.sh")
		if err := os.WriteFile(scriptPath, []byte(scriptContent), 0755); err != nil {
			t.Fatalf("writing start.sh: %v", err)
		}
		git("add", "-A")
		git("commit", "-m", message)
		return git("rev-parse", "HEAD")
	}

	// Start scripts don't hardcode ports — the orchestrator injects PORT and
	// INTERNAL_PORT as env vars, just like systemd's EnvironmentFile would for
	// a real Bun/Flask app. Only behavior flags go in the start script.

	// Commit A — normal healthy app.
	commitA := writeStartAndCommit(
		"#!/bin/sh\nexec ./testapp\n",
		"commit A: healthy app",
	)

	// Commit B — also healthy, just a different commit hash.
	commitB := writeStartAndCommit(
		"#!/bin/sh\n# version B\nexec ./testapp\n",
		"commit B: healthy app v2",
	)

	// Commit C — third healthy variant.
	commitC := writeStartAndCommit(
		"#!/bin/sh\n# version C\nexec ./testapp\n",
		"commit C: healthy app v3",
	)

	// Commit Bad — starts unhealthy (health check returns 503).
	commitBad := writeStartAndCommit(
		"#!/bin/sh\nexec ./testapp --start-unhealthy\n",
		"commit Bad: unhealthy app",
	)

	// Commit Slow — 3-second boot delay before serving.
	commitSlow := writeStartAndCommit(
		"#!/bin/sh\nexec ./testapp --boot-delay 3\n",
		"commit Slow: slow-booting app",
	)

	return TestRepo{
		Dir:        dir,
		CommitA:    commitA,
		CommitB:    commitB,
		CommitC:    commitC,
		CommitBad:  commitBad,
		CommitSlow: commitSlow,
	}
}

// ---------------------------------------------------------------------------
// Contract config
// ---------------------------------------------------------------------------

// writeTestContract writes a minimal app.contract.json and returns its path.
// This is the contract file the orchestrator reads to know how to manage the app.
func writeTestContract(t *testing.T, dir string, port, internalPort, drainTimeoutMs int) string {
	t.Helper()

	if drainTimeoutMs == 0 {
		drainTimeoutMs = 2000 // default 2s (enough for graceful shutdown)
	}

	contract := map[string]any{
		"start_command":     "./start.sh",
		"port":              port,
		"internal_port":     internalPort,
		"health_endpoint":   "/healthz",
		"health_timeout_ms": 3000,
		"drain_timeout_ms":  drainTimeoutMs,
		"agent_auth":        "none",
	}

	data, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		t.Fatalf("marshaling contract: %v", err)
	}

	path := filepath.Join(dir, "app.contract.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("writing contract: %v", err)
	}

	return path
}

// ---------------------------------------------------------------------------
// Orchestrator lifecycle
// ---------------------------------------------------------------------------

// orchestratorBinary returns the path to the orchestrator binary under test.
// It reads the ORCHESTRATOR_BIN environment variable. Tests are skipped if unset.
func orchestratorBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("ORCHESTRATOR_BIN")
	if bin == "" {
		t.Skip("ORCHESTRATOR_BIN not set — skipping integration test")
	}
	// Resolve to absolute path.
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatalf("resolving ORCHESTRATOR_BIN: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("ORCHESTRATOR_BIN not found at %s: %v", abs, err)
	}
	return abs
}

// startOrchestrator launches the orchestrator binary as a subprocess and waits
// until its HTTP API is reachable. Returns a handle for stopping it later.
//
// If release is non-nil, it is called immediately before starting the process
// to free reserved ports (see reservePorts).
//
// The orchestrator is started with:
//
//	--config <contractPath> --repo <repoDir> --data <tempDataDir> --port <apiPort> --no-proxy
func startOrchestrator(t *testing.T, binary, contractPath, repoDir string, apiPort int, release func()) *Orchestrator {
	t.Helper()

	dataDir := t.TempDir()

	cmd := exec.Command(binary,
		"start",
		"--config", contractPath,
		"--repo", repoDir,
		"--data", dataDir,
		"--port", fmt.Sprintf("%d", apiPort),
		"--no-proxy",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if release != nil {
		release()
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting orchestrator: %v", err)
	}

	orch := &Orchestrator{
		Cmd:     cmd,
		APIPort: apiPort,
		DataDir: dataDir,
	}

	// t.Cleanup registers a function to run when the test ends — like
	// Ruby's after(:each). This ensures the orchestrator is always stopped.
	t.Cleanup(func() {
		stopOrchestrator(t, orch)
	})

	// Wait for the API to become reachable (up to 5 seconds).
	waitForHealth(t, apiPort, 5*time.Second)

	return orch
}

// stopOrchestrator sends SIGTERM and waits briefly. If the process doesn't exit,
// it sends SIGKILL. Errors are not fatal — the process may already be dead.
func stopOrchestrator(t *testing.T, orch *Orchestrator) {
	t.Helper()
	if orch.Cmd.Process == nil {
		return
	}

	// Send SIGTERM (graceful shutdown).
	_ = orch.Cmd.Process.Signal(syscall.SIGTERM)

	// Wait up to 3 seconds for exit.
	done := make(chan error, 1)
	go func() {
		done <- orch.Cmd.Wait()
	}()

	select {
	case <-done:
		// Process exited.
	case <-time.After(3 * time.Second):
		// Force kill.
		_ = orch.Cmd.Process.Signal(syscall.SIGKILL)
		<-done
	}
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

// deploy sends POST /deploy {"commit": commit} to the orchestrator API.
// Returns the decoded response and the HTTP status code.
func deploy(t *testing.T, apiPort int, commit string) (DeployResponse, int) {
	t.Helper()

	body, _ := json.Marshal(map[string]string{"commit": commit})
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/deploy", apiPort),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST /deploy: %v", err)
	}
	defer resp.Body.Close()

	var dr DeployResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		t.Fatalf("decoding deploy response: %v", err)
	}
	return dr, resp.StatusCode
}

// AsyncDeployResult holds the outcome of an asynchronous deploy call.
type AsyncDeployResult struct {
	Resp   DeployResponse
	Status int
	Err    error
}

// deployAsync is like deploy but doesn't block waiting for the response.
// It returns a channel that will receive the result. This is useful for testing
// concurrent deploy rejection (test 6).
func deployAsync(t *testing.T, apiPort int, commit string) <-chan AsyncDeployResult {
	t.Helper()

	ch := make(chan AsyncDeployResult, 1)

	go func() {
		body, _ := json.Marshal(map[string]string{"commit": commit})
		resp, err := http.Post(
			fmt.Sprintf("http://127.0.0.1:%d/deploy", apiPort),
			"application/json",
			bytes.NewReader(body),
		)
		if err != nil {
			ch <- AsyncDeployResult{Err: err}
			return
		}
		defer resp.Body.Close()

		var dr DeployResponse
		if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
			ch <- AsyncDeployResult{Err: err}
			return
		}
		ch <- AsyncDeployResult{Resp: dr, Status: resp.StatusCode}
	}()

	return ch
}

// rollback sends POST /rollback to the orchestrator API.
func rollback(t *testing.T, apiPort int) (RollbackResponse, int) {
	t.Helper()

	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/rollback", apiPort),
		"application/json",
		nil,
	)
	if err != nil {
		t.Fatalf("POST /rollback: %v", err)
	}
	defer resp.Body.Close()

	var rr RollbackResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("decoding rollback response: %v", err)
	}
	return rr, resp.StatusCode
}

// status sends GET /status to the orchestrator API.
func status(t *testing.T, apiPort int) StatusResponse {
	t.Helper()

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/status", apiPort))
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	var sr StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decoding status response: %v", err)
	}
	return sr
}

// waitForHealth polls a port until it responds with HTTP 200 or the timeout
// expires. Used to wait for both the orchestrator API and the testapp to come up.
func waitForHealth(t *testing.T, port int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("port %d did not respond 200 within %v", port, timeout)
}

// waitForDown polls a port until it stops responding. Used to verify a process
// was killed.
func waitForDown(t *testing.T, port int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			return // connection refused — port is down
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("port %d still responding after %v", port, timeout)
}

// httpGet sends a GET to the given URL and returns the status code and body.
func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// testagentBinary returns the absolute path to the compiled testagent binary.
func testagentBinary(t *testing.T) string {
	t.Helper()

	candidates := []string{
		"testagent/testagent",
		"spec/testagent/testagent",
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
	t.Fatal("testagent binary not found — run: go build -o spec/testagent/testagent ./spec/testagent/")
	return ""
}

// startOrchestratorWithAgent launches the orchestrator with the agent binary
// available via SLOT_MACHINE_AGENT_BIN env var. Used for agent/deploy-through tests.
//
// If release is non-nil, it is called immediately before starting the process.
func startOrchestratorWithAgent(t *testing.T, binary, contractPath, repoDir string, apiPort int, agentBin string, release func()) *Orchestrator {
	t.Helper()

	dataDir := t.TempDir()

	cmd := exec.Command(binary,
		"start",
		"--config", contractPath,
		"--repo", repoDir,
		"--data", dataDir,
		"--port", fmt.Sprintf("%d", apiPort),
		"--no-proxy",
	)
	cmd.Env = append(os.Environ(), "SLOT_MACHINE_AGENT_BIN="+agentBin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if release != nil {
		release()
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting orchestrator: %v", err)
	}

	orch := &Orchestrator{
		Cmd:     cmd,
		APIPort: apiPort,
		DataDir: dataDir,
	}

	t.Cleanup(func() {
		stopOrchestrator(t, orch)
	})

	waitForHealth(t, apiPort, 5*time.Second)

	return orch
}

// writeTestContractWithAuth is like writeTestContract but allows specifying the auth mode.
func writeTestContractWithAuth(t *testing.T, dir string, port, internalPort, drainTimeoutMs int, authMode string) string {
	t.Helper()

	if drainTimeoutMs == 0 {
		drainTimeoutMs = 2000
	}

	contract := map[string]any{
		"start_command":     "./start.sh",
		"port":              port,
		"internal_port":     internalPort,
		"health_endpoint":   "/healthz",
		"health_timeout_ms": 3000,
		"drain_timeout_ms":  drainTimeoutMs,
		"agent_auth":        authMode,
	}

	data, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		t.Fatalf("marshaling contract: %v", err)
	}

	path := filepath.Join(dir, "app.contract.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("writing contract: %v", err)
	}

	return path
}

// httpPost sends a POST to the given URL and returns the status code.
func httpPost(t *testing.T, url string) int {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(url, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
