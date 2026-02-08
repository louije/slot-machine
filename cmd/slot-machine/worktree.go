package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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
			o.applySharedDirs(dstDir)
			return
		}
		// Clone metadata repair failed — remove and fall back.
		os.RemoveAll(dstDir)
	}

	// Fallback: fresh worktree.
	exec.Command("git", "-C", o.repoDir, "worktree", "prune").Run()
	exec.Command("git", "-C", o.repoDir, "worktree", "add", "--detach", dstDir, commit).Run()

	o.applySharedDirs(dstDir)
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

	// Rebuild the index from HEAD so git status is clean.
	cmd := exec.Command("git", "reset", "--quiet")
	cmd.Dir = wtDir
	cmd.Run()

	return nil
}

// applySharedDirs replaces configured shared_dirs in slotDir with symlinks
// to the canonical location in the source repo. This ensures all slots and
// the staging dir share the same data — no duplicate state.
func (o *orchestrator) applySharedDirs(slotDir string) {
	if len(o.cfg.SharedDirs) == 0 {
		return
	}

	for _, name := range o.cfg.SharedDirs {
		name = filepath.Clean(name)
		if name == "." || name == ".." || filepath.IsAbs(name) {
			continue
		}

		target := filepath.Join(o.repoDir, name)
		slotPath := filepath.Join(slotDir, name)

		// Ensure the canonical location exists in the repo.
		os.MkdirAll(target, 0755)

		// Remove whatever is at slotPath (real dir from CoW clone, old symlink, etc).
		os.RemoveAll(slotPath)

		// Ensure parent dirs exist in slot.
		os.MkdirAll(filepath.Dir(slotPath), 0755)

		// Symlink slot → repo.
		absTarget, _ := filepath.Abs(target)
		os.Symlink(absTarget, slotPath)
	}
}

func (o *orchestrator) removeWorktree(dir string) {
	cmd := exec.Command("git", "-C", o.repoDir, "worktree", "remove", "--force", dir)
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dir)
		exec.Command("git", "-C", o.repoDir, "worktree", "prune").Run()
	}
}
