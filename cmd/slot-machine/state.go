package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

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
