package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// resolveClaude finds the claude binary. Search order:
// 1. SLOT_MACHINE_AGENT_BIN env var
// 2. <dataDir>/.local/bin/claude (managed install)
// 3. ~/.local/bin/claude (user install)
// 4. PATH lookup
func resolveClaude(dataDir string) string {
	if bin := os.Getenv("SLOT_MACHINE_AGENT_BIN"); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
	}

	managed := filepath.Join(dataDir, ".local", "bin", "claude")
	if _, err := os.Stat(managed); err == nil {
		return managed
	}

	if home, err := os.UserHomeDir(); err == nil {
		userBin := filepath.Join(home, ".local", "bin", "claude")
		if _, err := os.Stat(userBin); err == nil {
			return userBin
		}
	}

	if path, err := exec.LookPath("claude"); err == nil {
		return path
	}

	return ""
}

// installClaude runs the official installer with HOME pointed at dataDir.
func installClaude(dataDir string) (string, error) {
	fmt.Println("claude binary not found, installing...")

	cmd := exec.Command("bash", "-c",
		"curl -fsSL https://claude.ai/install.sh | bash")
	cmd.Env = append(os.Environ(), "HOME="+dataDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude install failed: %w", err)
	}

	bin := filepath.Join(dataDir, ".local", "bin", "claude")
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("claude binary not found after install at %s", bin)
	}

	fmt.Printf("claude installed at %s\n", bin)
	return bin, nil
}
