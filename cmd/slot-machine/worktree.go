package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Slot layout under dataDir:
//
//	machine        worktree on the machine branch — the agent's, permanent
//	slot-staging   detached at the candidate commit — deploy preparation
//	slot-<hash>    detached — live, and the previous live
//	live, prev     symlinks to the above
//
// The machine slot deliberately does not carry the slot- prefix. It is not a
// deploy slot and must never be treated as one: it is not renamed, promoted,
// garbage-collected, or force-checked-out. Keeping it outside the naming
// convention means code that reasons about deploy slots cannot pick it up by
// accident.
const (
	stagingSlotName = "slot-staging"
	machineSlotName = "machine"
)

func (o *orchestrator) stagingDir() string {
	return filepath.Join(o.dataDir, stagingSlotName)
}

func (o *orchestrator) machineDir() string {
	return filepath.Join(o.dataDir, machineSlotName)
}

// ---------------------------------------------------------------------------
// The machine slot
// ---------------------------------------------------------------------------

// ensureMachineSlot creates the agent's worktree if it is missing and makes sure
// it is on the machine branch.
//
// The branch matters more than it looks. The agent used to work in a detached
// worktree, so its commits were referenced by nothing but that worktree's HEAD:
// the next deploy moved HEAD elsewhere and the work became unreachable, one
// `git gc` away from gone. On a real branch the commits have a ref, and the
// design's branch model (docs/design.md §4) becomes implementable — you cannot
// merge main into a detached HEAD and expect it to mean anything.
func (o *orchestrator) ensureMachineSlot() error {
	dir := o.machineDir()
	branch := o.cfg.MachineBranch

	// Create the branch if this repo has never had one.
	if !gitOK(o.repoDir, "rev-parse", "--verify", "--quiet", branch) {
		base := o.cfg.HumanBranch
		if !gitOK(o.repoDir, "rev-parse", "--verify", "--quiet", base) {
			base = "HEAD"
		}
		if _, err := git(o.repoDir, "branch", branch, base); err != nil {
			return fmt.Errorf("creating %s branch: %w", branch, err)
		}
		logf("created branch %s from %s", branch, base)
	}

	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		// Already a worktree. Put it back on the branch if something moved it,
		// but never with --force: uncommitted agent work lives here.
		current, err := git(dir, "rev-parse", "--abbrev-ref", "HEAD")
		if err == nil && current == branch {
			return nil
		}
		if _, err := git(dir, "checkout", branch); err != nil {
			return fmt.Errorf("machine slot is not on %s and could not be moved back "+
				"(it may have uncommitted changes): %w", branch, err)
		}
		return nil
	}

	os.RemoveAll(dir)
	exec.Command("git", "-C", o.repoDir, "worktree", "prune").Run()

	if _, err := git(o.repoDir, "worktree", "add", dir, branch); err != nil {
		// git refuses to check out a branch that is already checked out in
		// another worktree. That refusal is the one-agent-worktree invariant
		// enforcing itself, so pass it through rather than working around it.
		return fmt.Errorf("creating machine slot: %w", err)
	}

	o.applySharedDirs(dir)

	if o.cfg.SetupCommand != "" {
		logf("running setup in the machine slot...")
		if err := o.runSetup(dir, 0, 0); err != nil {
			// Not fatal: the agent has a shell and can fix its own workspace.
			logf("warning: setup in the machine slot failed: %v", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Deploy slots
// ---------------------------------------------------------------------------

func (o *orchestrator) prepareSlot(slotDir, commit string) error {
	if _, err := os.Stat(filepath.Join(slotDir, ".git")); err == nil {
		if _, err := git(slotDir, "checkout", "--force", "--detach", commit); err != nil {
			return fmt.Errorf("checkout in staging: %w", err)
		}
		return nil
	}

	os.RemoveAll(slotDir)
	exec.Command("git", "-C", o.repoDir, "worktree", "prune").Run()

	if _, err := git(o.repoDir, "worktree", "add", "--detach", slotDir, commit); err != nil {
		return fmt.Errorf("creating staging slot: %w", err)
	}
	return nil
}

// promoteStaging renames slot-staging → slot-<hash> and repairs git worktree
// metadata.
func (o *orchestrator) promoteStaging(oldDir, newDir string) error {
	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}

	gitFile := filepath.Join(newDir, ".git")
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return err
	}

	metaDir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))

	absNewGit, _ := filepath.Abs(filepath.Join(newDir, ".git"))
	os.WriteFile(filepath.Join(metaDir, "gitdir"), []byte(absNewGit+"\n"), 0644)

	newName := filepath.Base(newDir)
	newMetaDir := filepath.Join(filepath.Dir(metaDir), newName)
	if metaDir != newMetaDir {
		os.Rename(metaDir, newMetaDir)
		absNewMeta, _ := filepath.Abs(newMetaDir)
		os.WriteFile(gitFile, []byte("gitdir: "+absNewMeta+"\n"), 0644)
	}

	return nil
}

// cloneTree copies src to dst, preferring a copy-on-write clone so that
// node_modules and build artifacts carry over without being re-created.
//
// The flag is platform-specific and getting it wrong is silent: `cp -c` is an
// APFS clone on macOS, and on GNU coreutils `-c` is not an option at all, so
// every Linux deploy fell straight through to the slow path and re-ran
// setup_command against an empty tree. Each candidate is tried in order and the
// caller falls back to a fresh worktree if all of them fail.
func cloneTree(src, dst string) error {
	var attempts [][]string
	if runtime.GOOS == "darwin" {
		attempts = append(attempts, []string{"-c", "-R"})
	} else {
		attempts = append(attempts,
			[]string{"--reflink=auto", "-a"}, // btrfs/xfs CoW, plain copy otherwise
			[]string{"-al"},                  // hardlink farm: cheap and near-instant
		)
	}
	attempts = append(attempts, []string{"-R"})

	var lastErr error
	for _, flags := range attempts {
		// The destination must not exist: `cp -R src dst` copies *into* dst when
		// dst is a directory, which nests the tree instead of replacing it.
		os.RemoveAll(dst)
		args := append(append([]string{}, flags...), src, dst)
		if err := exec.Command("cp", args...).Run(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	os.RemoveAll(dst)
	return lastErr
}

// createStaging rebuilds slot-staging as a copy of the promoted slot.
func (o *orchestrator) createStaging(srcDir, commit string) {
	dstDir := o.stagingDir()

	if err := cloneTree(srcDir, dstDir); err == nil {
		if o.fixClonedWorktree(dstDir, commit) == nil {
			o.applySharedDirs(dstDir)
			return
		}
		os.RemoveAll(dstDir)
	}

	// Fallback: a fresh worktree. Correct, just slower — setup_command will
	// have to rebuild whatever the clone would have carried over.
	exec.Command("git", "-C", o.repoDir, "worktree", "prune").Run()
	if _, err := git(o.repoDir, "worktree", "add", "--detach", dstDir, commit); err != nil {
		logf("warning: could not recreate the staging slot: %v", err)
		return
	}
	o.applySharedDirs(dstDir)
}

// fixClonedWorktree sets up git worktree metadata for a cloned directory.
func (o *orchestrator) fixClonedWorktree(wtDir, commit string) error {
	gitFile := filepath.Join(wtDir, ".git")
	os.Remove(gitFile)

	repoGitDir := filepath.Join(o.repoDir, ".git")
	info, err := os.Stat(repoGitDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("repo .git is not a directory")
	}

	metaDir := filepath.Join(repoGitDir, "worktrees", stagingSlotName)

	os.RemoveAll(metaDir)
	if err := os.MkdirAll(metaDir, 0755); err != nil {
		return err
	}

	absWtDir, _ := filepath.Abs(wtDir)
	absGitFile := filepath.Join(absWtDir, ".git")
	absMetaDir, _ := filepath.Abs(metaDir)

	os.WriteFile(filepath.Join(metaDir, "HEAD"), []byte(commit+"\n"), 0644)
	os.WriteFile(filepath.Join(metaDir, "commondir"), []byte("../..\n"), 0644)
	os.WriteFile(filepath.Join(metaDir, "gitdir"), []byte(absGitFile+"\n"), 0644)
	os.WriteFile(gitFile, []byte("gitdir: "+absMetaDir+"\n"), 0644)

	// Rebuild the index from HEAD so git status is clean.
	cmd := exec.Command("git", "reset", "--quiet")
	cmd.Dir = wtDir
	cmd.Run()

	return nil
}

// applySharedDirs replaces configured shared_dirs in slotDir with symlinks to
// the canonical location in the source repo, so every slot — including the
// machine slot — sees the same live data.
func (o *orchestrator) applySharedDirs(slotDir string) {
	if len(o.cfg.SharedDirs) == 0 {
		return
	}

	for _, name := range o.cfg.SharedDirs {
		name = filepath.Clean(name)
		if name == "." || name == ".." || filepath.IsAbs(name) || strings.HasPrefix(name, "../") {
			logf("warning: ignoring shared_dir %q: must be a relative path inside the repo", name)
			continue
		}

		target := filepath.Join(o.repoDir, name)
		slotPath := filepath.Join(slotDir, name)

		// Seed the canonical location from the slot's checkout on first deploy,
		// rather than creating an empty directory and losing the contents.
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			if info, err := os.Lstat(slotPath); err == nil && info.IsDir() {
				os.MkdirAll(filepath.Dir(target), 0755)
				os.Rename(slotPath, target)
			} else {
				os.MkdirAll(target, 0755)
			}
		}

		os.RemoveAll(slotPath)
		os.MkdirAll(filepath.Dir(slotPath), 0755)

		absTarget, _ := filepath.Abs(target)
		os.Symlink(absTarget, slotPath)
	}
}

func (o *orchestrator) removeWorktree(dir string) {
	// Guard: the machine slot is not a deploy slot and must never be collected.
	if filepath.Clean(dir) == filepath.Clean(o.machineDir()) {
		logf("refusing to remove the machine slot")
		return
	}
	cmd := exec.Command("git", "-C", o.repoDir, "worktree", "remove", "--force", dir)
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dir)
		exec.Command("git", "-C", o.repoDir, "worktree", "prune").Run()
	}
}

// warnTrackedSharedDirs checks the shared_dirs configuration against git.
//
// A shared dir is replaced by a symlink in every slot. If the same path is also
// tracked in the repository, the two mechanisms fight: a deploy's
// `git checkout --force` wants real files there, and applySharedDirs wants a
// symlink. The result depends on ordering, which is the worst kind of bug to
// diagnose. Nothing here can safely fix it — dropping tracked files or ignoring
// the config would both lose data — so say so clearly at startup instead.
func (o *orchestrator) warnTrackedSharedDirs() {
	for _, name := range o.cfg.SharedDirs {
		name = filepath.Clean(name)
		if name == "." || name == ".." || filepath.IsAbs(name) {
			continue
		}
		out, err := git(o.repoDir, "ls-files", "--", name)
		if err != nil || strings.TrimSpace(out) == "" {
			continue
		}
		n := len(splitLines(out))
		logf("warning: shared_dir %q has %d file(s) tracked in git. "+
			"Shared dirs are replaced by symlinks in every slot, so a tracked path "+
			"will fight the checkout. Add %s to .gitignore and `git rm -r --cached %s`.",
			name, n, name, name)
	}
}
