package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ResolveClaude finds the claude binary. Search order:
// 1. SLOT_MACHINE_AGENT_BIN env var
// 2. <dataDir>/.local/bin/claude (managed install)
// 3. ~/.local/bin/claude (user install)
// 4. PATH lookup
func ResolveClaude(dataDir string) string {
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

// InstallClaude runs the official installer with HOME pointed at dataDir.
func InstallClaude(dataDir string) (string, error) {
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

// CLIVersion reports the version string of the agent binary, for the startup
// banner.
//
// Worth printing because slot-machine's invocation depends on flags whose
// availability varies by version — the system prompt travels via
// --append-system-prompt-file, and a CLI that did not support it would silently
// run the agent with no context at all. Recording the version in the log makes
// that answerable after the fact instead of guessable.
func CLIVersion(bin string) string {
	if bin == "" {
		bin = "claude"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
