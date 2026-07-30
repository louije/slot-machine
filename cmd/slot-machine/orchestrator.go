package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type orchestrator struct {
	cfg        config
	repoDir    string
	dataDir    string
	authSecret string // hex HMAC secret, passed to the app as SLOT_MACHINE_AUTH_SECRET

	mu         sync.Mutex
	deploying  bool
	liveSlot   *slot
	prevSlot   *slot
	lastDeploy time.Time

	appProxy *dynamicProxy // proxies config.Port → live slot's appPort
	intProxy *dynamicProxy // proxies config.InternalPort → live slot's intPort
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
	// Stage names where a failed deploy stopped: resolve, gate, prepare, setup,
	// verify, boot, probe or promote. Empty on success.
	Stage string `json:"stage,omitempty"`
	Error string `json:"error,omitempty"`
}

func (o *orchestrator) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Commit == "" {
		writeJSON(w, 400, deployResponse{Stage: "resolve", Error: "missing commit"})
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
	Stage   string `json:"stage,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (o *orchestrator) handleRollback(w http.ResponseWriter, r *http.Request) {
	resp, code := o.doRollback()
	writeJSON(w, code, resp)
}

// --- GET /status ---

type statusResponse struct {
	LiveSlot       string            `json:"live_slot"`
	LiveCommit     string            `json:"live_commit"`
	PreviousSlot   string            `json:"previous_slot"`
	PreviousCommit string            `json:"previous_commit"`
	StagingDir     string            `json:"staging_dir"`
	MachineDir     string            `json:"machine_dir"`
	MachineBranch  string            `json:"machine_branch"`
	MachineCommit  string            `json:"machine_commit,omitempty"`
	Divergence     *branchDivergence `json:"machine_vs_human,omitempty"`
	LastDeployTime string            `json:"last_deploy_time"`
	Healthy        bool              `json:"healthy"`
	Deploying      bool              `json:"deploying"`
	// ProxyListening reports whether the public port is actually bound. A
	// daemon whose proxy failed to listen used to report itself perfectly
	// healthy while nothing could reach the app.
	ProxyListening bool `json:"proxy_listening"`
}

func (o *orchestrator) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Read git state before taking the lock: shelling out under o.mu would block
	// deploys for the duration.
	machineCommit, _ := git(o.machineDir(), "rev-parse", "HEAD")
	divergence := o.machineDivergence()

	o.mu.Lock()
	defer o.mu.Unlock()

	resp := statusResponse{
		StagingDir:     stagingSlotName,
		MachineDir:     machineSlotName,
		MachineBranch:  o.cfg.MachineBranch,
		MachineCommit:  machineCommit,
		Divergence:     divergence,
		Deploying:      o.deploying,
		ProxyListening: o.appProxy.listening(),
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
// Deploy
// ---------------------------------------------------------------------------

// doDeploy runs the promotion pipeline for one commit.
//
//	resolve → gate → prepare → setup → verify → boot → probe → promote
//
// The two ordering decisions worth stating: the static gate runs before setup,
// because setup_command executes code from the candidate commit (an npm/bun
// postinstall hook runs before anything else we do), and a secret scan that runs
// after that has already lost. Verify runs after setup, because a test suite
// needs its dependencies.
//
// Every failure path leaves the live slot serving and returns the stage it
// stopped at, so "deploy failed:" is never followed by an empty string.
func (o *orchestrator) doDeploy(commit string) (deployResponse, int) {
	o.mu.Lock()
	if o.deploying {
		o.mu.Unlock()
		return deployResponse{Stage: "resolve", Error: "a deploy is already in progress"}, 409
	}
	o.deploying = true
	oldLive := o.liveSlot
	oldPrev := o.prevSlot
	liveCommit := ""
	if oldLive != nil {
		liveCommit = oldLive.commit
	}
	o.mu.Unlock()

	defer func() {
		o.mu.Lock()
		o.deploying = false
		o.mu.Unlock()
	}()

	fail := func(stage, msg string, code int) (deployResponse, int) {
		logf("deploy %s failed at %s: %s", shortHash(commit), stage, msg)
		return deployResponse{Stage: stage, Error: msg, PreviousCommit: liveCommit}, code
	}

	// 1. Resolve — reject anything we cannot name before doing work.
	full, err := git(o.repoDir, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return fail("resolve", fmt.Sprintf("unknown commit %q", commit), 400)
	}
	commit = full

	// 2. Gate — static checks against what production is about to experience.
	if err := o.runGate(liveCommit, commit); err != nil {
		var stage = "gate"
		if ge, ok := err.(*gateError); ok {
			return fail(stage, ge.Check+": "+ge.Detail, 422)
		}
		return fail(stage, err.Error(), 422)
	}

	stagingDir := o.stagingDir()

	// 3. Prepare.
	if err := o.prepareSlot(stagingDir, commit); err != nil {
		return fail("prepare", err.Error(), 500)
	}
	o.applySharedDirs(stagingDir)

	appPort, err := findFreePort()
	if err != nil {
		return fail("prepare", "allocating app port: "+err.Error(), 500)
	}
	intPort, err := findFreePort()
	if err != nil {
		return fail("prepare", "allocating internal port: "+err.Error(), 500)
	}

	// 4. Setup.
	if o.cfg.SetupCommand != "" {
		if err := o.runSetup(stagingDir, appPort, intPort); err != nil {
			return fail("setup", "setup_command: "+err.Error(), 500)
		}
	}

	// 5. Verify — the app's own tests, against the tree about to go live.
	if err := o.runPreDeploy(stagingDir, appPort, intPort); err != nil {
		return fail("verify", err.Error(), 422)
	}

	// 6. Boot.
	newSlot, err := o.startProcess(stagingDir, commit, appPort, intPort)
	if err != nil {
		return fail("boot", "start_command: "+err.Error(), 500)
	}

	// 7. Probe — the old live is still serving through the proxy throughout.
	if !o.healthCheck(newSlot) {
		o.kill(newSlot)
		return fail("probe", fmt.Sprintf(
			"the new process did not pass %s within %dms (see %s.log); the live slot is untouched",
			o.cfg.HealthEndpoint, o.cfg.HealthTimeoutMs, stagingSlotName), 422)
	}
	if err := o.checkSchemaCompatible(newSlot); err != nil {
		o.kill(newSlot)
		return fail("probe", err.Error(), 422)
	}

	// 8. Promote.
	slotName := fmt.Sprintf("slot-%s", shortHash(commit))
	slotDir := filepath.Join(o.dataDir, slotName)

	// GC the old prev first, so re-deploying the same commit cannot collide.
	if oldPrev != nil {
		o.drain(oldPrev)
		o.removeWorktree(oldPrev.dir)
	}

	// If the target name already exists (re-deploying the same commit), move it
	// aside first. The old process keeps running: Unix does not invalidate open
	// file handles on rename.
	drainingDir := ""
	if _, err := os.Stat(slotDir); err == nil {
		drainingDir = slotDir + ".draining"
		os.RemoveAll(drainingDir)
		os.Rename(slotDir, drainingDir)
	}
	if err := o.promoteStaging(stagingDir, slotDir); err != nil {
		// Non-fatal: the process is running from stagingDir, so use that path.
		logf("warning: could not rename the staging slot: %v", err)
		slotDir = stagingDir
		slotName = stagingSlotName
	}
	newSlot.dir = slotDir
	newSlot.name = slotName

	o.appProxy.setTarget(appPort)
	o.intProxy.setTarget(intPort)

	// Update state before draining, so the crash callback cannot clear the proxy
	// target we just set.
	o.mu.Lock()
	o.prevSlot = oldLive
	o.liveSlot = newSlot
	o.lastDeploy = time.Now()
	o.mu.Unlock()

	if oldLive != nil {
		o.drain(oldLive)
	}
	if drainingDir != "" {
		os.RemoveAll(drainingDir)
	}

	atomicSymlink(filepath.Join(o.dataDir, "live"), slotName)
	if oldLive != nil {
		atomicSymlink(filepath.Join(o.dataDir, "prev"), oldLive.name)
	}

	o.createStaging(slotDir, commit)
	o.appendJournal("deploy", commit, slotName, liveCommit)

	return deployResponse{
		Success:        true,
		Slot:           slotName,
		Commit:         commit,
		PreviousCommit: liveCommit,
	}, 200
}

func (o *orchestrator) kill(s *slot) {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	<-s.done
}

// runPreDeploy runs the app's own checks in the staging tree. A non-zero exit
// blocks the promotion — this is the "is it safe to make live" step that a
// health endpoint alone cannot answer.
func (o *orchestrator) runPreDeploy(dir string, appPort, intPort int) error {
	if o.cfg.PreDeployCommand == "" {
		return nil
	}

	timeout := time.Duration(o.cfg.PreDeployTimeoutMs) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", o.cfg.PreDeployCommand)
	cmd.Dir = dir
	cmd.Env = o.buildEnv(appPort, intPort)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second

	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("pre_deploy_command timed out after %s", timeout)
	}
	if err != nil {
		return fmt.Errorf("pre_deploy_command failed: %v\n%s", err, tailString(string(out), 4000))
	}
	return nil
}

func tailString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

// doRollback returns to the previous slot.
//
// It does not run the gate: the target already passed it and already served
// traffic. It does run the schema check, because the database may have moved
// forward since — that is the asymmetry that makes rollback dangerous, and it is
// the one migration case the orchestrator can actually rule on.
func (o *orchestrator) doRollback() (rollbackResponse, int) {
	o.mu.Lock()
	if o.deploying {
		o.mu.Unlock()
		return rollbackResponse{Stage: "resolve", Error: "a deploy is already in progress"}, 409
	}
	if o.prevSlot == nil {
		o.mu.Unlock()
		return rollbackResponse{Stage: "resolve", Error: "no previous slot to roll back to"}, 400
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

	fail := func(stage, msg string, code int) (rollbackResponse, int) {
		logf("rollback failed at %s: %s", stage, msg)
		return rollbackResponse{Stage: stage, Error: msg}, code
	}

	appPort, err := findFreePort()
	if err != nil {
		return fail("prepare", "allocating app port: "+err.Error(), 500)
	}
	intPort, err := findFreePort()
	if err != nil {
		return fail("prepare", "allocating internal port: "+err.Error(), 500)
	}

	newSlot, err := o.startProcess(prev.dir, prev.commit, appPort, intPort)
	if err != nil {
		return fail("boot", "start_command: "+err.Error(), 500)
	}

	if !o.healthCheck(newSlot) {
		o.kill(newSlot)
		return fail("probe", "the previous version did not pass its health check", 422)
	}
	if err := o.checkSchemaCompatible(newSlot); err != nil {
		o.kill(newSlot)
		return fail("probe", err.Error(), 422)
	}

	o.appProxy.setTarget(appPort)
	o.intProxy.setTarget(intPort)

	newSlot.name = prev.name
	o.mu.Lock()
	o.liveSlot = newSlot
	o.prevSlot = oldLive
	o.lastDeploy = time.Now()
	o.mu.Unlock()

	if oldLive != nil {
		o.drain(oldLive)
	}

	atomicSymlink(filepath.Join(o.dataDir, "live"), prev.name)
	if oldLive != nil {
		atomicSymlink(filepath.Join(o.dataDir, "prev"), oldLive.name)
	}

	o.createStaging(prev.dir, prev.commit)
	o.appendJournal("rollback", prev.commit, prev.name, "")

	return rollbackResponse{
		Success: true,
		Slot:    prev.name,
		Commit:  prev.commit,
	}, 200
}
