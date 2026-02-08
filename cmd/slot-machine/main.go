// slot-machine — blue-green deploys on a single machine.
//
// Usage:
//
//	slot-machine init                  # scaffold slot-machine.json + update .gitignore
//	slot-machine start [flags]         # start daemon, auto-deploy HEAD
//	slot-machine deploy [commit]       # tell running daemon to deploy (defaults to HEAD)
//	slot-machine rollback              # tell running daemon to rollback
//	slot-machine status                # get status from running daemon
//
// Build:
//
//	go build -o slot-machine ./cmd/slot-machine/
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const specVersion = "2" // spec/VERSION

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type config struct {
	SetupCommand    string `json:"setup_command"`
	StartCommand    string `json:"start_command"`
	Port            int    `json:"port"`
	InternalPort    int    `json:"internal_port"`
	HealthEndpoint  string `json:"health_endpoint"`
	HealthTimeoutMs int    `json:"health_timeout_ms"`
	DrainTimeoutMs  int    `json:"drain_timeout_ms"`
	EnvFile         string `json:"env_file"`
	APIPort         int    `json:"api_port"`
}

type slot struct {
	name    string // directory basename, e.g. "slot-abc1234"
	commit  string
	dir     string // absolute path
	cmd     *exec.Cmd
	done    chan struct{}
	alive   bool
	appPort int // dynamic
	intPort int // dynamic
}

type orchestrator struct {
	cfg     config
	repoDir string
	dataDir string

	mu         sync.Mutex
	deploying  bool
	liveSlot   *slot
	prevSlot   *slot
	lastDeploy time.Time

	appProxy *dynamicProxy // proxies config.Port → live slot's appPort
	intProxy *dynamicProxy // proxies config.InternalPort → live slot's intPort
}

// ---------------------------------------------------------------------------
// Dynamic reverse proxy
// ---------------------------------------------------------------------------

type dynamicProxy struct {
	mu   sync.RWMutex
	port int
	addr string
	srv  *http.Server
}

func newDynamicProxy(addr string) *dynamicProxy {
	return &dynamicProxy{addr: addr}
}

func (p *dynamicProxy) setTarget(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.port = port
	if port > 0 && p.srv == nil && p.addr != "" {
		ln, err := net.Listen("tcp", p.addr)
		if err != nil {
			return
		}
		p.srv = &http.Server{Handler: http.HandlerFunc(p.serveHTTP)}
		go p.srv.Serve(ln)
	}
}

func (p *dynamicProxy) clearTarget() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.port = 0
	if p.srv != nil {
		p.srv.Close()
		p.srv = nil
	}
}

func (p *dynamicProxy) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.port = 0
	if p.srv != nil {
		p.srv.Shutdown(context.Background())
		p.srv = nil
	}
}

func (p *dynamicProxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	port := p.port
	p.mu.RUnlock()

	if port == 0 {
		http.Error(w, "no live slot", http.StatusServiceUnavailable)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = fmt.Sprintf("127.0.0.1:%d", port)
		},
	}
	proxy.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// HTTP API
// ---------------------------------------------------------------------------

func (o *orchestrator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "GET" && r.URL.Path == "/":
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))

	case r.Method == "POST" && r.URL.Path == "/deploy":
		o.handleDeploy(w, r)

	case r.Method == "POST" && r.URL.Path == "/rollback":
		o.handleRollback(w, r)

	case r.Method == "GET" && r.URL.Path == "/status":
		o.handleStatus(w, r)

	default:
		http.NotFound(w, r)
	}
}

// --- POST /deploy ---

type deployRequest struct {
	Commit string `json:"commit"`
}

type deployResponse struct {
	Success        bool   `json:"success"`
	Slot           string `json:"slot"`
	Commit         string `json:"commit"`
	PreviousCommit string `json:"previous_commit"`
	Error          string `json:"error,omitempty"`
}

func (o *orchestrator) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Commit == "" {
		writeJSON(w, 400, deployResponse{Error: "missing commit"})
		return
	}

	resp, code := o.doDeploy(req.Commit)
	writeJSON(w, code, resp)
}

// --- POST /rollback ---

type rollbackResponse struct {
	Success bool   `json:"success"`
	Slot    string `json:"slot"`
	Commit  string `json:"commit"`
	Error   string `json:"error,omitempty"`
}

func (o *orchestrator) handleRollback(w http.ResponseWriter, r *http.Request) {
	resp, code := o.doRollback()
	writeJSON(w, code, resp)
}

// --- GET /status ---

type statusResponse struct {
	LiveSlot       string `json:"live_slot"`
	LiveCommit     string `json:"live_commit"`
	PreviousSlot   string `json:"previous_slot"`
	PreviousCommit string `json:"previous_commit"`
	StagingDir     string `json:"staging_dir"`
	LastDeployTime string `json:"last_deploy_time"`
	Healthy        bool   `json:"healthy"`
}

func (o *orchestrator) handleStatus(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()

	resp := statusResponse{
		StagingDir: "slot-staging",
	}

	if o.liveSlot != nil {
		resp.LiveSlot = o.liveSlot.name
		resp.LiveCommit = o.liveSlot.commit
		resp.Healthy = o.liveSlot.alive
	}
	if o.prevSlot != nil {
		resp.PreviousSlot = o.prevSlot.name
		resp.PreviousCommit = o.prevSlot.commit
	}
	if !o.lastDeploy.IsZero() {
		resp.LastDeployTime = o.lastDeploy.Format(time.RFC3339)
	}

	writeJSON(w, 200, resp)
}

// ---------------------------------------------------------------------------
// Deploy logic (v2: start-before-drain, dynamic ports)
// ---------------------------------------------------------------------------

func (o *orchestrator) doDeploy(commit string) (deployResponse, int) {
	o.mu.Lock()
	if o.deploying {
		o.mu.Unlock()
		return deployResponse{Error: "deploy in progress"}, 409
	}
	o.deploying = true
	oldLive := o.liveSlot
	oldPrev := o.prevSlot
	o.mu.Unlock()

	defer func() {
		o.mu.Lock()
		o.deploying = false
		o.mu.Unlock()
	}()

	stagingDir := filepath.Join(o.dataDir, "slot-staging")

	// 1. Checkout commit in staging.
	if err := o.prepareSlot(stagingDir, commit); err != nil {
		return deployResponse{Error: err.Error()}, 500
	}

	// 2. Run setup command.
	appPort, err := findFreePort()
	if err != nil {
		return deployResponse{Error: "free port: " + err.Error()}, 500
	}
	intPort, err := findFreePort()
	if err != nil {
		return deployResponse{Error: "free port: " + err.Error()}, 500
	}

	if o.cfg.SetupCommand != "" {
		if err := o.runSetup(stagingDir, appPort, intPort); err != nil {
			return deployResponse{Error: "setup: " + err.Error()}, 500
		}
	}

	// 3. Start process with dynamic ports.
	newSlot, err := o.startProcess(stagingDir, commit, appPort, intPort)
	if err != nil {
		return deployResponse{Error: "start: " + err.Error()}, 500
	}

	// 4. Health check (old live still serving through proxy).
	if !o.healthCheck(newSlot) {
		syscall.Kill(-newSlot.cmd.Process.Pid, syscall.SIGKILL)
		<-newSlot.done
		return deployResponse{}, 200
	}

	// 5. Healthy — promote.
	slotName := fmt.Sprintf("slot-%s", commit[:8])
	slotDir := filepath.Join(o.dataDir, slotName)

	// GC old prev first (avoid name collision if re-deploying same commit).
	if oldPrev != nil {
		o.drain(oldPrev)
		o.removeWorktree(oldPrev.dir)
	}

	// Rename staging → slot-<hash>.
	if err := o.promoteStaging(stagingDir, slotDir); err != nil {
		// Non-fatal: process is running from stagingDir, just use that path.
		slotDir = stagingDir
		slotName = "slot-staging"
	}
	newSlot.dir = slotDir
	newSlot.name = slotName

	// Switch proxy to new slot.
	o.appProxy.setTarget(appPort)
	o.intProxy.setTarget(intPort)

	// Update state BEFORE draining — prevents crash callback from clearing proxy.
	prevCommit := ""
	o.mu.Lock()
	if oldLive != nil {
		prevCommit = oldLive.commit
	}
	o.prevSlot = oldLive
	o.liveSlot = newSlot
	o.lastDeploy = time.Now()
	o.mu.Unlock()

	// Drain old live (it was still serving until proxy switch above).
	if oldLive != nil {
		o.drain(oldLive)
	}

	// Update symlinks.
	atomicSymlink(filepath.Join(o.dataDir, "live"), slotName)
	if oldLive != nil {
		atomicSymlink(filepath.Join(o.dataDir, "prev"), oldLive.name)
	}

	// Create new staging (CoW clone of promoted slot).
	o.createStaging(slotDir, commit)

	// Journal (best-effort).
	o.appendJournal("deploy", commit, slotName, prevCommit)

	return deployResponse{
		Success:        true,
		Slot:           slotName,
		Commit:         commit,
		PreviousCommit: prevCommit,
	}, 200
}

// ---------------------------------------------------------------------------
// Rollback logic
// ---------------------------------------------------------------------------

func (o *orchestrator) doRollback() (rollbackResponse, int) {
	o.mu.Lock()
	if o.deploying {
		o.mu.Unlock()
		return rollbackResponse{Error: "deploy in progress"}, 409
	}
	if o.prevSlot == nil {
		o.mu.Unlock()
		return rollbackResponse{Error: "no previous slot"}, 400
	}
	o.deploying = true
	oldLive := o.liveSlot
	prev := o.prevSlot
	o.mu.Unlock()

	defer func() {
		o.mu.Lock()
		o.deploying = false
		o.mu.Unlock()
	}()

	// Start prev slot with fresh dynamic ports.
	appPort, err := findFreePort()
	if err != nil {
		return rollbackResponse{Error: "free port: " + err.Error()}, 500
	}
	intPort, err := findFreePort()
	if err != nil {
		return rollbackResponse{Error: "free port: " + err.Error()}, 500
	}

	newSlot, err := o.startProcess(prev.dir, prev.commit, appPort, intPort)
	if err != nil {
		return rollbackResponse{Error: "start: " + err.Error()}, 500
	}

	if !o.healthCheck(newSlot) {
		syscall.Kill(-newSlot.cmd.Process.Pid, syscall.SIGKILL)
		<-newSlot.done
		return rollbackResponse{Error: "health check failed"}, 500
	}

	// Switch proxy.
	o.appProxy.setTarget(appPort)
	o.intProxy.setTarget(intPort)

	// Update state BEFORE draining — prevents crash callback from clearing proxy.
	newSlot.name = prev.name
	o.mu.Lock()
	o.liveSlot = newSlot
	o.prevSlot = oldLive
	o.lastDeploy = time.Now()
	o.mu.Unlock()

	// Drain old live.
	if oldLive != nil {
		o.drain(oldLive)
	}

	// Update symlinks.
	atomicSymlink(filepath.Join(o.dataDir, "live"), prev.name)
	if oldLive != nil {
		atomicSymlink(filepath.Join(o.dataDir, "prev"), oldLive.name)
	}

	// Create new staging.
	o.createStaging(prev.dir, prev.commit)

	return rollbackResponse{
		Success: true,
		Slot:    prev.name,
		Commit:  prev.commit,
	}, 200
}

// ---------------------------------------------------------------------------
// Process management
// ---------------------------------------------------------------------------

func findFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

func (o *orchestrator) runSetup(dir string, appPort, intPort int) error {
	cmd := exec.Command("/bin/sh", "-c", o.cfg.SetupCommand)
	cmd.Dir = dir
	cmd.Env = o.buildEnv(appPort, intPort)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (o *orchestrator) buildEnv(appPort, intPort int) []string {
	env := os.Environ()
	if o.cfg.EnvFile != "" {
		if extra, err := loadEnvFile(o.cfg.EnvFile); err == nil {
			env = append(env, extra...)
		}
	}
	env = append(env,
		"SLOT_MACHINE=1",
		fmt.Sprintf("PORT=%d", appPort),
		fmt.Sprintf("INTERNAL_PORT=%d", intPort),
	)
	return env
}

func (o *orchestrator) startProcess(dir, commit string, appPort, intPort int) (*slot, error) {
	cmd := exec.Command("/bin/sh", "-c", o.cfg.StartCommand)
	cmd.Dir = dir
	cmd.Env = o.buildEnv(appPort, intPort)
	logPath := filepath.Join(o.dataDir, fmt.Sprintf("%s.log", filepath.Base(dir)))
	if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	s := &slot{
		name:    filepath.Base(dir),
		commit:  commit,
		dir:     dir,
		cmd:     cmd,
		done:    make(chan struct{}),
		alive:   true,
		appPort: appPort,
		intPort: intPort,
	}

	go func() {
		cmd.Wait()
		o.mu.Lock()
		s.alive = false
		if o.liveSlot == s {
			o.appProxy.clearTarget()
			o.intProxy.clearTarget()
		}
		o.mu.Unlock()
		close(s.done)
	}()

	return s, nil
}

func (o *orchestrator) drainAll() {
	o.mu.Lock()
	var slots []*slot
	if o.liveSlot != nil {
		slots = append(slots, o.liveSlot)
	}
	if o.prevSlot != nil && o.prevSlot.cmd != nil {
		slots = append(slots, o.prevSlot)
	}
	o.mu.Unlock()
	for _, s := range slots {
		o.drain(s)
	}
}

func (o *orchestrator) drain(s *slot) {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}

	syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)

	select {
	case <-s.done:
	case <-time.After(time.Duration(o.cfg.DrainTimeoutMs) * time.Millisecond):
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}
}

func (o *orchestrator) healthCheck(s *slot) bool {
	timeout := time.Duration(o.cfg.HealthTimeoutMs) * time.Millisecond
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d%s", s.intPort, o.cfg.HealthEndpoint)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			return false
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// Git worktree management
// ---------------------------------------------------------------------------

func (o *orchestrator) prepareSlot(slotDir, commit string) error {
	if _, err := os.Stat(filepath.Join(slotDir, ".git")); err == nil {
		cmd := exec.Command("git", "checkout", "--force", "--detach", commit)
		cmd.Dir = slotDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git checkout in worktree: %s: %w", out, err)
		}
		return nil
	}

	os.RemoveAll(slotDir)
	exec.Command("git", "-C", o.repoDir, "worktree", "prune").Run()

	cmd := exec.Command("git", "-C", o.repoDir, "worktree", "add", "--detach", slotDir, commit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add: %s: %w", out, err)
	}
	return nil
}

// promoteStaging renames slot-staging → slot-<hash> and repairs git worktree metadata.
func (o *orchestrator) promoteStaging(oldDir, newDir string) error {
	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}

	// Read .git file to find the worktree metadata dir.
	gitFile := filepath.Join(newDir, ".git")
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return err
	}

	metaDir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))

	// Update gitdir in metadata to point to new location.
	absNewGit, _ := filepath.Abs(filepath.Join(newDir, ".git"))
	os.WriteFile(filepath.Join(metaDir, "gitdir"), []byte(absNewGit+"\n"), 0644)

	// Rename metadata dir to match new slot name.
	newName := filepath.Base(newDir)
	newMetaDir := filepath.Join(filepath.Dir(metaDir), newName)
	if metaDir != newMetaDir {
		os.Rename(metaDir, newMetaDir)
		// Update .git file to point to renamed metadata dir.
		absNewMeta, _ := filepath.Abs(newMetaDir)
		os.WriteFile(gitFile, []byte("gitdir: "+absNewMeta+"\n"), 0644)
	}

	return nil
}

// createStaging creates a new slot-staging directory by cloning the promoted slot.
func (o *orchestrator) createStaging(srcDir, commit string) {
	dstDir := filepath.Join(o.dataDir, "slot-staging")

	// Try CoW clone (macOS APFS).
	cpCmd := exec.Command("cp", "-c", "-R", srcDir, dstDir)
	if err := cpCmd.Run(); err == nil {
		// Fix git worktree metadata for the clone.
		if o.fixClonedWorktree(dstDir, commit) == nil {
			return
		}
		// Clone metadata repair failed — remove and fall back.
		os.RemoveAll(dstDir)
	}

	// Fallback: fresh worktree.
	exec.Command("git", "-C", o.repoDir, "worktree", "prune").Run()
	exec.Command("git", "-C", o.repoDir, "worktree", "add", "--detach", dstDir, commit).Run()
}

// fixClonedWorktree sets up proper git worktree metadata for a cloned directory.
func (o *orchestrator) fixClonedWorktree(wtDir, commit string) error {
	gitFile := filepath.Join(wtDir, ".git")
	os.Remove(gitFile)

	// Find repo's .git directory.
	repoGitDir := filepath.Join(o.repoDir, ".git")

	// Ensure it's a directory (not a worktree .git file).
	info, err := os.Stat(repoGitDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("repo .git is not a directory")
	}

	wtName := "slot-staging"
	metaDir := filepath.Join(repoGitDir, "worktrees", wtName)

	os.RemoveAll(metaDir)
	os.MkdirAll(metaDir, 0755)

	absWtDir, _ := filepath.Abs(wtDir)
	absGitFile := filepath.Join(absWtDir, ".git")
	absMetaDir, _ := filepath.Abs(metaDir)

	// Write metadata files.
	os.WriteFile(filepath.Join(metaDir, "HEAD"), []byte(commit+"\n"), 0644)
	os.WriteFile(filepath.Join(metaDir, "commondir"), []byte("../..\n"), 0644)
	os.WriteFile(filepath.Join(metaDir, "gitdir"), []byte(absGitFile+"\n"), 0644)

	// Write .git file in worktree.
	os.WriteFile(gitFile, []byte("gitdir: "+absMetaDir+"\n"), 0644)

	return nil
}

func (o *orchestrator) removeWorktree(dir string) {
	cmd := exec.Command("git", "-C", o.repoDir, "worktree", "remove", "--force", dir)
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dir)
		exec.Command("git", "-C", o.repoDir, "worktree", "prune").Run()
	}
}

// ---------------------------------------------------------------------------
// State management (symlinks + journal)
// ---------------------------------------------------------------------------

func atomicSymlink(linkPath, target string) error {
	tmpLink := linkPath + ".tmp"
	os.Remove(tmpLink)
	if err := os.Symlink(target, tmpLink); err != nil {
		return err
	}
	return os.Rename(tmpLink, linkPath)
}

func (o *orchestrator) recoverState() {
	// Read live symlink.
	liveLink := filepath.Join(o.dataDir, "live")
	target, err := os.Readlink(liveLink)
	if err != nil {
		return
	}

	slotDir := filepath.Join(o.dataDir, target)
	if _, err := os.Stat(slotDir); err != nil {
		os.Remove(liveLink)
		return
	}

	commit := o.getWorktreeCommit(slotDir)
	if commit == "" {
		return
	}

	appPort, err := findFreePort()
	if err != nil {
		return
	}
	intPort, err := findFreePort()
	if err != nil {
		return
	}

	s, err := o.startProcess(slotDir, commit, appPort, intPort)
	if err != nil {
		fmt.Printf("warning: failed to restart live slot: %v\n", err)
		return
	}

	if o.healthCheck(s) {
		s.name = target
		o.liveSlot = s
		o.appProxy.setTarget(appPort)
		o.intProxy.setTarget(intPort)
		fmt.Printf("recovered live slot: %s (%s)\n", target, shortHash(commit))
	} else {
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}

	// Read prev symlink.
	prevLink := filepath.Join(o.dataDir, "prev")
	prevTarget, err := os.Readlink(prevLink)
	if err != nil {
		return
	}
	prevDir := filepath.Join(o.dataDir, prevTarget)
	if _, err := os.Stat(prevDir); err != nil {
		os.Remove(prevLink)
		return
	}
	prevCommit := o.getWorktreeCommit(prevDir)
	if prevCommit != "" {
		o.prevSlot = &slot{
			name:   prevTarget,
			commit: prevCommit,
			dir:    prevDir,
			done:   make(chan struct{}),
		}
		close(o.prevSlot.done) // Not running.
	}
}

func (o *orchestrator) getWorktreeCommit(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (o *orchestrator) appendJournal(action, commit, slotDir, prevCommit string) {
	entry := map[string]string{
		"time":        time.Now().Format(time.RFC3339),
		"action":      action,
		"commit":      commit,
		"slot_dir":    slotDir,
		"prev_commit": prevCommit,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	path := filepath.Join(o.dataDir, "journal.ndjson")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func loadEnvFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var env []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "=") {
			env = append(env, line)
		}
	}
	return env, scanner.Err()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func gitHeadCommit(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ---------------------------------------------------------------------------
// Subcommand: init
// ---------------------------------------------------------------------------

func cmdInit() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cfg := config{
		Port:            3000,
		InternalPort:    3000,
		HealthEndpoint:  "/healthz",
		HealthTimeoutMs: 10000,
		DrainTimeoutMs:  5000,
		APIPort:         9100,
	}

	switch {
	case fileExists(filepath.Join(cwd, "bun.lock")):
		cfg.SetupCommand = "bun install --frozen-lockfile"
		cfg.StartCommand = readStartScript(cwd, "bun")
	case fileExists(filepath.Join(cwd, "package-lock.json")):
		cfg.SetupCommand = "npm ci"
		cfg.StartCommand = readStartScript(cwd, "node")
	case fileExists(filepath.Join(cwd, "uv.lock")):
		cfg.SetupCommand = "uv sync --frozen"
		cfg.StartCommand = "uv run python app.py"
	case fileExists(filepath.Join(cwd, "Gemfile.lock")):
		cfg.SetupCommand = "bundle install"
		cfg.StartCommand = "bundle exec ruby app.rb"
	}

	if fileExists(filepath.Join(cwd, ".env")) {
		cfg.EnvFile = ".env"
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	cfgPath := filepath.Join(cwd, "slot-machine.json")
	if err := os.WriteFile(cfgPath, append(data, '\n'), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", cfgPath, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", cfgPath)

	gitignorePath := filepath.Join(cwd, ".gitignore")
	if !gitignoreContains(gitignorePath, ".slot-machine") {
		f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			if info, _ := f.Stat(); info.Size() > 0 {
				buf := make([]byte, 1)
				if fRead, err := os.Open(gitignorePath); err == nil {
					fRead.Seek(-1, io.SeekEnd)
					fRead.Read(buf)
					fRead.Close()
					if buf[0] != '\n' {
						f.WriteString("\n")
					}
				}
			}
			f.WriteString(".slot-machine\n")
			f.Close()
			fmt.Println("added .slot-machine to .gitignore")
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readStartScript(dir, runtime string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return runtime + " index.js"
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
		Main    string            `json:"main"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return runtime + " index.js"
	}
	if s, ok := pkg.Scripts["start"]; ok {
		return s
	}
	if pkg.Main != "" {
		return runtime + " " + pkg.Main
	}
	return runtime + " index.js"
}

func gitignoreContains(path, entry string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == entry {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Subcommand: start
// ---------------------------------------------------------------------------

func cmdStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	configPath := fs.String("config", "", "path to slot-machine.json (default: ./slot-machine.json)")
	repoDir := fs.String("repo", "", "path to git repo (default: .)")
	dataDir := fs.String("data", "", "path to data directory (default: ./.slot-machine)")
	port := fs.Int("port", 0, "API listen port (default: config api_port or 9100)")
	_ = fs.Bool("no-proxy", false, "ignored (kept for backward compatibility)")
	fs.Parse(args)

	cwd, _ := os.Getwd()

	if *configPath == "" {
		*configPath = filepath.Join(cwd, "slot-machine.json")
	}
	if *repoDir == "" {
		*repoDir = cwd
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(cwd, ".slot-machine")
	}

	cfgData, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot read %s\n", *configPath)
		fmt.Fprintln(os.Stderr, "run 'slot-machine init' to create it")
		os.Exit(1)
	}
	var cfg config
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing config: %v\n", err)
		os.Exit(1)
	}

	apiPort := 9100
	if cfg.APIPort != 0 {
		apiPort = cfg.APIPort
	}
	if *port != 0 {
		apiPort = *port
	}

	absRepo, err := filepath.Abs(*repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error resolving repo path: %v\n", err)
		os.Exit(1)
	}

	os.MkdirAll(*dataDir, 0755)

	appProxyAddr := ""
	if cfg.Port != 0 {
		appProxyAddr = fmt.Sprintf(":%d", cfg.Port)
	}
	intProxyAddr := ""
	if cfg.InternalPort != 0 && cfg.InternalPort != cfg.Port {
		intProxyAddr = fmt.Sprintf(":%d", cfg.InternalPort)
	}

	o := &orchestrator{
		cfg:      cfg,
		repoDir:  absRepo,
		dataDir:  *dataDir,
		appProxy: newDynamicProxy(appProxyAddr),
		intProxy: newDynamicProxy(intProxyAddr),
	}

	// Recover state from symlinks.
	o.recoverState()

	// API server.
	apiAddr := fmt.Sprintf(":%d", apiPort)
	apiSrv := &http.Server{Addr: apiAddr, Handler: o}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		fmt.Println("\nshutting down...")
		o.drainAll()
		o.appProxy.shutdown()
		o.intProxy.shutdown()
		apiSrv.Shutdown(context.Background())
	}()

	fmt.Printf("slot-machine listening on %s\n", apiAddr)
	if err := apiSrv.ListenAndServe(); err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Subcommand: deploy
// ---------------------------------------------------------------------------

func cmdDeploy(args []string) {
	commit := ""
	if len(args) > 0 {
		commit = args[0]
	}

	if commit == "" {
		cwd, _ := os.Getwd()
		c, err := gitHeadCommit(cwd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot determine HEAD commit: %v\n", err)
			os.Exit(1)
		}
		commit = c
	}

	port := readAPIPort()
	body, _ := json.Marshal(map[string]string{"commit": commit})
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/deploy", port),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot reach slot-machine daemon: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var dr deployResponse
	json.NewDecoder(resp.Body).Decode(&dr)

	if dr.Success {
		fmt.Printf("deployed %s to %s\n", shortHash(dr.Commit), dr.Slot)
	} else {
		fmt.Fprintf(os.Stderr, "deploy failed: %s\n", dr.Error)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Subcommand: rollback
// ---------------------------------------------------------------------------

func cmdRollback() {
	port := readAPIPort()
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/rollback", port),
		"application/json",
		nil,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot reach slot-machine daemon: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var rr rollbackResponse
	json.NewDecoder(resp.Body).Decode(&rr)

	if rr.Success {
		fmt.Printf("rolled back to %s (%s)\n", shortHash(rr.Commit), rr.Slot)
	} else {
		fmt.Fprintf(os.Stderr, "rollback failed: %s\n", rr.Error)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Subcommand: status
// ---------------------------------------------------------------------------

func cmdStatus() {
	port := readAPIPort()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/status", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot reach slot-machine daemon: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var sr statusResponse
	json.NewDecoder(resp.Body).Decode(&sr)

	healthy := "no"
	if sr.Healthy {
		healthy = "yes"
	}

	fmt.Printf("live:     %s  %s  healthy=%s\n", sr.LiveSlot, sr.LiveCommit, healthy)
	if sr.PreviousSlot != "" {
		fmt.Printf("previous: %s  %s\n", sr.PreviousSlot, sr.PreviousCommit)
	}
	if sr.StagingDir != "" {
		fmt.Printf("staging:  %s\n", sr.StagingDir)
	}
	if sr.LastDeployTime != "" {
		fmt.Printf("last deploy: %s\n", sr.LastDeployTime)
	}
}

func shortHash(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func readAPIPort() int {
	cwd, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(cwd, "slot-machine.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot read slot-machine.json in current directory")
		os.Exit(1)
	}
	var cfg config
	json.Unmarshal(data, &cfg)
	if cfg.APIPort != 0 {
		return cfg.APIPort
	}
	return 9100
}

// ---------------------------------------------------------------------------
// Main — subcommand routing
// ---------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: slot-machine <command> [args]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "commands:")
		fmt.Fprintln(os.Stderr, "  init       scaffold slot-machine.json")
		fmt.Fprintln(os.Stderr, "  start      start the daemon")
		fmt.Fprintln(os.Stderr, "  deploy     deploy a commit")
		fmt.Fprintln(os.Stderr, "  rollback   rollback to previous")
		fmt.Fprintln(os.Stderr, "  status     show current status")
		fmt.Fprintln(os.Stderr, "  version    print version info")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		cmdInit()
	case "start":
		cmdStart(os.Args[2:])
	case "deploy":
		cmdDeploy(os.Args[2:])
	case "rollback":
		cmdRollback()
	case "status":
		cmdStatus()
	case "version":
		fmt.Printf("slot-machine (go) spec/%s\n", specVersion)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
