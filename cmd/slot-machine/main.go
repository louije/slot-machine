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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

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
	commit string
	dir    string
	cmd    *exec.Cmd
	done   chan struct{}
	alive  bool
}

type orchestrator struct {
	cfg     config
	repoDir string
	dataDir string
	noProxy bool

	mu         sync.Mutex
	deploying  bool
	liveSlot   string
	prevSlot   string
	slots      map[string]*slot
	lastDeploy time.Time
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
	LastDeployTime string `json:"last_deploy_time"`
	Healthy        bool   `json:"healthy"`
}

func (o *orchestrator) handleStatus(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()

	resp := statusResponse{
		LiveSlot:     o.liveSlot,
		PreviousSlot: o.prevSlot,
	}

	if o.liveSlot != "" {
		s := o.slots[o.liveSlot]
		resp.LiveCommit = s.commit
		resp.Healthy = s.alive
	}
	if o.prevSlot != "" {
		resp.PreviousCommit = o.slots[o.prevSlot].commit
	}
	if !o.lastDeploy.IsZero() {
		resp.LastDeployTime = o.lastDeploy.Format(time.RFC3339)
	}

	writeJSON(w, 200, resp)
}

// ---------------------------------------------------------------------------
// Deploy logic
// ---------------------------------------------------------------------------

func (o *orchestrator) doDeploy(commit string) (deployResponse, int) {
	o.mu.Lock()
	if o.deploying {
		o.mu.Unlock()
		return deployResponse{Error: "deploy in progress"}, 409
	}
	o.deploying = true
	liveSlotName := o.liveSlot
	var liveSlotObj *slot
	if liveSlotName != "" {
		liveSlotObj = o.slots[liveSlotName]
	}
	o.mu.Unlock()

	defer func() {
		o.mu.Lock()
		o.deploying = false
		o.mu.Unlock()
	}()

	inactive := "a"
	if liveSlotName == "a" {
		inactive = "b"
	}

	slotDir := filepath.Join(o.dataDir, "slot-"+inactive)
	if err := o.prepareSlot(slotDir, commit); err != nil {
		return deployResponse{Error: err.Error()}, 500
	}

	if o.cfg.SetupCommand != "" {
		if err := o.runSetup(slotDir); err != nil {
			return deployResponse{Error: "setup: " + err.Error()}, 500
		}
	}

	if liveSlotObj != nil {
		o.drain(liveSlotObj)
	}

	newSlot, err := o.startProcess(slotDir, commit)
	if err != nil {
		return deployResponse{Error: "start: " + err.Error()}, 500
	}

	if o.healthCheck(newSlot) {
		prevCommit := ""
		o.mu.Lock()
		if liveSlotName != "" && o.slots[liveSlotName] != nil {
			prevCommit = o.slots[liveSlotName].commit
		}
		o.prevSlot = liveSlotName
		o.liveSlot = inactive
		o.slots[inactive] = newSlot
		o.lastDeploy = time.Now()
		o.mu.Unlock()

		return deployResponse{
			Success:        true,
			Slot:           inactive,
			Commit:         commit,
			PreviousCommit: prevCommit,
		}, 200
	}

	syscall.Kill(-newSlot.cmd.Process.Pid, syscall.SIGKILL)
	<-newSlot.done

	return deployResponse{}, 200
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
	if o.prevSlot == "" {
		o.mu.Unlock()
		return rollbackResponse{Error: "no previous slot"}, 400
	}
	o.deploying = true

	prevSlotName := o.prevSlot
	prevSlotObj := o.slots[prevSlotName]
	liveSlotName := o.liveSlot
	liveSlotObj := o.slots[liveSlotName]
	o.mu.Unlock()

	defer func() {
		o.mu.Lock()
		o.deploying = false
		o.mu.Unlock()
	}()

	if liveSlotObj != nil {
		o.drain(liveSlotObj)
	}

	newSlot, err := o.startProcess(prevSlotObj.dir, prevSlotObj.commit)
	if err != nil {
		return rollbackResponse{Error: "start: " + err.Error()}, 500
	}

	if o.healthCheck(newSlot) {
		o.mu.Lock()
		o.liveSlot = prevSlotName
		o.prevSlot = liveSlotName
		o.slots[prevSlotName] = newSlot
		o.lastDeploy = time.Now()
		o.mu.Unlock()

		return rollbackResponse{
			Success: true,
			Slot:    prevSlotName,
			Commit:  prevSlotObj.commit,
		}, 200
	}

	syscall.Kill(-newSlot.cmd.Process.Pid, syscall.SIGKILL)
	<-newSlot.done
	return rollbackResponse{Error: "health check failed"}, 500
}

// ---------------------------------------------------------------------------
// Process management
// ---------------------------------------------------------------------------

func (o *orchestrator) runSetup(dir string) error {
	cmd := exec.Command("/bin/sh", "-c", o.cfg.SetupCommand)
	cmd.Dir = dir
	cmd.Env = o.buildEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (o *orchestrator) buildEnv() []string {
	env := os.Environ()
	if o.cfg.EnvFile != "" {
		if extra, err := loadEnvFile(o.cfg.EnvFile); err == nil {
			env = append(env, extra...)
		}
	}
	env = append(env,
		fmt.Sprintf("PORT=%d", o.cfg.Port),
		fmt.Sprintf("INTERNAL_PORT=%d", o.cfg.InternalPort),
	)
	return env
}

func (o *orchestrator) startProcess(dir, commit string) (*slot, error) {
	cmd := exec.Command("/bin/sh", "-c", o.cfg.StartCommand)
	cmd.Dir = dir
	cmd.Env = o.buildEnv()
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
		commit: commit,
		dir:    dir,
		cmd:    cmd,
		done:   make(chan struct{}),
		alive:  true,
	}

	go func() {
		cmd.Wait()
		o.mu.Lock()
		s.alive = false
		o.mu.Unlock()
		close(s.done)
	}()

	return s, nil
}

// drainAll stops all managed processes. Called on daemon shutdown.
func (o *orchestrator) drainAll() {
	o.mu.Lock()
	slots := make([]*slot, 0, len(o.slots))
	for _, s := range o.slots {
		slots = append(slots, s)
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
	url := fmt.Sprintf("http://127.0.0.1:%d%s", o.cfg.InternalPort, o.cfg.HealthEndpoint)
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
// Git worktree
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

// gitHeadCommit returns the current HEAD commit hash for the repo at dir.
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

	// Detect project type and set setup/start commands.
	switch {
	case fileExists(filepath.Join(cwd, "bun.lock")):
		cfg.SetupCommand = "bun install --frozen-lockfile"
		cfg.StartCommand = readStartScript(cwd, "bun")
	case fileExists(filepath.Join(cwd, "package-lock.json")):
		cfg.SetupCommand = "npm ci"
		cfg.StartCommand = readStartScript(cwd, "node")
	case fileExists(filepath.Join(cwd, "requirements.txt")):
		cfg.SetupCommand = "pip install -r requirements.txt"
		cfg.StartCommand = "python app.py"
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

	// Append .slot-machine to .gitignore if not already there.
	gitignorePath := filepath.Join(cwd, ".gitignore")
	if !gitignoreContains(gitignorePath, ".slot-machine") {
		f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			// Add newline before if file doesn't end with one.
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
	noProxy := fs.Bool("no-proxy", false, "skip proxy configuration")
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

	// Load config.
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

	// Resolve API port: flag > config > 9100.
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

	// Create data dir.
	os.MkdirAll(*dataDir, 0755)

	o := &orchestrator{
		cfg:     cfg,
		repoDir: absRepo,
		dataDir: *dataDir,
		noProxy: *noProxy,
		slots:   make(map[string]*slot),
	}

	addr := fmt.Sprintf(":%d", apiPort)
	srv := &http.Server{Addr: addr, Handler: o}

	// Drain managed processes on SIGTERM/SIGINT.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		fmt.Println("\nshutting down...")
		o.drainAll()
		srv.Shutdown(context.Background())
	}()

	fmt.Printf("slot-machine listening on %s\n", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
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
		fmt.Printf("deployed %s to slot %s\n", shortHash(dr.Commit), dr.Slot)
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
		fmt.Printf("rolled back to %s (slot %s)\n", shortHash(rr.Commit), rr.Slot)
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

	fmt.Printf("live:     slot %s  %s  healthy=%s\n", sr.LiveSlot, sr.LiveCommit, healthy)
	if sr.PreviousSlot != "" {
		fmt.Printf("previous: slot %s  %s\n", sr.PreviousSlot, sr.PreviousCommit)
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

// readAPIPort reads slot-machine.json from cwd to get the api_port.
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
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
