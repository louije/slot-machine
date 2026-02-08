package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

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
// Deploy logic
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
	// If the target already exists (re-deploy same commit), move it aside first.
	// The old process keeps running fine — Unix doesn't invalidate open file handles on rename.
	drainingDir := ""
	if _, err := os.Stat(slotDir); err == nil {
		drainingDir = slotDir + ".draining"
		os.RemoveAll(drainingDir)
		os.Rename(slotDir, drainingDir)
	}
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
	if drainingDir != "" {
		os.RemoveAll(drainingDir)
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
